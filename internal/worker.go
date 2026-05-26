package internal

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

var transientPatterns = []string{
	"socket connection was closed",
	"connection reset by peer",
	"ECONNRESET",
	"ETIMEDOUT",
	"ECONNREFUSED",
	"network timeout",
	"overloaded_error",
	"rate_limit",
	"529",
	"503",
	"502",
}

const maxTransientRetries = 5
const defaultMaxFixAttemptsPerRoot = 20

func isTransientError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, p := range transientPatterns {
		if strings.Contains(msg, strings.ToLower(p)) {
			return true
		}
	}
	return false
}

type WorkerStatus string

const (
	WorkerPending            WorkerStatus = "pending"
	WorkerRunning            WorkerStatus = "running"
	WorkerDone               WorkerStatus = "done"
	WorkerFailed             WorkerStatus = "failed"
	WorkerAwaitingValidation WorkerStatus = "awaiting_validation"
	WorkerValidating         WorkerStatus = "validating"
	WorkerRefining           WorkerStatus = "refining"
)

type FeatureWorker struct {
	Feature        Feature
	Status         WorkerStatus
	Lines          []string
	LastLine       string
	StartTime      time.Time
	EndTime        time.Time
	FailureContext string
	cmd            *exec.Cmd
}

type WorkerPool struct {
	projectDir          string
	missionDir          string
	workers             map[string]*FeatureWorker
	phases              map[int][]string
	logger              *MissionLogger
	eventCh             chan WorkerEvent
	mu                  sync.Mutex
	fileMu              sync.Mutex
	stopped             bool
	verbose             *bool
	retries             map[string]int
	maxRetries          int
	transientRetries    map[string]int
	validatorRetries    map[string]int
	maxValidatorRetries int
	refinementCount     map[string]int
	maxRefinements      int
	phaseRetries        map[int]int
	maxPhaseRetries     int
	criticDone          bool
	criticPassed        bool
	skipCritic          bool
	tainted             map[string]bool
	fixAttemptsByRoot   map[string]int
	maxFixAttempts      int
}

func NewWorkerPool(projectDir, missionDir string, features []Feature, logger *MissionLogger, verbose *bool) *WorkerPool {
	workers := make(map[string]*FeatureWorker)
	phases := make(map[int][]string)

	for _, f := range features {
		workers[f.ID] = &FeatureWorker{
			Feature: f,
			Status:  WorkerPending,
		}
		phases[f.Phase] = append(phases[f.Phase], f.ID)
	}

	return &WorkerPool{
		projectDir:          projectDir,
		missionDir:          missionDir,
		workers:             workers,
		phases:              phases,
		logger:              logger,
		eventCh:             make(chan WorkerEvent, 256),
		verbose:             verbose,
		retries:             make(map[string]int),
		maxRetries:          3,
		transientRetries:    make(map[string]int),
		validatorRetries:    make(map[string]int),
		maxValidatorRetries: 2,
		refinementCount:     make(map[string]int),
		maxRefinements:      3,
		phaseRetries:        make(map[int]int),
		maxPhaseRetries:     1,
		tainted:             loadTaintedFeatureIDs(missionDir, features),
		fixAttemptsByRoot:   loadFixAttemptBudgets(missionDir, features),
		maxFixAttempts:      defaultMaxFixAttemptsPerRoot,
	}
}

func (wp *WorkerPool) Start() tea.Cmd {
	wp.logger.Log("", "Mission execution started — %d features", len(wp.workers))

	minPhase := 999
	for phase := range wp.phases {
		if phase < minPhase {
			minPhase = phase
		}
	}

	contract := readFileContent(filepath.Join(wp.missionDir, "validation-contract.md"))

	go func() {
		if !wp.skipCritic {
			wp.logger.Log("", "Running critic gate before workers...")

			criticCh := make(chan WorkerEvent, 64)
			go RunCriticGate(wp.projectDir, wp.missionDir, wp.verbose, criticCh)

			var passed bool
			for ev := range criticCh {
				wp.eventCh <- ev
				if ev.Done && ev.Role == "critic" {
					passed = ev.Verdict == "PASS"
					break
				}
			}

			wp.mu.Lock()
			wp.criticDone = true
			wp.criticPassed = passed
			if wp.stopped {
				wp.mu.Unlock()
				return
			}
			wp.mu.Unlock()

			if !passed {
				wp.logger.Log("", "Critic gate failed — workers will not start")
				wp.eventCh <- WorkerEvent{
					AllDone: true,
					Line:    "✕ Critic gate failed — fix issues and retry",
				}
				return
			}

			wp.logger.Log("", "Critic gate passed — starting workers")
		} else {
			wp.logger.Log("", "Critic gate skipped — starting workers directly")
			wp.eventCh <- WorkerEvent{
				Role: "critic",
				Line: "⚠ Critic gate skipped by user",
			}
		}

		wp.runPhase(minPhase, contract)
	}()

	return listenWorker(wp.eventCh)
}

func (wp *WorkerPool) Stop() {
	wp.mu.Lock()
	defer wp.mu.Unlock()
	wp.stopped = true

	for _, w := range wp.workers {
		if (w.Status == WorkerRunning || w.Status == WorkerAwaitingValidation || w.Status == WorkerValidating || w.Status == WorkerRefining) && w.cmd != nil {
			_ = w.cmd.Process.Kill()
			w.Status = WorkerFailed
			w.EndTime = time.Now()
		}
	}
	wp.logger.Log("", "Execution stopped by user")
}

func (wp *WorkerPool) GetWorkerStatuses() []FeatureWorker {
	wp.mu.Lock()
	defer wp.mu.Unlock()

	var result []FeatureWorker
	for _, w := range wp.workers {
		cp := *w
		cp.cmd = nil
		result = append(result, cp)
	}
	return result
}

func (wp *WorkerPool) GetTaintedStatuses() map[string]bool {
	wp.mu.Lock()
	defer wp.mu.Unlock()

	out := make(map[string]bool, len(wp.tainted))
	for id, tainted := range wp.tainted {
		out[id] = tainted
	}
	return out
}

func (wp *WorkerPool) freshKnowledge() string {
	return readFileContent(filepath.Join(wp.missionDir, "knowledge-base.md"))
}

func (wp *WorkerPool) runPhase(phase int, contract string) {
	wp.mu.Lock()
	if wp.stopped {
		wp.mu.Unlock()
		return
	}

	featureIDs, ok := wp.phases[phase]
	if !ok {
		wp.mu.Unlock()
		wp.checkAllDone()
		return
	}

	var allPending []Feature
	var siblingNames []string
	for _, id := range featureIDs {
		w := wp.workers[id]
		if w.Status == WorkerPending {
			allPending = append(allPending, w.Feature)
			siblingNames = append(siblingNames, fmt.Sprintf("%s: %s", w.Feature.ID, w.Feature.Title))
		}
	}
	wp.mu.Unlock()

	if len(allPending) == 0 {
		wp.checkAllDone()
		return
	}

	ids := make([]string, len(allPending))
	for i, f := range allPending {
		ids[i] = f.ID
	}
	wp.logger.Log("", "Phase %d starting — %d features: %s", phase, len(allPending), strings.Join(ids, ", "))

	wp.eventCh <- WorkerEvent{
		Phase: phase,
		Line:  fmt.Sprintf("▶ Phase %d started — %d features", phase, len(allPending)),
	}

	ready := wp.startReadyFeatures(allPending, siblingNames, contract)
	if ready == 0 && len(allPending) > 0 {
		wp.logger.Log("", "Phase %d: no features ready (circular deps?)", phase)
	}
}

func (wp *WorkerPool) depsMetLocked(f Feature) bool {
	for _, depID := range f.DependsOn {
		w, ok := wp.workers[depID]
		if !ok {
			continue
		}
		if w.Status == WorkerDone {
			continue
		}
		// Fix features can proceed when their parent is failed — they exist to fix it.
		if f.Fixes == depID && w.Status == WorkerFailed {
			continue
		}
		// Per PDF p.16: blocked dep whose fix tree completed counts as satisfied for downstream.
		if w.Status == WorkerFailed && wp.isEffectivelyDoneLocked(depID) {
			continue
		}
		return false
	}
	return true
}

func (wp *WorkerPool) depsBlockedLocked(f Feature) (bool, []string) {
	var failedDeps []string
	for _, depID := range f.DependsOn {
		w, ok := wp.workers[depID]
		if !ok {
			continue
		}
		if w.Status == WorkerFailed {
			// Fix features are allowed to run even when their parent failed —
			// that's their whole purpose.
			if f.Fixes == depID {
				continue
			}
			// Per PDF p.16: if the fix tree resolved the parent's assertions, downstream is not blocked.
			if wp.isEffectivelyDoneLocked(depID) {
				continue
			}
			failedDeps = append(failedDeps, depID)
		}
	}
	if len(failedDeps) > 0 {
		return true, failedDeps
	}
	return false, nil
}

// isEffectivelyDoneLocked reports whether the feature's assertions are satisfied
// either directly or via its fix tree. Caller must hold wp.mu.
func (wp *WorkerPool) isEffectivelyDoneLocked(featureID string) bool {
	return wp.featureOutcomeLocked(featureID).EffectiveDone
}

func (wp *WorkerPool) featureOutcomeLocked(featureID string) FeatureOutcome {
	outcomes := wp.computeFeatureOutcomesLocked()
	if out, ok := outcomes[featureID]; ok {
		return out
	}
	return FeatureOutcome{EffectiveDone: true, Resolution: ResolutionOpen}
}

func (wp *WorkerPool) computeFeatureOutcomesLocked() map[string]FeatureOutcome {
	features := make([]Feature, 0, len(wp.workers))
	for _, w := range wp.workers {
		f := w.Feature
		switch w.Status {
		case WorkerRunning:
			f.Status = "in_progress"
		case WorkerAwaitingValidation:
			f.Status = "awaiting_validation"
		case WorkerValidating:
			f.Status = "validating"
		case WorkerRefining:
			f.Status = "refining"
		case WorkerDone:
			f.Status = "done"
		case WorkerFailed:
			f.Status = "blocked"
		default:
			f.Status = "pending"
		}
		if wp.tainted[f.ID] {
			f.Tainted = true
		}
		features = append(features, f)
	}
	return buildFeatureOutcomes(features, wp.tainted)
}

func (wp *WorkerPool) startReadyFeatures(candidates []Feature, siblingNames []string, contract string) int {
	started := 0
	wp.mu.Lock()

	var cascadeBlocked []Feature
	for _, f := range candidates {
		if wp.stopped {
			break
		}
		w := wp.workers[f.ID]
		if w.Status != WorkerPending {
			continue
		}
		if isBlocked, failedDeps := wp.depsBlockedLocked(f); isBlocked {
			w.Status = WorkerFailed
			w.EndTime = time.Now()
			wp.logger.Log(f.ID, "Blocked — depends on failed: %s", strings.Join(failedDeps, ", "))
			wp.eventCh <- WorkerEvent{
				FeatureID: f.ID,
				Line:      fmt.Sprintf("✕ %s blocked — depends on failed: %s", f.ID, strings.Join(failedDeps, ", ")),
			}
			cascadeBlocked = append(cascadeBlocked, f)
			continue
		}
		if !wp.depsMetLocked(f) {
			wp.logger.Log(f.ID, "Waiting — deps not satisfied: %s", strings.Join(f.DependsOn, ", "))
			continue
		}
		var siblings []string
		for _, s := range siblingNames {
			if !strings.HasPrefix(s, f.ID+":") {
				siblings = append(siblings, s)
			}
		}
		wp.mu.Unlock()
		wp.startWorker(f, siblings, contract)
		wp.mu.Lock()
		started++
	}

	wp.mu.Unlock()

	for _, f := range cascadeBlocked {
		wp.updateFeatureStatus(f.ID, "blocked")
	}

	return started
}

func (wp *WorkerPool) startWorker(feature Feature, siblings []string, contract string) {
	wp.mu.Lock()
	if wp.stopped {
		wp.mu.Unlock()
		return
	}
	w := wp.workers[feature.ID]
	w.Status = WorkerRunning
	w.StartTime = time.Now()
	wp.mu.Unlock()

	wp.logger.Log(feature.ID, "Worker started — %s", feature.Title)
	wp.updateFeatureStatus(feature.ID, "in_progress")

	wp.eventCh <- WorkerEvent{
		FeatureID: feature.ID,
		Line:      fmt.Sprintf("● Started: %s", feature.Title),
	}

	specPath := filepath.Dir(wp.missionDir)
	knowledge := wp.freshKnowledge()
	failureCtx := w.FailureContext
	prompt := BuildWorkerPrompt(feature, siblings, contract, knowledge, specPath, wp.projectDir, failureCtx)
	ch := make(chan ClaudeStreamMsg, 64)
	cmd := StartClaude(prompt, wp.projectDir, wp.verbose, ch)

	wp.mu.Lock()
	w.cmd = cmd
	wp.mu.Unlock()

	go wp.runWorkerLoop(feature, ch, contract)
}

func (wp *WorkerPool) runWorkerLoop(feature Feature, ch chan ClaudeStreamMsg, contract string) {
	for msg := range ch {
		wp.mu.Lock()
		if wp.stopped {
			wp.mu.Unlock()
			return
		}
		w := wp.workers[feature.ID]
		wp.mu.Unlock()

		if msg.Line != "" {
			wp.mu.Lock()
			w.Lines = append(w.Lines, msg.Line)
			w.LastLine = msg.Line
			if len(w.Lines) > 5000 {
				w.Lines = w.Lines[len(w.Lines)-5000:]
			}
			wp.mu.Unlock()

			wp.logger.Log(feature.ID, "%s", msg.Line)
			wp.eventCh <- WorkerEvent{FeatureID: feature.ID, Line: msg.Line}
		}

		if msg.Done {
			wp.mu.Lock()
			w.EndTime = time.Now()
			elapsed := w.EndTime.Sub(w.StartTime).Round(time.Second)

			if msg.Err != nil {
				transient := isTransientError(msg.Err)
				sessionID := msg.SessionID

				if transient {
					wp.transientRetries[feature.ID]++
				} else {
					wp.retries[feature.ID]++
				}

				attempt := wp.retries[feature.ID]
				tAttempt := wp.transientRetries[feature.ID]
				canRetry := !wp.stopped && ((transient && tAttempt <= maxTransientRetries) || (!transient && attempt <= wp.maxRetries))
				wp.mu.Unlock()

				if canRetry {
					label := ""
					retryNum := attempt
					retryMax := wp.maxRetries
					if transient {
						label = " (transient, resuming)"
						retryNum = tAttempt
						retryMax = maxTransientRetries
					} else if sessionID != "" {
						label = " (resuming session)"
					}

					backoff := time.Duration(retryNum) * 3 * time.Second
					if transient {
						backoff = time.Duration(tAttempt) * 5 * time.Second
					}

					wp.logger.Log(feature.ID, "Error: %v — retrying (%d/%d)%s after %s", msg.Err, retryNum, retryMax, label, elapsed)
					wp.eventCh <- WorkerEvent{
						FeatureID: feature.ID,
						Line:      fmt.Sprintf("⚠ %s error, retrying (%d/%d)%s...", feature.ID, retryNum, retryMax, label),
					}

					time.Sleep(backoff)

					wp.mu.Lock()
					w.Status = WorkerRunning
					w.StartTime = time.Now()
					wp.mu.Unlock()

					newCh := make(chan ClaudeStreamMsg, 64)
					var cmd *exec.Cmd
					if sessionID != "" {
						cmd = StartClaude(
							"An error interrupted your work. Continue implementing the feature from where you left off.",
							wp.projectDir, wp.verbose, newCh,
							"--resume", sessionID,
						)
					} else {
						var siblings []string
						specPath := filepath.Dir(wp.missionDir)
						knowledge := wp.freshKnowledge()
						wp.mu.Lock()
						retryCtx := w.FailureContext
						wp.mu.Unlock()
						prompt := BuildWorkerPrompt(feature, siblings, contract, knowledge, specPath, wp.projectDir, retryCtx)
						cmd = StartClaude(prompt, wp.projectDir, wp.verbose, newCh)
					}

					wp.mu.Lock()
					w.cmd = cmd
					wp.mu.Unlock()

					go wp.runWorkerLoop(feature, newCh, contract)
					return
				}

				wp.mu.Lock()
				w.Status = WorkerFailed
				wp.mu.Unlock()

				wp.logger.Log(feature.ID, "FAILED after %d attempts: %v", attempt, msg.Err)
				wp.logger.Log("", "Worker %s FAILED after %d attempts", feature.ID, attempt)
				wp.updateFeatureStatus(feature.ID, "blocked")

				wp.eventCh <- WorkerEvent{
					FeatureID: feature.ID,
					Done:      true,
					Err:       msg.Err,
					Line:      fmt.Sprintf("✕ %s failed after %d attempts", feature.ID, attempt),
				}

				wp.advanceIfPhaseComplete(feature.Phase, contract)
			} else {
				w.Status = WorkerAwaitingValidation
				wp.mu.Unlock()

				wp.logger.Log(feature.ID, "Worker completed in %s — awaiting validation", elapsed)
				wp.updateFeatureStatus(feature.ID, "awaiting_validation")

				wp.eventCh <- WorkerEvent{
					FeatureID: feature.ID,
					Line:      fmt.Sprintf("✓ %s worker done in %s — awaiting validation...", feature.ID, elapsed),
				}

				go wp.runValidator(feature, contract)
			}
			return
		}
	}
}

func (wp *WorkerPool) runValidator(feature Feature, contract string) {
	wp.mu.Lock()
	if wp.stopped {
		wp.mu.Unlock()
		return
	}
	wp.workers[feature.ID].Status = WorkerValidating
	wp.mu.Unlock()

	wp.updateFeatureStatus(feature.ID, "validating")

	specDir := filepath.Dir(wp.missionDir)
	prompt := BuildValidatorPrompt(feature, wp.missionDir, specDir)
	ch := make(chan ClaudeStreamMsg, 64)
	cmd := StartClaude(prompt, wp.projectDir, wp.verbose, ch, "--max-turns", "50")

	wp.mu.Lock()
	wp.workers[feature.ID].cmd = cmd
	wp.mu.Unlock()

	wp.eventCh <- WorkerEvent{
		FeatureID: feature.ID,
		Role:      "validator",
		Line:      fmt.Sprintf("◎ Validating %s...", feature.ID),
	}

	var resultText string
	var tainted bool
	for msg := range ch {
		wp.mu.Lock()
		if wp.stopped {
			wp.mu.Unlock()
			return
		}
		wp.mu.Unlock()

		if msg.Line != "" {
			if !tainted && isWorkerOutputAccess(msg.Line) {
				tainted = true
				wp.logger.Log(feature.ID, "[VALIDATOR] WARNING: accessed worker output — validation may be biased")
				wp.eventCh <- WorkerEvent{
					FeatureID: feature.ID,
					Role:      "validator",
					Line:      "⚠ WARNING: validator accessed worker output — black-box rule violated",
				}
			}
			wp.logger.Log(feature.ID, "[VALIDATOR] %s", msg.Line)
			wp.eventCh <- WorkerEvent{
				FeatureID: feature.ID,
				Role:      "validator",
				Line:      msg.Line,
			}
		}
		if msg.Done {
			if msg.Err != nil {
				if isTransientError(msg.Err) {
					wp.mu.Lock()
					wp.transientRetries[feature.ID+"_val"]++
					tAttempt := wp.transientRetries[feature.ID+"_val"]
					wp.mu.Unlock()
					if tAttempt <= maxTransientRetries {
						backoff := time.Duration(tAttempt) * 5 * time.Second
						wp.logger.Log(feature.ID, "Validator transient error: %v — retrying (%d/%d) in %s", msg.Err, tAttempt, maxTransientRetries, backoff)
						wp.eventCh <- WorkerEvent{
							FeatureID: feature.ID,
							Role:      "validator",
							Line:      fmt.Sprintf("⚠ %s validator socket error, retrying (%d/%d)...", feature.ID, tAttempt, maxTransientRetries),
						}
						time.Sleep(backoff)
						go wp.runValidator(feature, contract)
						return
					}
				}
				if wp.retryValidator(feature, contract, fmt.Sprintf("error: %v", msg.Err)) {
					return
				}
				wp.logger.Log(feature.ID, "Validator error after retries: %v — sending to refinement with synthetic report", msg.Err)
				report := syntheticValidatorReport(feature, "validator error", fmt.Sprintf("%v", msg.Err))
				wp.goToRefinement(feature, contract, report, fmt.Sprintf("⚠ %s validator unavailable (%v) — refining anyway...", feature.ID, msg.Err))
				return
			}
			resultText = msg.Result
		}
	}

	report := ParseValidatorReport(resultText)
	if report != nil {
		if tainted {
			report.Notes = append(report.Notes, "TAINTED: validator accessed worker output — black-box rule violated, results may be biased")
		}
		reportTainted := reportHasTaintedNote(*report)
		wp.mu.Lock()
		if reportTainted {
			wp.tainted[feature.ID] = true
		} else {
			delete(wp.tainted, feature.ID)
		}
		if w, ok := wp.workers[feature.ID]; ok {
			w.Feature.Tainted = reportTainted
		}
		wp.mu.Unlock()
		wp.persistReport(feature.ID, "validator", report)
	}

	if report == nil {
		if wp.retryValidator(feature, contract, "unparseable output") {
			return
		}
		wp.logger.Log(feature.ID, "Validator returned unparseable output after retries — sending to refinement with synthetic report")
		synth := syntheticValidatorReport(feature, "unparseable output", "validator did not return a valid JSON report after retries")
		wp.goToRefinement(feature, contract, synth, fmt.Sprintf("⚠ %s validator output unreadable — refining anyway...", feature.ID))
		return
	}

	switch report.Verdict {
	case "PASS":
		wp.logger.Log(feature.ID, "Validator PASSED")
		wp.mu.Lock()
		wp.workers[feature.ID].Status = WorkerDone
		wp.workers[feature.ID].EndTime = time.Now()
		wp.mu.Unlock()
		wp.updateFeatureStatus(feature.ID, "done")
		wp.eventCh <- WorkerEvent{
			FeatureID: feature.ID,
			Role:      "validator",
			Done:      true,
			Verdict:   "PASS",
			Line:      fmt.Sprintf("✓ %s validated — PASS", feature.ID),
		}
		wp.advanceIfPhaseComplete(feature.Phase, contract)

	case "FAIL":
		wp.goToRefinement(feature, contract, *report, fmt.Sprintf("✕ %s validation FAILED — refining...", feature.ID))

	default:
		wp.goToRefinement(feature, contract, *report, fmt.Sprintf("✕ %s validation BLOCKED — refining...", feature.ID))
	}
}

func (wp *WorkerPool) goToRefinement(feature Feature, contract string, report ValidatorReport, displayLine string) {
	wp.persistReport(feature.ID, "validator", &report)

	wp.logger.Log(feature.ID, "Sending to refinement (verdict=%s)", report.Verdict)

	wp.mu.Lock()
	wp.workers[feature.ID].Status = WorkerRefining
	wp.mu.Unlock()

	wp.updateFeatureStatus(feature.ID, "refining")

	wp.eventCh <- WorkerEvent{
		FeatureID: feature.ID,
		Role:      "validator",
		Done:      true,
		Verdict:   report.Verdict,
		Line:      displayLine,
	}

	go wp.runRefinement(feature, report, contract)
}

func syntheticValidatorReport(feature Feature, reason, detail string) ValidatorReport {
	now := time.Now().UTC().Format(time.RFC3339)
	var assertions []ValidatorAssertion
	for _, ref := range feature.ValidationRefs {
		id := strings.TrimSpace(ref)
		if colon := strings.Index(ref, ":"); colon > 0 {
			id = strings.TrimSpace(ref[:colon])
		}
		assertions = append(assertions, ValidatorAssertion{
			ID:       id,
			Result:   "BLOCKED",
			Evidence: fmt.Sprintf("Validator could not produce a verdict: %s", reason),
		})
	}
	return ValidatorReport{
		FeatureID:  feature.ID,
		Role:       "validator",
		StartedAt:  now,
		EndedAt:    now,
		Verdict:    "BLOCKED",
		Assertions: assertions,
		Notes: []string{
			fmt.Sprintf("VALIDATOR_FAILURE: %s", detail),
			"This report is synthetic — the validator could not produce a verdict. Diagnose whether the implementation is incomplete or whether the validator needs different evidence (e.g. a smoke-test script, more logging, an HTTP probe) and propose minimum-scope fix features accordingly.",
		},
	}
}

func (wp *WorkerPool) runRefinement(feature Feature, report ValidatorReport, contract string) {
	wp.mu.Lock()
	if wp.stopped {
		wp.mu.Unlock()
		return
	}
	round := wp.refinementCount[feature.ID] + 1
	wp.refinementCount[feature.ID] = round

	if round > wp.maxRefinements {
		wp.workers[feature.ID].Status = WorkerFailed
		wp.workers[feature.ID].EndTime = time.Now()
		wp.mu.Unlock()

		wp.logger.Log(feature.ID, "Refinement limit reached (%d rounds) — escalating", wp.maxRefinements)
		wp.updateFeatureStatus(feature.ID, "blocked")
		wp.eventCh <- WorkerEvent{
			FeatureID: feature.ID,
			Role:      "refinement",
			Done:      true,
			Line:      fmt.Sprintf("✕ %s refinement limit (%d rounds) — needs manual fix", feature.ID, wp.maxRefinements),
		}
		wp.advanceIfPhaseComplete(feature.Phase, contract)
		return
	}
	wp.mu.Unlock()

	wp.logger.Log(feature.ID, "Refinement round %d/%d", round, wp.maxRefinements)
	wp.eventCh <- WorkerEvent{
		FeatureID: feature.ID,
		Role:      "refinement",
		Line:      fmt.Sprintf("⟳ %s refinement round %d/%d", feature.ID, round, wp.maxRefinements),
	}

	specDir := filepath.Dir(wp.missionDir)
	prompt := BuildRefinementPrompt(feature, report, wp.missionDir, specDir)
	ch := make(chan ClaudeStreamMsg, 64)
	cmd := StartClaude(prompt, wp.projectDir, wp.verbose, ch, "--max-turns", "15")

	wp.mu.Lock()
	wp.workers[feature.ID].cmd = cmd
	wp.mu.Unlock()

	var resultText string
	for msg := range ch {
		wp.mu.Lock()
		if wp.stopped {
			wp.mu.Unlock()
			return
		}
		wp.mu.Unlock()

		if msg.Line != "" {
			wp.logger.Log(feature.ID, "[REFINE] %s", msg.Line)
			wp.eventCh <- WorkerEvent{
				FeatureID: feature.ID,
				Role:      "refinement",
				Line:      msg.Line,
			}
		}
		if msg.Done {
			if msg.Err != nil {
				if isTransientError(msg.Err) {
					wp.mu.Lock()
					wp.transientRetries[feature.ID+"_ref"]++
					tAttempt := wp.transientRetries[feature.ID+"_ref"]
					wp.mu.Unlock()
					if tAttempt <= maxTransientRetries {
						backoff := time.Duration(tAttempt) * 5 * time.Second
						wp.logger.Log(feature.ID, "Refinement transient error: %v — retrying (%d/%d) in %s", msg.Err, tAttempt, maxTransientRetries, backoff)
						wp.eventCh <- WorkerEvent{
							FeatureID: feature.ID,
							Role:      "refinement",
							Line:      fmt.Sprintf("⚠ %s refinement socket error, retrying (%d/%d)...", feature.ID, tAttempt, maxTransientRetries),
						}
						time.Sleep(backoff)
						go wp.runRefinement(feature, report, contract)
						return
					}
				}
				wp.logger.Log(feature.ID, "Refinement error: %v", msg.Err)
				wp.mu.Lock()
				wp.workers[feature.ID].Status = WorkerFailed
				wp.workers[feature.ID].EndTime = time.Now()
				wp.mu.Unlock()
				wp.updateFeatureStatus(feature.ID, "blocked")
				wp.eventCh <- WorkerEvent{
					FeatureID: feature.ID,
					Role:      "refinement",
					Done:      true,
					Line:      fmt.Sprintf("✕ %s refinement error: %v", feature.ID, msg.Err),
				}
				wp.advanceIfPhaseComplete(feature.Phase, contract)
				return
			}
			resultText = msg.Result
		}
	}

	fixes := ParseFixFeatures(resultText)
	if len(fixes) == 0 {
		wp.logger.Log(feature.ID, "Refinement produced no fix features — marking blocked")
		wp.mu.Lock()
		wp.workers[feature.ID].Status = WorkerFailed
		wp.workers[feature.ID].EndTime = time.Now()
		wp.mu.Unlock()
		wp.updateFeatureStatus(feature.ID, "blocked")
		wp.eventCh <- WorkerEvent{
			FeatureID: feature.ID,
			Role:      "refinement",
			Done:      true,
			Line:      fmt.Sprintf("✕ %s refinement produced no fixes", feature.ID),
		}
		wp.advanceIfPhaseComplete(feature.Phase, contract)
		return
	}

	if err := AddFixFeatures(wp.missionDir, fixes, feature.ID, &wp.fileMu); err != nil {
		wp.logger.Log(feature.ID, "Failed to write fix features: %v", err)
		wp.mu.Lock()
		wp.workers[feature.ID].Status = WorkerFailed
		wp.workers[feature.ID].EndTime = time.Now()
		wp.mu.Unlock()
		wp.updateFeatureStatus(feature.ID, "blocked")
		wp.eventCh <- WorkerEvent{
			FeatureID: feature.ID,
			Role:      "refinement",
			Done:      true,
			Line:      fmt.Sprintf("✕ %s refinement failed to persist fixes", feature.ID),
		}
		wp.advanceIfPhaseComplete(feature.Phase, contract)
		return
	}

	rootID := wp.resolveRootFeatureID(feature.ID)
	if rootID == "" {
		rootID = feature.ID
	}

	wp.mu.Lock()
	usedAttempts := wp.fixAttemptsByRoot[rootID]
	projectedAttempts := usedAttempts + len(fixes)
	limit := wp.maxFixAttempts
	if projectedAttempts > limit {
		wp.workers[feature.ID].Status = WorkerFailed
		wp.workers[feature.ID].EndTime = time.Now()
		for _, fix := range fixes {
			wp.workers[fix.ID] = &FeatureWorker{
				Feature: fix,
				Status:  WorkerFailed,
			}
		}
		wp.mu.Unlock()

		wp.updateFeatureStatus(feature.ID, "blocked")
		for _, fix := range fixes {
			wp.updateFeatureStatus(fix.ID, "blocked")
		}

		wp.logger.Log(feature.ID, "Fix autopilot limit reached for %s: %d/%d (new fixes=%d)", rootID, usedAttempts, limit, len(fixes))
		wp.eventCh <- WorkerEvent{
			FeatureID: feature.ID,
			Role:      "refinement",
			Done:      true,
			Line:      fmt.Sprintf("✕ %s fix autopilot limit reached for %s (%d/%d) — awaiting manual action", feature.ID, rootID, usedAttempts, limit),
		}
		wp.advanceIfPhaseComplete(feature.Phase, contract)
		return
	}
	wp.fixAttemptsByRoot[rootID] = projectedAttempts

	wp.workers[feature.ID].Status = WorkerFailed
	wp.workers[feature.ID].EndTime = time.Now()
	for _, fix := range fixes {
		wp.workers[fix.ID] = &FeatureWorker{
			Feature: fix,
			Status:  WorkerPending,
		}
		wp.phases[fix.Phase] = append(wp.phases[fix.Phase], fix.ID)
	}
	wp.mu.Unlock()

	wp.updateFeatureStatus(feature.ID, "blocked")

	fixIDs := make([]string, len(fixes))
	for i, f := range fixes {
		fixIDs[i] = f.ID
	}
	wp.logger.Log(
		feature.ID,
		"Generated %d fix features: %s (autopilot budget %s %d/%d)",
		len(fixes), strings.Join(fixIDs, ", "), rootID, projectedAttempts, wp.maxFixAttempts,
	)
	wp.eventCh <- WorkerEvent{
		FeatureID: feature.ID,
		Role:      "refinement",
		Done:      true,
		Line:      fmt.Sprintf("⟳ %s → %d fix features: %s", feature.ID, len(fixes), strings.Join(fixIDs, ", ")),
	}

	go wp.runFixCriticAndStart(fixes, contract)
}

func (wp *WorkerPool) runFixCriticAndStart(fixes []Feature, contract string) {
	criticCh := make(chan WorkerEvent, 64)
	go RunFixCriticGate(wp.projectDir, wp.missionDir, fixes, wp.verbose, criticCh)

	var passed bool
	for ev := range criticCh {
		wp.eventCh <- ev
		if ev.Done && ev.Role == "critic" {
			passed = ev.Verdict == "PASS"
			break
		}
	}

	if !passed {
		wp.logger.Log("", "Fix critic gate failed — fix features will not start")
		wp.mu.Lock()
		for _, fix := range fixes {
			if w, ok := wp.workers[fix.ID]; ok {
				w.Status = WorkerFailed
				w.EndTime = time.Now()
			}
		}
		wp.mu.Unlock()
		for _, fix := range fixes {
			wp.updateFeatureStatus(fix.ID, "blocked")
		}
		wp.advanceIfPhaseComplete(fixes[0].Phase, contract)
		return
	}

	wp.logger.Log("", "Fix critic gate passed — starting generated fix workers")
	siblingNames := make([]string, 0, len(fixes))
	for _, fix := range fixes {
		siblingNames = append(siblingNames, fmt.Sprintf("%s: %s", fix.ID, fix.Title))
	}
	started := wp.startReadyFeatures(fixes, siblingNames, contract)
	if started == 0 {
		wp.logger.Log("", "No generated fix features were ready after critic pass")
		wp.eventCh <- WorkerEvent{
			Role: "refinement",
			Line: "⚠ No generated fix features are ready — awaiting dependency resolution",
		}
		wp.advanceIfPhaseComplete(fixes[0].Phase, contract)
	}
}

func (wp *WorkerPool) retryValidator(feature Feature, contract string, reason string) bool {
	wp.mu.Lock()
	wp.validatorRetries[feature.ID]++
	attempt := wp.validatorRetries[feature.ID]
	canRetry := attempt <= wp.maxValidatorRetries && !wp.stopped
	wp.mu.Unlock()

	if !canRetry {
		return false
	}

	wp.logger.Log(feature.ID, "Validator %s — retrying (%d/%d)", reason, attempt, wp.maxValidatorRetries)
	wp.eventCh <- WorkerEvent{
		FeatureID: feature.ID,
		Role:      "validator",
		Line:      fmt.Sprintf("⚠ %s validator %s — retrying (%d/%d)...", feature.ID, reason, attempt, wp.maxValidatorRetries),
	}

	time.Sleep(time.Duration(attempt) * 2 * time.Second)

	go wp.runValidator(feature, contract)
	return true
}

func (wp *WorkerPool) buildFailureContext(w *FeatureWorker) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("PREVIOUS ATTEMPT FAILED (feature %s).\n", w.Feature.ID))

	reportPath := filepath.Join(wp.missionDir, "runs", w.Feature.ID+"-validator.json")
	if data, err := os.ReadFile(reportPath); err == nil {
		var report ValidatorReport
		if json.Unmarshal(data, &report) == nil && report.Verdict != "" {
			sb.WriteString(fmt.Sprintf("Validator verdict: %s\n", report.Verdict))
			for _, a := range report.Assertions {
				if a.Result != "PASS" {
					sb.WriteString(fmt.Sprintf("  - %s: %s — %s\n", a.ID, a.Result, a.Evidence))
				}
			}
			for _, n := range report.Notes {
				sb.WriteString(fmt.Sprintf("  Note: %s\n", n))
			}
		}
	}

	if len(w.Lines) > 0 {
		sb.WriteString("\nKey log lines from failed run:\n")
		start := 0
		if len(w.Lines) > 30 {
			start = len(w.Lines) - 30
		}
		for _, line := range w.Lines[start:] {
			if strings.Contains(line, "error") || strings.Contains(line, "Error") ||
				strings.Contains(line, "denied") || strings.Contains(line, "fail") ||
				strings.Contains(line, "FAIL") || strings.Contains(line, "✕") ||
				strings.Contains(line, "Cannot") || strings.Contains(line, "cannot") ||
				strings.Contains(line, "missing") || strings.Contains(line, "not found") {
				sb.WriteString(fmt.Sprintf("  %s\n", line))
			}
		}
	}

	sb.WriteString("\nFix the issues above in this attempt. Do NOT repeat the same mistakes.")
	return sb.String()
}

func (wp *WorkerPool) retryFailedInPhase(phase int, contract string) bool {
	wp.mu.Lock()
	wp.phaseRetries[phase]++
	attempt := wp.phaseRetries[phase]
	if attempt > wp.maxPhaseRetries || wp.stopped {
		wp.mu.Unlock()
		return false
	}

	ids := wp.phases[phase]
	var toRetry []Feature
	for _, id := range ids {
		w := wp.workers[id]
		if w.Status == WorkerFailed && !wp.isEffectivelyDoneLocked(id) {
			w.FailureContext = wp.buildFailureContext(w)
			w.Status = WorkerPending
			w.StartTime = time.Time{}
			w.EndTime = time.Time{}
			w.Lines = nil
			w.LastLine = ""
			toRetry = append(toRetry, w.Feature)
		}
	}
	wp.mu.Unlock()

	if len(toRetry) == 0 {
		return false
	}

	for _, f := range toRetry {
		wp.updateFeatureStatus(f.ID, "pending")
	}

	retryIDs := make([]string, len(toRetry))
	for i, f := range toRetry {
		retryIDs[i] = f.ID
	}
	wp.logger.Log("", "Phase %d: retrying %d failed features (%d/%d): %s", phase, len(toRetry), attempt, wp.maxPhaseRetries, strings.Join(retryIDs, ", "))
	wp.eventCh <- WorkerEvent{
		Phase: phase,
		Line:  fmt.Sprintf("⟳ Phase %d retry (%d/%d) — %d features: %s", phase, attempt, wp.maxPhaseRetries, len(toRetry), strings.Join(retryIDs, ", ")),
	}

	go wp.runPhase(phase, contract)
	return true
}

func (wp *WorkerPool) advanceIfPhaseComplete(phase int, contract string) {
	wp.mu.Lock()
	if wp.stopped {
		wp.mu.Unlock()
		return
	}

	ids := wp.phases[phase]

	var waitingInPhase []Feature
	var siblingNames []string
	allTerminal := true
	for _, id := range ids {
		w := wp.workers[id]
		switch w.Status {
		case WorkerPending:
			waitingInPhase = append(waitingInPhase, w.Feature)
			siblingNames = append(siblingNames, fmt.Sprintf("%s: %s", w.Feature.ID, w.Feature.Title))
			allTerminal = false
		case WorkerRunning, WorkerAwaitingValidation, WorkerValidating, WorkerRefining:
			allTerminal = false
		}
	}
	wp.mu.Unlock()

	if len(waitingInPhase) > 0 {
		wp.startReadyFeatures(waitingInPhase, siblingNames, contract)
	}

	if !allTerminal {
		return
	}

	hasFailed := false
	wp.mu.Lock()
	for _, id := range wp.phases[phase] {
		if wp.workers[id].Status == WorkerFailed && !wp.isEffectivelyDoneLocked(id) {
			hasFailed = true
			break
		}
	}
	wp.mu.Unlock()

	// Root-phase retries are manual-only. We intentionally avoid automatic
	// phase retries to keep unresolved failures explicit and user-controlled.
	if hasFailed {
		wp.logger.Log("", "Phase %d ended with unresolved failures — manual recovery required", phase)
		wp.eventCh <- WorkerEvent{
			Phase: phase,
			Line:  fmt.Sprintf("⚠ Phase %d ended with unresolved failures — manual recovery required", phase),
		}
	}

	wp.logger.Log("", "Phase %d complete", phase)
	wp.eventCh <- WorkerEvent{
		Phase: phase,
		Line:  fmt.Sprintf("✓ Phase %d complete", phase),
	}

	nextPhase := -1
	wp.mu.Lock()
	for p, pIDs := range wp.phases {
		if p <= phase {
			continue
		}
		for _, id := range pIDs {
			if wp.workers[id].Status == WorkerPending {
				if nextPhase == -1 || p < nextPhase {
					nextPhase = p
				}
				break
			}
		}
	}
	wp.mu.Unlock()

	if nextPhase == -1 {
		wp.checkAllDone()
		return
	}

	go wp.runPhase(nextPhase, contract)
}

func (wp *WorkerPool) checkAllDone() {
	wp.mu.Lock()
	for _, w := range wp.workers {
		if w.Status == WorkerRunning || w.Status == WorkerPending || w.Status == WorkerAwaitingValidation || w.Status == WorkerValidating || w.Status == WorkerRefining {
			wp.mu.Unlock()
			return
		}
	}
	wp.mu.Unlock()

	var done, viaFix, failed, tainted int
	wp.mu.Lock()
	outcomes := wp.computeFeatureOutcomesLocked()
	for _, w := range wp.workers {
		switch w.Status {
		case WorkerDone:
			done++
			if outcomes[w.Feature.ID].Resolution == ResolutionResolvedTainted {
				tainted++
			}
		case WorkerFailed:
			out := outcomes[w.Feature.ID]
			if out.EffectiveDone {
				viaFix++
				if out.Resolution == ResolutionResolvedTainted {
					tainted++
				}
			} else {
				failed++
			}
		}
	}
	wp.mu.Unlock()

	wp.logger.Log("", "All phases complete — %d done, %d via fix, %d failed, %d tainted", done, viaFix, failed, tainted)
	wp.eventCh <- WorkerEvent{
		AllDone: true,
		Line:    fmt.Sprintf("✓ Execution complete — %d done, %d via fix, %d failed, %d tainted", done, viaFix, failed, tainted),
	}
}

func (wp *WorkerPool) persistReport(featureID, role string, data any) {
	runDir := filepath.Join(wp.missionDir, "runs")
	_ = os.MkdirAll(runDir, 0o755)

	reportJSON, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return
	}

	filename := fmt.Sprintf("%s-%s.json", featureID, role)
	_ = os.WriteFile(filepath.Join(runDir, filename), reportJSON, 0o644)
}

func (wp *WorkerPool) updateFeatureStatus(featureID string, status string) {
	wp.fileMu.Lock()
	defer wp.fileMu.Unlock()

	path := filepath.Join(wp.missionDir, "features.json")
	data, err := os.ReadFile(path)
	if err != nil {
		wp.logger.Log(featureID, "WARN: cannot read features.json: %v", err)
		return
	}

	var manifest FeaturesManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		wp.logger.Log(featureID, "WARN: cannot parse features.json: %v", err)
		return
	}

	update := func(features []Feature) {
		for i := range features {
			if features[i].ID == featureID {
				features[i].Status = status
			}
		}
	}
	update(manifest.Features)
	update(manifest.FixFeatures)

	all := make([]Feature, 0, len(manifest.Features)+len(manifest.FixFeatures))
	all = append(all, manifest.Features...)
	all = append(all, manifest.FixFeatures...)
	tainted := loadTaintedFeatureIDs(wp.missionDir, all)
	for id, isTainted := range wp.tainted {
		if isTainted {
			tainted[id] = true
		}
	}
	outcomes := buildFeatureOutcomes(all, tainted)
	now := time.Now().UTC().Format(time.RFC3339)
	applyOutcome := func(features []Feature) {
		for i := range features {
			out, ok := outcomes[features[i].ID]
			if !ok {
				continue
			}
			features[i].Resolution = out.Resolution
			features[i].Tainted = out.Tainted
			if out.ResolvedBy != "" {
				features[i].ResolvedBy = out.ResolvedBy
			}
			if (out.Resolution == ResolutionResolvedViaFix || out.Resolution == ResolutionResolvedTainted) && features[i].ResolvedAt == "" {
				features[i].ResolvedAt = now
			}
			if out.Resolution == ResolutionOpen || out.Resolution == ResolutionUnresolved {
				features[i].ResolvedAt = ""
				features[i].ResolvedBy = ""
			}
		}
	}
	applyOutcome(manifest.Features)
	applyOutcome(manifest.FixFeatures)

	out, _ := json.MarshalIndent(manifest, "", "  ")
	if err := os.WriteFile(path, out, 0o644); err != nil {
		wp.logger.Log(featureID, "WARN: cannot write features.json: %v", err)
	}
}

func readFileContent(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(data)
}

func isWorkerOutputAccess(line string) bool {
	lower := strings.ToLower(line)
	patterns := []string{"-worker.json", "-worker.md", "runs/", "worker.json"}
	for _, p := range patterns {
		if strings.Contains(lower, p) && strings.Contains(lower, "worker") {
			if strings.Contains(lower, "▸ read") || strings.Contains(lower, "▸ bash") ||
				strings.Contains(lower, "file_path") {
				return true
			}
		}
	}
	return false
}

func listenWorker(ch chan WorkerEvent) tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok {
			return WorkerEvent{AllDone: true}
		}
		return ev
	}
}

func loadFixAttemptBudgets(missionDir string, fallback []Feature) map[string]int {
	all := fallback
	if manifestFeatures, err := readAllFeaturesFromManifest(missionDir); err == nil && len(manifestFeatures) > 0 {
		all = manifestFeatures
	}

	byID := make(map[string]Feature, len(all))
	for _, f := range all {
		if f.ID == "" {
			continue
		}
		if _, exists := byID[f.ID]; !exists {
			byID[f.ID] = f
		}
	}

	out := make(map[string]int)
	for _, f := range all {
		if f.ID == "" || f.Fixes == "" {
			continue
		}
		rootID := resolveRootFeatureIDInMap(byID, f.ID)
		out[rootID]++
	}
	return out
}

func readAllFeaturesFromManifest(missionDir string) ([]Feature, error) {
	path := filepath.Join(missionDir, "features.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var manifest FeaturesManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, err
	}
	all := make([]Feature, 0, len(manifest.Features)+len(manifest.FixFeatures))
	all = append(all, manifest.Features...)
	all = append(all, manifest.FixFeatures...)
	return all, nil
}

func resolveRootFeatureIDInMap(byID map[string]Feature, featureID string) string {
	if featureID == "" {
		return ""
	}
	current := featureID
	seen := make(map[string]struct{})
	for {
		if _, loop := seen[current]; loop {
			return current
		}
		seen[current] = struct{}{}
		f, ok := byID[current]
		if !ok || f.Fixes == "" {
			return current
		}
		current = f.Fixes
	}
}

func (wp *WorkerPool) resolveRootFeatureID(featureID string) string {
	if featureID == "" {
		return ""
	}

	wp.mu.Lock()
	byID := make(map[string]Feature, len(wp.workers))
	for id, w := range wp.workers {
		byID[id] = w.Feature
	}
	rootID := resolveRootFeatureIDInMap(byID, featureID)
	wp.mu.Unlock()

	if rootID != featureID {
		return rootID
	}

	manifestFeatures, err := readAllFeaturesFromManifest(wp.missionDir)
	if err != nil {
		return rootID
	}
	byID = make(map[string]Feature, len(manifestFeatures))
	for _, f := range manifestFeatures {
		if _, exists := byID[f.ID]; !exists {
			byID[f.ID] = f
		}
	}
	return resolveRootFeatureIDInMap(byID, featureID)
}
