package internal

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
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
const maxAutoResetsPerRun = 1
const criticTransientBudget = 12
const criticStructuralBudget = 4
const criticAutoFixBudget = 3
const debugLogPath = "/Users/alefemafra/Project/mission-dashboard-v2/.cursor/debug-d4fc9a.log"
const debugSessionID = "d4fc9a"
const debugRunIDRefineStuckV1 = "investigate-refine-stuck-v1"
const debugRunIDValidatorVerdictV1 = "validator-pass-fail-mismatch-v1"

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
	autonomousState     AutonomousRuntimeState
	autonomousMode      bool
	criticAutoFixFn     func(report *CriticReport) error
}

func NewWorkerPool(projectDir, missionDir string, features []Feature, logger *MissionLogger, verbose *bool) *WorkerPool {
	state := loadAutonomousRuntimeState(missionDir)

	workers := make(map[string]*FeatureWorker)
	phases := make(map[int][]string)

	for _, f := range features {
		workers[f.ID] = &FeatureWorker{
			Feature: f,
			Status:  WorkerPending,
		}
		phases[f.Phase] = append(phases[f.Phase], f.ID)
	}

	wp := &WorkerPool{
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
		autonomousState:     state,
	}
	wp.criticAutoFixFn = wp.runInitialCriticAutoFix
	return wp
}

func (wp *WorkerPool) Start() tea.Cmd {
	wp.logger.Log("", "Mission execution started — %d features", len(wp.workers))

	contract := readFileContent(filepath.Join(wp.missionDir, "validation-contract.md"))

	go func() {
		minPhase, hasPhase := wp.minPhase()
		if !hasPhase {
			wp.checkAllDone()
			return
		}

		if !wp.skipCritic {
			wp.logger.Log("", "Running critic gate before workers...")
			rootID := normalizeRecoveryRootID(filepath.Base(filepath.Dir(wp.missionDir)))
			for {
				passed, report := wp.runInitialCriticGateOnce()

				wp.mu.Lock()
				wp.criticDone = true
				wp.criticPassed = passed
				stopped := wp.stopped
				wp.mu.Unlock()
				if stopped {
					return
				}

				if passed {
					wp.clearRecoveryLevel(rootID)
					wp.clearCriticAttempts(rootID)
					wp.logger.Log("", "Critic gate passed — starting workers")
					break
				}

				if !wp.autonomousMode {
					wp.logger.Log("", "Critic gate failed — workers will not start")
					wp.eventCh <- WorkerEvent{
						AllDone: true,
						Line:    "✕ Critic gate failed — fix issues and retry",
					}
					return
				}

				if wp.tryAutonomousCriticAutoFix(rootID, report) {
					continue
				}

				action, attempt, signature := wp.decideCriticRecoveryAction(rootID, report)
				switch action {
				case criticRecoveryRetryTransient:
					backoff := time.Duration(attempt) * 5 * time.Second
					wp.logger.Log("", "Critic transient failure signature=%s attempt=%d/%d — retrying in %s", signature, attempt, criticTransientBudget, backoff)
					wp.eventCh <- WorkerEvent{
						Role: "critic",
						Line: fmt.Sprintf("⚠ Critic transient failure — retrying with resume (%d/%d) in %s", attempt, criticTransientBudget, backoff),
					}
					time.Sleep(backoff)
					continue
				case criticRecoveryRetryStructural:
					wp.logger.Log("", "Critic structural recovery attempt %d/%d (signature=%s)", attempt, criticStructuralBudget, signature)
					wp.eventCh <- WorkerEvent{
						Role: "critic",
						Line: fmt.Sprintf("⚠ Critic still failing — auto recovery step %d/%d (reset + retry)", attempt, criticStructuralBudget),
					}
					resetMinPhase, err := wp.resetMissionToRootPending()
					if err != nil {
						wp.logger.Log("", "Critic structural recovery reset failed: %v", err)
						wp.eventCh <- WorkerEvent{
							Role: "critic",
							Line: fmt.Sprintf("⚠ Critic recovery reset failed: %v", err),
						}
						continue
					}
					minPhase = resetMinPhase
					continue
				case criticRecoveryBypass:
					wp.logger.Log("", "Critic bypass activated after exhausted retries (signature=%s, bypass_count=%d)", signature, attempt)
					wp.eventCh <- WorkerEvent{
						Role: "critic",
						Line: fmt.Sprintf("⚠ Critic bypass enabled after recovery budget exhaustion (count=%d) — continuing mission in sleep mode", attempt),
					}
					wp.clearCriticAttempts(rootID)
					break
				}
				break
			}
		} else {
			wp.logger.Log("", "Critic gate skipped — starting workers directly")
			wp.eventCh <- WorkerEvent{
				Role: "critic",
				Line: "⚠ Critic gate skipped by user",
			}
		}

		if minPhase < 0 {
			wp.checkAllDone()
			return
		}
		wp.runPhase(minPhase, contract)
	}()

	return listenWorker(wp.eventCh)
}

func (wp *WorkerPool) minPhase() (int, bool) {
	wp.mu.Lock()
	defer wp.mu.Unlock()
	if len(wp.phases) == 0 {
		return -1, false
	}
	minPhase := -1
	for phase := range wp.phases {
		if minPhase == -1 || phase < minPhase {
			minPhase = phase
		}
	}
	return minPhase, minPhase >= 0
}

func (wp *WorkerPool) runInitialCriticGateOnce() (bool, *CriticReport) {
	criticCh := make(chan WorkerEvent, 64)
	go RunCriticGate(wp.projectDir, wp.missionDir, wp.verbose, criticCh)

	var passed bool
	var report *CriticReport
	for ev := range criticCh {
		wp.eventCh <- ev
		if ev.Done && ev.Role == "critic" {
			passed = ev.Verdict == "PASS"
			report = ev.CriticReport
			break
		}
	}
	return passed, report
}

func (wp *WorkerPool) resetMissionToRootPending() (int, error) {
	wp.fileMu.Lock()
	path := filepath.Join(wp.missionDir, "features.json")
	data, err := os.ReadFile(path)
	if err != nil {
		wp.fileMu.Unlock()
		return -1, err
	}

	var manifest FeaturesManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		wp.fileMu.Unlock()
		return -1, err
	}

	for i := range manifest.Features {
		manifest.Features[i].Status = "pending"
		manifest.Features[i].Resolution = ""
		manifest.Features[i].ResolvedBy = ""
		manifest.Features[i].ResolvedAt = ""
		manifest.Features[i].Tainted = false
	}
	manifest.FixFeatures = nil

	out, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		wp.fileMu.Unlock()
		return -1, err
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		wp.fileMu.Unlock()
		return -1, err
	}
	wp.fileMu.Unlock()

	wp.mu.Lock()
	wp.workers = make(map[string]*FeatureWorker, len(manifest.Features))
	wp.phases = make(map[int][]string)
	for _, f := range manifest.Features {
		f.Status = "pending"
		wp.workers[f.ID] = &FeatureWorker{Feature: f, Status: WorkerPending}
		wp.phases[f.Phase] = append(wp.phases[f.Phase], f.ID)
	}
	wp.retries = make(map[string]int)
	wp.transientRetries = make(map[string]int)
	wp.validatorRetries = make(map[string]int)
	wp.refinementCount = make(map[string]int)
	wp.phaseRetries = make(map[int]int)
	wp.fixAttemptsByRoot = make(map[string]int)
	wp.tainted = make(map[string]bool)
	wp.mu.Unlock()

	wp.updateAutonomousState(func(state *AutonomousRuntimeState) {
		state.AutoResetCount++
	})

	minPhase, ok := wp.minPhase()
	if !ok {
		return -1, nil
	}
	return minPhase, nil
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

func (wp *WorkerPool) autonomousSessionID(role, featureID string) string {
	key := autonomousSessionKey(role, featureID)
	wp.mu.Lock()
	defer wp.mu.Unlock()
	return strings.TrimSpace(wp.autonomousState.LastSessionIDs[key])
}

func (wp *WorkerPool) rememberAutonomousSession(role, featureID, sessionID string) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return
	}
	key := autonomousSessionKey(role, featureID)
	wp.updateAutonomousState(func(state *AutonomousRuntimeState) {
		state.LastSessionIDs[key] = sessionID
	})
}

func (wp *WorkerPool) updateAutonomousState(mutate func(state *AutonomousRuntimeState)) {
	wp.mu.Lock()
	mutate(&wp.autonomousState)
	ensureAutonomousState(&wp.autonomousState)
	snapshot := wp.autonomousState
	wp.mu.Unlock()

	if err := saveAutonomousRuntimeState(wp.missionDir, snapshot); err != nil {
		if wp.logger != nil {
			wp.logger.Log("", "WARN: failed to persist autonomous state: %v", err)
		}
	}
}

func (wp *WorkerPool) recordFailureSignature(rootID, signature string) (count int, level int) {
	rootID = strings.TrimSpace(rootID)
	signature = strings.TrimSpace(strings.ToLower(signature))
	if rootID == "" {
		rootID = "quest"
	}
	if signature == "" {
		signature = "unknown"
	}
	key := rootID + "|" + signature

	wp.updateAutonomousState(func(state *AutonomousRuntimeState) {
		state.FailureSignatures[key]++
		count = state.FailureSignatures[key]
		level = count
		if level > 5 {
			level = 5
		}
		state.RecoveryLevel[rootID] = level
	})
	return count, level
}

func (wp *WorkerPool) clearRecoveryLevel(rootID string) {
	rootID = strings.TrimSpace(rootID)
	if rootID == "" {
		return
	}
	wp.updateAutonomousState(func(state *AutonomousRuntimeState) {
		delete(state.RecoveryLevel, rootID)
	})
}

type criticRecoveryAction int

const (
	criticRecoveryRetryTransient criticRecoveryAction = iota
	criticRecoveryRetryStructural
	criticRecoveryBypass
)

func normalizeRecoveryRootID(rootID string) string {
	rootID = strings.TrimSpace(rootID)
	if rootID == "" {
		return "quest"
	}
	return rootID
}

func isCriticTransientReport(report *CriticReport) bool {
	if report == nil {
		return false
	}
	for _, finding := range report.Findings {
		if finding.Status != "needs-work" {
			continue
		}
		text := strings.ToLower(strings.TrimSpace(
			strings.Join([]string{
				finding.Criterion,
				finding.Note,
				finding.Suggestion,
				finding.Target,
			}, " "),
		))
		if strings.Contains(text, "timed out") ||
			strings.Contains(text, "timeout") ||
			strings.Contains(text, "socket") ||
			strings.Contains(text, "connection reset") ||
			strings.Contains(text, "econn") ||
			strings.Contains(text, "network timeout") {
			return true
		}
	}
	return false
}

func (wp *WorkerPool) bumpCriticTransientAttempt(rootID string) int {
	rootID = normalizeRecoveryRootID(rootID)
	var attempt int
	wp.updateAutonomousState(func(state *AutonomousRuntimeState) {
		state.CriticTransientAttempt[rootID]++
		attempt = state.CriticTransientAttempt[rootID]
	})
	return attempt
}

func (wp *WorkerPool) bumpCriticStructuralAttempt(rootID string) int {
	rootID = normalizeRecoveryRootID(rootID)
	var attempt int
	wp.updateAutonomousState(func(state *AutonomousRuntimeState) {
		state.CriticStructuralTry[rootID]++
		attempt = state.CriticStructuralTry[rootID]
	})
	return attempt
}

func (wp *WorkerPool) bumpCriticAutoFixAttempt(rootID string) int {
	rootID = normalizeRecoveryRootID(rootID)
	var attempt int
	wp.updateAutonomousState(func(state *AutonomousRuntimeState) {
		state.CriticAutoFixAttempt[rootID]++
		attempt = state.CriticAutoFixAttempt[rootID]
	})
	return attempt
}

func (wp *WorkerPool) recordCriticBypass(rootID string) int {
	rootID = normalizeRecoveryRootID(rootID)
	var count int
	wp.updateAutonomousState(func(state *AutonomousRuntimeState) {
		state.CriticBypassCount[rootID]++
		count = state.CriticBypassCount[rootID]
	})
	return count
}

func (wp *WorkerPool) clearCriticAttempts(rootID string) {
	rootID = normalizeRecoveryRootID(rootID)
	wp.updateAutonomousState(func(state *AutonomousRuntimeState) {
		delete(state.CriticTransientAttempt, rootID)
		delete(state.CriticStructuralTry, rootID)
		delete(state.CriticAutoFixAttempt, rootID)
	})
}

func (wp *WorkerPool) clearCriticPhaseSessions(phaseIDs []string) {
	if len(phaseIDs) == 0 {
		return
	}
	wp.updateAutonomousState(func(state *AutonomousRuntimeState) {
		for _, phaseID := range phaseIDs {
			phaseID = strings.TrimSpace(phaseID)
			if phaseID == "" {
				continue
			}
			delete(state.LastSessionIDs, autonomousSessionKey("critic", "phase-"+phaseID))
		}
	})
}

func (wp *WorkerPool) runInitialCriticAutoFix(report *CriticReport) error {
	specDir := filepath.Dir(wp.missionDir)
	verboseLogs := wp.verbose != nil && *wp.verbose
	return RunCriticBlockingAutoFix(report, specDir, wp.projectDir, wp.verbose, func(line string) {
		if !verboseLogs {
			return
		}
		if strings.TrimSpace(line) == "" {
			return
		}
		wp.eventCh <- WorkerEvent{Role: "critic-fix", Line: line}
	})
}

func (wp *WorkerPool) runCriticAutoFix(report *CriticReport) error {
	if wp.criticAutoFixFn != nil {
		return wp.criticAutoFixFn(report)
	}
	return wp.runInitialCriticAutoFix(report)
}

func (wp *WorkerPool) tryAutonomousCriticAutoFix(rootID string, report *CriticReport) bool {
	rootID = normalizeRecoveryRootID(rootID)
	if report == nil || len(report.BlockingFailures()) == 0 {
		return false
	}

	attempt := wp.bumpCriticAutoFixAttempt(rootID)
	if attempt > criticAutoFixBudget {
		if wp.logger != nil {
			wp.logger.Log("", "Critic auto-fix budget exhausted for %s (%d/%d)", rootID, attempt, criticAutoFixBudget)
		}
		wp.eventCh <- WorkerEvent{
			Role: "critic-fix",
			Line: fmt.Sprintf("⚠ Critic auto-fix budget exhausted (%d/%d) — proceeding with recovery ladder", attempt, criticAutoFixBudget),
		}
		return false
	}

	if wp.logger != nil {
		wp.logger.Log("", "Critic auto-fix attempt %d/%d for %s", attempt, criticAutoFixBudget, rootID)
	}
	wp.eventCh <- WorkerEvent{
		Role: "critic-fix",
		Line: "▶ Starting critic auto-fix agent...",
	}
	wp.eventCh <- WorkerEvent{
		Role: "critic-fix",
		Line: fmt.Sprintf("⚠ Critic needs-work — auto-fixing blocking findings (%d/%d)...", attempt, criticAutoFixBudget),
	}

	startedAt := time.Now()
	snapshotBefore, beforeErr := captureCriticArtifactSnapshot(wp.missionDir)
	progressStop := make(chan struct{})
	go func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-progressStop:
				return
			case <-ticker.C:
				elapsed := time.Since(startedAt).Round(time.Second)
				wp.eventCh <- WorkerEvent{
					Role: "critic-fix",
					Line: fmt.Sprintf("… Still auto-fixing (%s)", elapsed),
				}
			}
		}
	}()

	if err := wp.runCriticAutoFix(report); err != nil {
		close(progressStop)
		elapsed := time.Since(startedAt).Round(time.Second)
		if wp.logger != nil {
			wp.logger.Log("", "Critic auto-fix failed on attempt %d/%d: %v", attempt, criticAutoFixBudget, err)
		}
		wp.eventCh <- WorkerEvent{
			Role: "critic-fix",
			Line: fmt.Sprintf("⚠ Critic auto-fix failed after %s: %v", elapsed, err),
		}
		return false
	}
	close(progressStop)

	elapsed := time.Since(startedAt).Round(time.Second)
	snapshotAfter, afterErr := captureCriticArtifactSnapshot(wp.missionDir)
	changedArtifacts := []string{}
	invalidatedPhases := []string{}
	fullFallback := false

	switch {
	case beforeErr != nil || afterErr != nil:
		fullFallback = true
		if wp.logger != nil {
			wp.logger.Log("", "Critic auto-fix snapshot fallback (before_err=%v after_err=%v)", beforeErr, afterErr)
		}
	default:
		changed, err := changedCriticArtifacts(snapshotBefore, snapshotAfter)
		if err != nil {
			fullFallback = true
			if wp.logger != nil {
				wp.logger.Log("", "Critic auto-fix changed-artifact diff failed: %v", err)
			}
		} else {
			changedArtifacts = changed
			phases, fallback := determineCriticPhaseInvalidation(changedArtifacts)
			fullFallback = fallback
			invalidatedPhases = phases
		}
	}

	if fullFallback {
		invalidatedPhases = []string{"A", "B", "C"}
		if err := clearCriticPhaseExecutionState(wp.missionDir); err != nil {
			if wp.logger != nil {
				wp.logger.Log("", "Could not clear critic phase cache after auto-fix fallback: %v", err)
			}
			wp.eventCh <- WorkerEvent{
				Role: "critic-fix",
				Line: fmt.Sprintf("⚠ Could not clear critic phase cache after auto-fix fallback: %v", err),
			}
		}
	} else if len(invalidatedPhases) > 0 {
		if err := invalidateCriticPhaseExecutionState(wp.missionDir, invalidatedPhases); err != nil {
			fullFallback = true
			invalidatedPhases = []string{"A", "B", "C"}
			if wp.logger != nil {
				wp.logger.Log("", "Could not invalidate critic phase cache selectively: %v", err)
			}
			wp.eventCh <- WorkerEvent{
				Role: "critic-fix",
				Line: fmt.Sprintf("⚠ Could not invalidate critic cache selectively: %v (fallback full rerun)", err),
			}
			if err := clearCriticPhaseExecutionState(wp.missionDir); err != nil && wp.logger != nil {
				wp.logger.Log("", "Could not clear critic phase cache after selective invalidation failure: %v", err)
			}
		}
	}

	phaseLabel := "none"
	if len(invalidatedPhases) > 0 {
		phaseLabel = strings.Join(invalidatedPhases, ",")
	}
	artifactLabel := "none"
	if len(changedArtifacts) > 0 {
		artifactLabel = strings.Join(changedArtifacts, ", ")
	}

	if wp.logger != nil {
		wp.logger.Log("", "Critic auto-fix completed in %s (changed_artifacts=%s invalidated_phases=%s fallback_full=%t)", elapsed, artifactLabel, phaseLabel, fullFallback)
	}
	wp.eventCh <- WorkerEvent{
		Role: "critic-fix",
		Line: fmt.Sprintf("✓ Critic auto-fix completed in %s", elapsed),
	}
	wp.eventCh <- WorkerEvent{
		Role: "critic-fix",
		Line: fmt.Sprintf("ℹ Changed artifacts: %s", artifactLabel),
	}
	wp.eventCh <- WorkerEvent{
		Role: "critic-fix",
		Line: fmt.Sprintf("ℹ Invalidated critic phases: %s", phaseLabel),
	}

	wp.clearCriticPhaseSessions(invalidatedPhases)
	if fullFallback {
		wp.eventCh <- WorkerEvent{
			Role: "critic",
			Line: "✓ Critic auto-fix applied — fallback rerun full critic gate (A+B+C)",
		}
		return true
	}

	if len(invalidatedPhases) == 0 {
		wp.eventCh <- WorkerEvent{
			Role: "critic",
			Line: "✓ Critic auto-fix applied — rerunning critic with cache reuse",
		}
		return true
	}

	wp.eventCh <- WorkerEvent{
		Role: "critic",
		Line: fmt.Sprintf("✓ Critic auto-fix applied — rerunning critic with selective invalidation (%s)", phaseLabel),
	}
	return true
}

func (wp *WorkerPool) decideCriticRecoveryAction(rootID string, report *CriticReport) (criticRecoveryAction, int, string) {
	rootID = normalizeRecoveryRootID(rootID)
	signature := "critic:" + rootID
	if report != nil {
		signature = report.NormalizedFailureSignature("critic:" + rootID)
	}
	_, _ = wp.recordFailureSignature(rootID, signature)

	if isCriticTransientReport(report) {
		transientAttempt := wp.bumpCriticTransientAttempt(rootID)
		if transientAttempt <= criticTransientBudget {
			return criticRecoveryRetryTransient, transientAttempt, signature
		}
	}

	structuralAttempt := wp.bumpCriticStructuralAttempt(rootID)
	if structuralAttempt <= criticStructuralBudget {
		return criticRecoveryRetryStructural, structuralAttempt, signature
	}

	bypassCount := wp.recordCriticBypass(rootID)
	return criticRecoveryBypass, bypassCount, signature
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
	resumeSession := wp.autonomousSessionID("worker", feature.ID)
	var cmd *exec.Cmd
	if resumeSession != "" {
		cmd = StartClaude(
			"An interrupted worker session exists for this feature. Continue implementation from where you left off.",
			wp.projectDir, wp.verbose, ch,
			"--resume", resumeSession,
		)
	} else {
		cmd = StartClaude(prompt, wp.projectDir, wp.verbose, ch)
	}

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
			if msg.SessionID != "" {
				wp.rememberAutonomousSession("worker", feature.ID, msg.SessionID)
			}

			wp.mu.Lock()
			w.EndTime = time.Now()
			elapsed := w.EndTime.Sub(w.StartTime).Round(time.Second)

			if msg.Err != nil {
				transient := isTransientError(msg.Err)
				sessionID := strings.TrimSpace(msg.SessionID)
				if sessionID == "" {
					sessionID = wp.autonomousSessionID("worker", feature.ID)
				}

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
				wp.mu.Unlock()

				wp.logger.Log(feature.ID, "Worker completed in %s — running mandatory quality gate (lint + tests)", elapsed)
				wp.eventCh <- WorkerEvent{
					FeatureID: feature.ID,
					Line:      fmt.Sprintf("◎ %s worker done in %s — running quality gate (lint + tests)...", feature.ID, elapsed),
				}

				gateResult := RunQualityGate(wp.projectDir, DetectQualityCommands(wp.projectDir))
				wp.emitQualityGateEvents(feature.ID, gateResult)
				if !gateResult.Passed {
					if wp.retryWorkerAfterQualityGate(feature, contract, gateResult) {
						return
					}

					wp.mu.Lock()
					w.Status = WorkerFailed
					wp.mu.Unlock()
					wp.updateFeatureStatus(feature.ID, "blocked")

					wp.mu.Lock()
					attempt := wp.retries[feature.ID]
					wp.mu.Unlock()
					wp.logger.Log(feature.ID, "FAILED quality gate after %d attempts", attempt)
					wp.eventCh <- WorkerEvent{
						FeatureID: feature.ID,
						Done:      true,
						Line:      fmt.Sprintf("✕ %s quality gate failed after %d attempts", feature.ID, attempt),
					}
					wp.advanceIfPhaseComplete(feature.Phase, contract)
					return
				}

				wp.mu.Lock()
				w.Status = WorkerAwaitingValidation
				wp.mu.Unlock()

				wp.logger.Log(feature.ID, "Quality gate passed — awaiting validation")
				wp.updateFeatureStatus(feature.ID, "awaiting_validation")

				wp.eventCh <- WorkerEvent{
					FeatureID: feature.ID,
					Line:      fmt.Sprintf("✓ %s quality gate passed — awaiting validation...", feature.ID),
				}

				go wp.runValidator(feature, contract)
			}
			return
		}
	}
}

func (wp *WorkerPool) emitQualityGateEvents(featureID string, result QualityGateResult) {
	lintStatus := "FAIL"
	if result.LintPassed {
		lintStatus = "PASS"
	}
	testStatus := "FAIL"
	if result.TestPassed {
		testStatus = "PASS"
	}

	wp.eventCh <- WorkerEvent{
		FeatureID: featureID,
		Line:      fmt.Sprintf("Quality gate summary — lint: %s, tests: %s", lintStatus, testStatus),
	}

	logRun := func(run QualityCommandRun) {
		status := "FAIL"
		if run.Passed {
			status = "PASS"
		}
		wp.logger.Log(
			featureID,
			"[QUALITY] %s %s command=%q scope=%s source=%s duration_ms=%d",
			run.Kind,
			status,
			run.Command,
			run.Scope,
			run.Source,
			run.DurationMs,
		)
		wp.eventCh <- WorkerEvent{
			FeatureID: featureID,
			Line:      fmt.Sprintf("  [%s/%s] %s", run.Kind, status, run.Command),
		}
	}

	for _, run := range result.LintRuns {
		logRun(run)
	}
	for _, run := range result.TestRuns {
		logRun(run)
	}
	for _, note := range result.Notes {
		wp.logger.Log(featureID, "[QUALITY] NOTE: %s", note)
		wp.eventCh <- WorkerEvent{
			FeatureID: featureID,
			Line:      "  [quality/note] " + note,
		}
	}
}

func (wp *WorkerPool) retryWorkerAfterQualityGate(feature Feature, contract string, gateResult QualityGateResult) bool {
	failureContext := BuildQualityGateFailureContext(gateResult)

	wp.mu.Lock()
	w, ok := wp.workers[feature.ID]
	if !ok {
		wp.mu.Unlock()
		return false
	}
	if strings.TrimSpace(w.FailureContext) != "" {
		w.FailureContext = strings.TrimSpace(w.FailureContext) + "\n\n" + failureContext
	} else {
		w.FailureContext = failureContext
	}
	wp.retries[feature.ID]++
	attempt := wp.retries[feature.ID]
	canRetry := !wp.stopped && attempt <= wp.maxRetries
	wp.mu.Unlock()

	if !canRetry {
		return false
	}

	backoff := time.Duration(attempt) * 3 * time.Second
	wp.logger.Log(feature.ID, "Quality gate failed — retrying worker (%d/%d) in %s", attempt, wp.maxRetries, backoff)
	wp.eventCh <- WorkerEvent{
		FeatureID: feature.ID,
		Line:      fmt.Sprintf("⚠ %s quality gate failed, retrying worker (%d/%d)...", feature.ID, attempt, wp.maxRetries),
	}

	time.Sleep(backoff)

	wp.mu.Lock()
	if wp.stopped {
		wp.mu.Unlock()
		return false
	}
	w = wp.workers[feature.ID]
	w.Status = WorkerRunning
	w.StartTime = time.Now()
	retryCtx := w.FailureContext
	wp.mu.Unlock()

	newCh := make(chan ClaudeStreamMsg, 64)
	sessionID := wp.autonomousSessionID("worker", feature.ID)
	var cmd *exec.Cmd
	if sessionID != "" {
		cmd = StartClaude(
			"The quality gate failed (lint/tests). Continue implementing this feature, fix the failures, and only stop after lint + tests pass.",
			wp.projectDir, wp.verbose, newCh,
			"--resume", sessionID,
		)
	} else {
		specPath := filepath.Dir(wp.missionDir)
		knowledge := wp.freshKnowledge()
		prompt := BuildWorkerPrompt(feature, nil, contract, knowledge, specPath, wp.projectDir, retryCtx)
		cmd = StartClaude(prompt, wp.projectDir, wp.verbose, newCh)
	}

	wp.mu.Lock()
	if current, ok := wp.workers[feature.ID]; ok {
		current.cmd = cmd
	}
	wp.mu.Unlock()

	go wp.runWorkerLoop(feature, newCh, contract)
	return true
}

func (wp *WorkerPool) runValidator(feature Feature, contract string) {
	wp.runValidatorWithResume(feature, contract, "")
}

func validatorClaudeArgs() []string {
	// Keep validator sandboxed to read/observe tools, but allow browser MCPs
	// so black-box UI validation can run against real websites.
	return []string{
		"--max-turns", "50",
		"--chrome",
		"--allowedTools", strings.Join([]string{
			"Read",
			"Bash",
			"Glob",
			"Grep",
			"WebFetch",
			"WebSearch",
			"mcp__cursor-ide-browser__browser_tabs",
			"mcp__cursor-ide-browser__browser_navigate",
			"mcp__cursor-ide-browser__browser_lock",
			"mcp__cursor-ide-browser__browser_snapshot",
			"mcp__cursor-ide-browser__browser_take_screenshot",
			"mcp__cursor-ide-browser__browser_click",
			"mcp__cursor-ide-browser__browser_type",
			"mcp__cursor-ide-browser__browser_fill",
			"mcp__cursor-ide-browser__browser_select_option",
			"mcp__cursor-ide-browser__browser_press_key",
			"mcp__cursor-ide-browser__browser_scroll",
			"mcp__cursor-ide-browser__browser_drag",
			"mcp__cursor-ide-browser__browser_highlight",
			"mcp__cursor-ide-browser__browser_get_bounding_box",
			"mcp__cursor-ide-browser__browser_cdp",
			"mcp__plugin-chrome-devtools-mcp-chrome-devtools__*",
		}, ","),
	}
}

func (wp *WorkerPool) runValidatorWithResume(feature Feature, contract, resumeSession string) {
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
	resumeSession = strings.TrimSpace(resumeSession)
	if resumeSession == "" {
		resumeSession = wp.autonomousSessionID("validator", feature.ID)
	}
	validatorArgs := validatorClaudeArgs()
	var cmd *exec.Cmd
	if resumeSession != "" {
		resumeArgs := append([]string{"--resume", resumeSession}, validatorArgs...)
		cmd = StartClaude(
			"A previous validator run was interrupted. Continue from where you left off and output the final JSON report only.",
			wp.projectDir, wp.verbose, ch,
			resumeArgs...,
		)
	} else {
		cmd = StartClaude(prompt, wp.projectDir, wp.verbose, ch, validatorArgs...)
	}

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
			if msg.SessionID != "" {
				wp.rememberAutonomousSession("validator", feature.ID, msg.SessionID)
			}
			if msg.Err != nil {
				resumeID := strings.TrimSpace(msg.SessionID)
				if resumeID == "" {
					resumeID = wp.autonomousSessionID("validator", feature.ID)
				}
				if isTransientError(msg.Err) {
					wp.mu.Lock()
					wp.transientRetries[feature.ID+"_val"]++
					tAttempt := wp.transientRetries[feature.ID+"_val"]
					wp.mu.Unlock()
					if tAttempt <= maxTransientRetries {
						backoff := time.Duration(tAttempt) * 5 * time.Second
						wp.logger.Log(feature.ID, "Validator transient error: %v — retrying (%d/%d) in %s", msg.Err, tAttempt, maxTransientRetries, backoff)
						label := ""
						if resumeID != "" {
							label = " with session resume"
						}
						wp.eventCh <- WorkerEvent{
							FeatureID: feature.ID,
							Role:      "validator",
							Line:      fmt.Sprintf("⚠ %s validator socket error, retrying (%d/%d)%s...", feature.ID, tAttempt, maxTransientRetries, label),
						}
						time.Sleep(backoff)
						go wp.runValidatorWithResume(feature, contract, resumeID)
						return
					}
				}
				if wp.retryValidator(feature, contract, fmt.Sprintf("error: %v", msg.Err), resumeID) {
					return
				}
				wp.logger.Log(feature.ID, "Validator error after retries: %v — sending to refinement with synthetic report", msg.Err)
				report := syntheticValidatorReport(feature, "validator error", fmt.Sprintf("%v", msg.Err))
				wp.goToRefinement(feature, contract, report, fmt.Sprintf("⚠ %s validator unavailable (%v) — refining anyway...", feature.ID, msg.Err))
				return
			}
			resultText = msg.Result
			// #region agent log
			emitDebugLog(debugRunIDValidatorVerdictV1, "H3", "internal/worker.go:runValidatorWithResume:msgDone", "validator_done_result_captured", map[string]any{
				"featureID":          feature.ID,
				"resultLen":          len(strings.TrimSpace(resultText)),
				"containsVerdictKey": strings.Contains(strings.ToUpper(resultText), "\"VERDICT\""),
				"containsPASS":       strings.Contains(strings.ToUpper(resultText), "PASS"),
				"containsFAIL":       strings.Contains(strings.ToUpper(resultText), "FAIL"),
			})
			// #endregion
		}
	}

	// #region agent log
	emitDebugLog(debugRunIDValidatorVerdictV1, "H2", "internal/worker.go:runValidatorWithResume:preParse", "validator_pre_parse_snapshot", map[string]any{
		"featureID":           feature.ID,
		"resultLen":           len(strings.TrimSpace(resultText)),
		"startsWithBrace":     strings.HasPrefix(strings.TrimSpace(resultText), "{"),
		"hasCodeFence":        strings.Contains(resultText, "```"),
		"containsFinalOutput": strings.Contains(strings.ToLower(resultText), "final output"),
	})
	// #endregion

	report := ParseValidatorReport(resultText)
	// #region agent log
	parsedVerdict := "<nil>"
	if report != nil {
		parsedVerdict = report.Verdict
	}
	emitDebugLog(debugRunIDValidatorVerdictV1, "H1", "internal/worker.go:runValidatorWithResume:postParse", "validator_post_parse_result", map[string]any{
		"featureID":     feature.ID,
		"reportNil":     report == nil,
		"parsedVerdict": parsedVerdict,
		"rawHasPASS":    strings.Contains(strings.ToUpper(resultText), "PASS"),
		"rawHasFAIL":    strings.Contains(strings.ToUpper(resultText), "FAIL"),
	})
	// #endregion
	if report != nil {
		if tainted {
			report.Notes = append(report.Notes, "TAINTED: validator accessed worker output — black-box rule violated, results may be biased")
		}
		normalizeValidatorVerdict(report)
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
		if wp.retryValidator(feature, contract, "unparseable output", wp.autonomousSessionID("validator", feature.ID)) {
			return
		}
		wp.logger.Log(feature.ID, "Validator returned unparseable output after retries — sending to refinement with synthetic report")
		synth := syntheticValidatorReport(feature, "unparseable output", "validator did not return a valid JSON report after retries")
		wp.goToRefinement(feature, contract, synth, fmt.Sprintf("⚠ %s validator output unreadable — refining anyway...", feature.ID))
		return
	}

	// #region agent log
	switchVerdict := "<nil>"
	if report != nil {
		switchVerdict = strings.TrimSpace(report.Verdict)
	}
	emitDebugLog(debugRunIDValidatorVerdictV1, "H4", "internal/worker.go:runValidatorWithResume:switch", "validator_switch_dispatch", map[string]any{
		"featureID":     feature.ID,
		"switchVerdict": switchVerdict,
		"switchUpper":   strings.ToUpper(switchVerdict),
		"hasRefinement": strings.EqualFold(strings.ToUpper(switchVerdict), "FAIL") || !strings.EqualFold(strings.ToUpper(switchVerdict), "PASS"),
	})
	// #endregion

	switch report.Verdict {
	case "PASS":
		wp.logger.Log(feature.ID, "Validator PASSED")
		wp.mu.Lock()
		wp.workers[feature.ID].Status = WorkerDone
		wp.workers[feature.ID].EndTime = time.Now()
		wp.mu.Unlock()
		wp.updateFeatureStatus(feature.ID, "done")
		rootID := wp.resolveRootFeatureID(feature.ID)
		if rootID != "" {
			wp.clearRecoveryLevel(rootID)
		}
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

func normalizeValidatorVerdict(report *ValidatorReport) {
	if report == nil {
		return
	}

	report.Verdict = strings.ToUpper(strings.TrimSpace(report.Verdict))
	for i := range report.Assertions {
		report.Assertions[i].Result = strings.ToUpper(strings.TrimSpace(report.Assertions[i].Result))
	}

	if report.Verdict != "PASS" {
		return
	}

	for _, assertion := range report.Assertions {
		if assertion.Result == "PASS" {
			continue
		}
		report.Verdict = "FAIL"
		report.Notes = append(report.Notes,
			fmt.Sprintf("VALIDATOR_VERDICT_OVERRIDDEN: top-level verdict PASS but assertion %s returned %s", assertion.ID, assertion.Result),
		)
		return
	}
}

func (wp *WorkerPool) goToRefinement(feature Feature, contract string, report ValidatorReport, displayLine string) {
	// #region agent log
	emitDebugLog(debugRunIDValidatorVerdictV1, "H5", "internal/worker.go:goToRefinement:entry", "go_to_refinement_called", map[string]any{
		"featureID":     feature.ID,
		"reportVerdict": report.Verdict,
		"displayLine":   displayLine,
	})
	// #endregion

	wp.persistReport(feature.ID, "validator", &report)

	wp.logger.Log(feature.ID, "Sending to refinement (verdict=%s)", report.Verdict)

	wp.mu.Lock()
	if sourceWorker, ok := wp.workers[feature.ID]; ok {
		sourceWorker.Status = WorkerRefining
	} else {
		wp.workers[feature.ID] = &FeatureWorker{
			Feature: feature,
			Status:  WorkerRefining,
		}
	}
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
	wp.runRefinementWithResume(feature, report, contract, "")
}

func (wp *WorkerPool) runRefinementWithResume(feature Feature, report ValidatorReport, contract, resumeSession string) {
	wp.mu.Lock()
	if wp.stopped {
		wp.mu.Unlock()
		return
	}
	if _, exists := wp.workers[feature.ID]; !exists {
		wp.workers[feature.ID] = &FeatureWorker{
			Feature: feature,
			Status:  WorkerRefining,
		}
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
			Line:      fmt.Sprintf("✕ %s refinement limit (%d rounds) — escalating autonomous recovery", feature.ID, wp.maxRefinements),
		}
		wp.advanceIfPhaseComplete(feature.Phase, contract)
		return
	}
	wp.mu.Unlock()

	// #region agent log
	emitDebugLog(debugRunIDRefineStuckV1, "H1", "internal/worker.go:runRefinementWithResume:entry", "refinement_round_started", map[string]any{
		"featureID":       feature.ID,
		"featureFixes":    feature.Fixes,
		"featurePhase":    feature.Phase,
		"round":           round,
		"maxRefinements":  wp.maxRefinements,
		"resumeProvided":  strings.TrimSpace(resumeSession) != "",
		"reportFeatureID": report.FeatureID,
		"reportVerdict":   report.Verdict,
	})
	// #endregion

	wp.logger.Log(feature.ID, "Refinement round %d/%d", round, wp.maxRefinements)
	wp.eventCh <- WorkerEvent{
		FeatureID: feature.ID,
		Role:      "refinement",
		Line:      fmt.Sprintf("⟳ %s refinement round %d/%d", feature.ID, round, wp.maxRefinements),
	}

	specDir := filepath.Dir(wp.missionDir)
	prompt := BuildRefinementPrompt(feature, report, wp.missionDir, specDir)
	ch := make(chan ClaudeStreamMsg, 64)
	resumeSession = strings.TrimSpace(resumeSession)
	if resumeSession == "" {
		resumeSession = wp.autonomousSessionID("refinement", feature.ID)
	}
	var cmd *exec.Cmd
	if resumeSession != "" {
		cmd = StartClaude(
			"A previous refinement run was interrupted. Continue from where you left off and output ONLY the JSON array of fix features.",
			wp.projectDir, wp.verbose, ch,
			"--resume", resumeSession,
			"--max-turns", "15",
		)
	} else {
		cmd = StartClaude(prompt, wp.projectDir, wp.verbose, ch, "--max-turns", "15")
	}

	wp.mu.Lock()
	if sourceWorker, ok := wp.workers[feature.ID]; ok {
		sourceWorker.cmd = cmd
	}
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
			if msg.SessionID != "" {
				wp.rememberAutonomousSession("refinement", feature.ID, msg.SessionID)
			}
			if msg.Err != nil {
				resumeID := strings.TrimSpace(msg.SessionID)
				if resumeID == "" {
					resumeID = wp.autonomousSessionID("refinement", feature.ID)
				}
				if isTransientError(msg.Err) {
					wp.mu.Lock()
					wp.transientRetries[feature.ID+"_ref"]++
					tAttempt := wp.transientRetries[feature.ID+"_ref"]
					wp.mu.Unlock()
					if tAttempt <= maxTransientRetries {
						backoff := time.Duration(tAttempt) * 5 * time.Second
						wp.logger.Log(feature.ID, "Refinement transient error: %v — retrying (%d/%d) in %s", msg.Err, tAttempt, maxTransientRetries, backoff)
						label := ""
						if resumeID != "" {
							label = " with session resume"
						}
						wp.eventCh <- WorkerEvent{
							FeatureID: feature.ID,
							Role:      "refinement",
							Line:      fmt.Sprintf("⚠ %s refinement socket error, retrying (%d/%d)%s...", feature.ID, tAttempt, maxTransientRetries, label),
						}
						time.Sleep(backoff)
						go wp.runRefinementWithResume(feature, report, contract, resumeID)
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
	// #region agent log
	emitDebugLog(debugRunIDRefineStuckV1, "H1", "internal/worker.go:runRefinementWithResume:parsed", "refinement_parse_result", map[string]any{
		"featureID":      feature.ID,
		"parsedFixCount": len(fixes),
		"resultLen":      len(resultText),
	})
	// #endregion
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

	rootID := wp.resolveRootFeatureID(feature.ID)
	if rootID == "" {
		rootID = feature.ID
	}

	fixes = wp.rewriteFixFeaturesForRoot(rootID, feature.ID, fixes)
	// #region agent log
	emitDebugLog(debugRunIDRefineStuckV1, "H2", "internal/worker.go:runRefinementWithResume:rewritten", "refinement_rewrite_result", map[string]any{
		"featureID":         feature.ID,
		"rootID":            rootID,
		"rewrittenFixCount": len(fixes),
		"fixIDs":            featureIDs(fixes),
	})
	// #endregion
	if len(fixes) == 0 {
		wp.logger.Log(feature.ID, "Refinement rewrite produced no usable fixes — marking blocked")
		wp.mu.Lock()
		if sourceWorker, ok := wp.workers[feature.ID]; ok {
			sourceWorker.Status = WorkerFailed
			sourceWorker.EndTime = time.Now()
		}
		wp.mu.Unlock()
		wp.updateFeatureStatus(rootID, "blocked")
		wp.eventCh <- WorkerEvent{
			FeatureID: feature.ID,
			Role:      "refinement",
			Done:      true,
			Line:      fmt.Sprintf("✕ %s refinement produced no valid fixes after rewrite", feature.ID),
		}
		wp.advanceIfPhaseComplete(feature.Phase, contract)
		return
	}

	// Invalid fix leaves should not accumulate forever; if we are refining a fix
	// feature, discard it before replaying replacement fixes for the same root.
	if feature.Fixes != "" {
		if err := wp.discardGeneratedFixes([]Feature{feature}); err != nil {
			wp.logger.Log(feature.ID, "WARN: failed to discard invalid fix entry before replay: %v", err)
		}
	}

	if err := AddFixFeatures(wp.missionDir, fixes, rootID, &wp.fileMu); err != nil {
		wp.logger.Log(feature.ID, "Failed to write fix features: %v", err)
		wp.mu.Lock()
		if sourceWorker, ok := wp.workers[feature.ID]; ok {
			sourceWorker.Status = WorkerFailed
			sourceWorker.EndTime = time.Now()
		}
		wp.mu.Unlock()
		wp.updateFeatureStatus(rootID, "blocked")
		wp.eventCh <- WorkerEvent{
			FeatureID: feature.ID,
			Role:      "refinement",
			Done:      true,
			Line:      fmt.Sprintf("✕ %s refinement failed to persist fixes", feature.ID),
		}
		wp.advanceIfPhaseComplete(feature.Phase, contract)
		return
	}

	wp.mu.Lock()
	usedAttempts := wp.fixAttemptsByRoot[rootID]
	projectedAttempts := usedAttempts + len(fixes)
	limit := wp.maxFixAttempts
	if projectedAttempts > limit {
		if sourceWorker, ok := wp.workers[feature.ID]; ok {
			sourceWorker.Status = WorkerFailed
			sourceWorker.EndTime = time.Now()
		}
		for _, fix := range fixes {
			wp.workers[fix.ID] = &FeatureWorker{
				Feature: fix,
				Status:  WorkerFailed,
			}
		}
		wp.mu.Unlock()

		wp.updateFeatureStatus(rootID, "blocked")
		for _, fix := range fixes {
			wp.updateFeatureStatus(fix.ID, "blocked")
		}

		wp.logger.Log(feature.ID, "Fix autopilot limit reached for %s: %d/%d (new fixes=%d)", rootID, usedAttempts, limit, len(fixes))
		wp.eventCh <- WorkerEvent{
			FeatureID: feature.ID,
			Role:      "refinement",
			Done:      true,
			Line:      fmt.Sprintf("✕ %s fix autopilot limit reached for %s (%d/%d) — escalating autonomous recovery", feature.ID, rootID, usedAttempts, limit),
		}
		wp.advanceIfPhaseComplete(feature.Phase, contract)
		return
	}
	wp.fixAttemptsByRoot[rootID] = projectedAttempts

	if sourceWorker, ok := wp.workers[feature.ID]; ok {
		sourceWorker.Status = WorkerFailed
		sourceWorker.EndTime = time.Now()
	}
	for _, fix := range fixes {
		wp.workers[fix.ID] = &FeatureWorker{
			Feature: fix,
			Status:  WorkerPending,
		}
		wp.phases[fix.Phase] = append(wp.phases[fix.Phase], fix.ID)
	}
	wp.mu.Unlock()

	wp.updateFeatureStatus(rootID, "blocked")

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

	go wp.runFixCriticAndStart(feature, fixes, contract)
}

func (wp *WorkerPool) rewriteFixFeaturesForRoot(rootID, sourceID string, fixes []Feature) []Feature {
	rootID = strings.TrimSpace(rootID)
	sourceID = strings.TrimSpace(sourceID)
	if rootID == "" || len(fixes) == 0 {
		return fixes
	}

	// Canonicalize generated fix IDs per root to prevent nested id explosion
	// (e.g. F01-fix-1-fix-1-fix-1). Replays reuse the same stable slots.
	idMap := make(map[string]string, len(fixes))
	rewritten := make([]Feature, 0, len(fixes))
	for i, fix := range fixes {
		newID := fmt.Sprintf("%s-fix-%02d", rootID, i+1)
		oldID := strings.TrimSpace(fix.ID)
		if oldID != "" {
			idMap[oldID] = newID
		}

		fix.ID = newID
		fix.Fixes = rootID
		if fix.Status == "" {
			fix.Status = "pending"
		}
		if fix.Phase < 0 {
			fix.Phase = 0
		}
		rewritten = append(rewritten, fix)
	}

	for i := range rewritten {
		rewrittenDeps := make([]string, 0, len(rewritten[i].DependsOn)+1)
		seen := make(map[string]struct{})
		for _, dep := range rewritten[i].DependsOn {
			dep = strings.TrimSpace(dep)
			if dep == "" {
				continue
			}
			if dep == sourceID {
				dep = rootID
			}
			if mapped, ok := idMap[dep]; ok {
				dep = mapped
			}
			if dep == rewritten[i].ID {
				continue
			}
			if _, exists := seen[dep]; exists {
				continue
			}
			seen[dep] = struct{}{}
			rewrittenDeps = append(rewrittenDeps, dep)
		}
		if len(rewrittenDeps) == 0 {
			rewrittenDeps = append(rewrittenDeps, rootID)
		}
		rewritten[i].DependsOn = rewrittenDeps
	}

	return rewritten
}

func (wp *WorkerPool) runFixCriticAndStart(source Feature, fixes []Feature, contract string) {
	criticCh := make(chan WorkerEvent, 64)
	go RunFixCriticGate(wp.projectDir, wp.missionDir, fixes, wp.verbose, criticCh)

	var passed bool
	var criticReport *CriticReport
	for ev := range criticCh {
		wp.eventCh <- ev
		if ev.Done && ev.Role == "critic" {
			passed = ev.Verdict == "PASS"
			criticReport = ev.CriticReport
			break
		}
	}

	// #region agent log
	emitDebugLog(debugRunIDRefineStuckV1, "H3", "internal/worker.go:runFixCriticAndStart:verdict", "fix_critic_completed", map[string]any{
		"sourceID":    source.ID,
		"sourceFixes": source.Fixes,
		"passed":      passed,
		"hasReport":   criticReport != nil,
		"fixIDs":      featureIDs(fixes),
	})
	// #endregion

	if !passed {
		if wp.autonomousRetryAfterFixCriticFailure(source, fixes, criticReport, contract) {
			return
		}

		wp.logger.Log("", "Fix critic gate failed — autonomous retries exhausted")
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
		wp.eventCh <- WorkerEvent{
			FeatureID: source.ID,
			Role:      "refinement",
			Line:      fmt.Sprintf("✕ %s fix critic failed after autonomous retries", source.ID),
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

func (wp *WorkerPool) autonomousRetryAfterFixCriticFailure(source Feature, fixes []Feature, criticReport *CriticReport, contract string) bool {
	if source.ID == "" {
		return false
	}

	rootID := wp.resolveRootFeatureID(source.ID)
	if rootID == "" {
		rootID = source.ID
	}

	signature := "fix-critic:" + rootID
	if criticReport != nil {
		signature = criticReport.NormalizedFailureSignature("fix-critic:" + rootID)
	}
	count, level := wp.recordFailureSignature(rootID, signature)
	// #region agent log
	emitDebugLog(debugRunIDRefineStuckV1, "H3", "internal/worker.go:autonomousRetryAfterFixCriticFailure:decision", "fix_critic_failure_recovery_decision", map[string]any{
		"sourceID":       source.ID,
		"rootID":         rootID,
		"signature":      signature,
		"count":          count,
		"level":          level,
		"maxRefinements": wp.maxRefinements,
		"willReplay":     count <= wp.maxRefinements,
	})
	// #endregion
	if count > wp.maxRefinements {
		wp.logger.Log(source.ID, "Fix critic failure signature repeated %d times (level %d) for %s; escalation required", count, level, rootID)
		return false
	}

	if err := wp.discardGeneratedFixes(fixes); err != nil {
		wp.logger.Log(source.ID, "Failed to discard critic-rejected fixes: %v", err)
		return false
	}
	wp.updateFeatureStatus(rootID, "blocked")

	report, err := wp.loadValidatorReport(source.ID)
	if err != nil || report.FeatureID == "" {
		report = syntheticValidatorReport(source, "fix critic failure", "Generated fix features were rejected by critic decomposition checks.")
	}
	report.Verdict = "BLOCKED"
	report.Notes = append(report.Notes,
		"FIX_CRITIC_FAILURE: Generated fix features failed critic gate; retrying refinement automatically.",
	)
	if criticReport != nil {
		for _, finding := range criticReport.BlockingFailures() {
			note := fmt.Sprintf("FIX_CRITIC_BLOCKING: %s", finding.Criterion)
			if finding.Note != "" {
				note = fmt.Sprintf("%s — %s", note, finding.Note)
			}
			report.Notes = append(report.Notes, note)
		}
	}

	wp.logger.Log(source.ID, "Fix critic failed for generated fixes — replaying same root (%s), attempt %d", rootID, count)
	wp.eventCh <- WorkerEvent{
		FeatureID: source.ID,
		Role:      "refinement",
		Line:      fmt.Sprintf("⚠ %s fix critic rejected generated fixes — replaying root %s (recovery L%d)", source.ID, rootID, level),
	}
	go wp.runRefinementWithResume(source, report, contract, wp.autonomousSessionID("refinement", source.ID))
	return true
}

func (wp *WorkerPool) discardGeneratedFixes(fixes []Feature) error {
	if len(fixes) == 0 {
		return nil
	}

	ids := make(map[string]struct{}, len(fixes))
	for _, fix := range fixes {
		if fix.ID != "" {
			ids[fix.ID] = struct{}{}
		}
	}

	wp.mu.Lock()
	for id := range ids {
		delete(wp.workers, id)
		delete(wp.tainted, id)
	}
	for phase, phaseIDs := range wp.phases {
		filtered := make([]string, 0, len(phaseIDs))
		for _, id := range phaseIDs {
			if _, remove := ids[id]; remove {
				continue
			}
			filtered = append(filtered, id)
		}
		if len(filtered) == 0 {
			delete(wp.phases, phase)
			continue
		}
		wp.phases[phase] = filtered
	}
	wp.mu.Unlock()

	wp.fileMu.Lock()
	defer wp.fileMu.Unlock()

	path := filepath.Join(wp.missionDir, "features.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var manifest FeaturesManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return err
	}

	filteredFixes := make([]Feature, 0, len(manifest.FixFeatures))
	for _, fix := range manifest.FixFeatures {
		if _, remove := ids[fix.ID]; remove {
			continue
		}
		filteredFixes = append(filteredFixes, fix)
	}
	manifest.FixFeatures = filteredFixes

	out, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, out, 0o644)
}

func (wp *WorkerPool) loadValidatorReport(featureID string) (ValidatorReport, error) {
	var report ValidatorReport
	path := filepath.Join(wp.missionDir, "runs", featureID+"-validator.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return report, err
	}
	if err := json.Unmarshal(data, &report); err != nil {
		return report, err
	}
	return report, nil
}

func (wp *WorkerPool) retryValidator(feature Feature, contract string, reason string, resumeSession string) bool {
	wp.mu.Lock()
	wp.validatorRetries[feature.ID]++
	attempt := wp.validatorRetries[feature.ID]
	canRetry := attempt <= wp.maxValidatorRetries && !wp.stopped
	wp.mu.Unlock()

	if !canRetry {
		return false
	}

	wp.logger.Log(feature.ID, "Validator %s — retrying (%d/%d)", reason, attempt, wp.maxValidatorRetries)
	label := ""
	if strings.TrimSpace(resumeSession) != "" {
		label = " (resuming session)"
	}
	wp.eventCh <- WorkerEvent{
		FeatureID: feature.ID,
		Role:      "validator",
		Line:      fmt.Sprintf("⚠ %s validator %s — retrying (%d/%d)%s...", feature.ID, reason, attempt, wp.maxValidatorRetries, label),
	}

	time.Sleep(time.Duration(attempt) * 2 * time.Second)

	go wp.runValidatorWithResume(feature, contract, resumeSession)
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

func (wp *WorkerPool) unresolvedPhaseFailures(phase int) ([]string, map[string][]string) {
	wp.mu.Lock()
	defer wp.mu.Unlock()

	byID := make(map[string]Feature, len(wp.workers))
	for id, worker := range wp.workers {
		byID[id] = worker.Feature
	}

	var failedIDs []string
	byRoot := make(map[string][]string)
	for _, id := range wp.phases[phase] {
		w, ok := wp.workers[id]
		if !ok {
			continue
		}
		if w.Status != WorkerFailed || wp.isEffectivelyDoneLocked(id) {
			continue
		}
		failedIDs = append(failedIDs, id)
		rootID := resolveRootFeatureIDInMap(byID, id)
		if rootID == "" {
			rootID = id
		}
		byRoot[rootID] = append(byRoot[rootID], id)
	}
	sort.Strings(failedIDs)
	for rootID := range byRoot {
		sort.Strings(byRoot[rootID])
	}
	return failedIDs, byRoot
}

func (wp *WorkerPool) autoRecoverFailedPhase(phase int, contract string, failedByRoot map[string][]string) bool {
	for rootID, failedIDs := range failedByRoot {
		signature := fmt.Sprintf("phase-%d:%s", phase, strings.Join(failedIDs, ","))
		count, level := wp.recordFailureSignature(rootID, signature)
		wp.logger.Log("", "Autonomous ladder root=%s signature=%s count=%d level=L%d", rootID, signature, count, level)
	}

	if wp.retryFailedInPhase(phase, contract) {
		wp.eventCh <- WorkerEvent{
			Phase: phase,
			Line:  fmt.Sprintf("⟳ Phase %d unresolved failures detected — auto-retrying phase before escalation", phase),
		}
		return true
	}

	wp.mu.Lock()
	autoResetCount := wp.autonomousState.AutoResetCount
	wp.mu.Unlock()
	if autoResetCount >= maxAutoResetsPerRun {
		return false
	}

	if err := wp.autoFullResetAndRestart(phase, contract); err != nil {
		wp.logger.Log("", "Autonomous full reset failed: %v", err)
		wp.eventCh <- WorkerEvent{
			Phase: phase,
			Line:  fmt.Sprintf("✕ Autonomous full reset failed: %v", err),
		}
		return false
	}
	return true
}

func (wp *WorkerPool) autoFullResetAndRestart(phase int, contract string) error {
	wp.fileMu.Lock()
	path := filepath.Join(wp.missionDir, "features.json")
	data, err := os.ReadFile(path)
	if err != nil {
		wp.fileMu.Unlock()
		return err
	}
	var manifest FeaturesManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		wp.fileMu.Unlock()
		return err
	}
	for i := range manifest.Features {
		manifest.Features[i].Status = "pending"
		manifest.Features[i].Resolution = ""
		manifest.Features[i].ResolvedBy = ""
		manifest.Features[i].ResolvedAt = ""
		manifest.Features[i].Tainted = false
	}
	manifest.FixFeatures = nil
	out, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		wp.fileMu.Unlock()
		return err
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		wp.fileMu.Unlock()
		return err
	}
	wp.fileMu.Unlock()

	wp.mu.Lock()
	wp.workers = make(map[string]*FeatureWorker, len(manifest.Features))
	wp.phases = make(map[int][]string)
	for _, f := range manifest.Features {
		f.Status = "pending"
		wp.workers[f.ID] = &FeatureWorker{Feature: f, Status: WorkerPending}
		wp.phases[f.Phase] = append(wp.phases[f.Phase], f.ID)
	}
	wp.retries = make(map[string]int)
	wp.transientRetries = make(map[string]int)
	wp.validatorRetries = make(map[string]int)
	wp.refinementCount = make(map[string]int)
	wp.phaseRetries = make(map[int]int)
	wp.fixAttemptsByRoot = make(map[string]int)
	wp.tainted = make(map[string]bool)
	wp.autonomousState.AutoResetCount++
	snapshot := wp.autonomousState
	wp.mu.Unlock()

	if err := saveAutonomousRuntimeState(wp.missionDir, snapshot); err != nil {
		if wp.logger != nil {
			wp.logger.Log("", "WARN: failed to persist autonomous reset count: %v", err)
		}
	}

	minPhase := -1
	for p := range wp.phases {
		if minPhase == -1 || p < minPhase {
			minPhase = p
		}
	}

	wp.logger.Log("", "Autonomous full reset executed after phase %d failure; restarting at phase %d", phase, minPhase)
	wp.eventCh <- WorkerEvent{
		Phase: phase,
		Line:  fmt.Sprintf("⟳ Autonomous full reset executed — restarting mission from phase %d", minPhase),
	}
	if minPhase >= 0 {
		go wp.runPhase(minPhase, contract)
	}
	return nil
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

	// #region agent log
	emitDebugLog(debugRunIDRefineStuckV1, "H4", "internal/worker.go:advanceIfPhaseComplete:state", "advance_phase_state", map[string]any{
		"phase":             phase,
		"waitingInPhase":    len(waitingInPhase),
		"waitingFeatureIDs": featureIDs(waitingInPhase),
		"allTerminal":       allTerminal,
	})
	// #endregion

	if len(waitingInPhase) > 0 {
		wp.startReadyFeatures(waitingInPhase, siblingNames, contract)
	}

	if !allTerminal {
		return
	}

	failedIDs, failedByRoot := wp.unresolvedPhaseFailures(phase)
	// #region agent log
	emitDebugLog(debugRunIDRefineStuckV1, "H5", "internal/worker.go:advanceIfPhaseComplete:unresolved", "phase_unresolved_scan", map[string]any{
		"phase":       phase,
		"failedIDs":   failedIDs,
		"failedRoots": failedByRoot,
	})
	// #endregion
	if len(failedIDs) > 0 {
		if wp.autoRecoverFailedPhase(phase, contract, failedByRoot) {
			return
		}
		wp.logger.Log("", "Phase %d unresolved failures after autonomous ladder: %s", phase, strings.Join(failedIDs, ", "))
		wp.eventCh <- WorkerEvent{
			Phase: phase,
			Line:  fmt.Sprintf("✕ Phase %d unresolved after autonomous ladder: %s", phase, strings.Join(failedIDs, ", ")),
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

func emitDebugLog(runID, hypothesisID, location, message string, data map[string]any) {
	payload := map[string]any{
		"sessionId":    debugSessionID,
		"runId":        runID,
		"hypothesisId": hypothesisID,
		"location":     location,
		"message":      message,
		"data":         data,
		"timestamp":    time.Now().UnixMilli(),
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return
	}
	f, err := os.OpenFile(debugLogPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	_, _ = f.Write(append(b, '\n'))
	_ = f.Close()
}

func featureIDs(features []Feature) []string {
	if len(features) == 0 {
		return nil
	}
	ids := make([]string, 0, len(features))
	for _, f := range features {
		if strings.TrimSpace(f.ID) == "" {
			continue
		}
		ids = append(ids, f.ID)
	}
	return ids
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
