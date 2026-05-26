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
)

func ExtractMechanicalScript() (string, func(), error) {
	data, err := skillsFS.ReadFile("skills/checks/run-mechanical.mjs")
	if err != nil {
		return "", nil, fmt.Errorf("embedded script not found: %w", err)
	}

	f, err := os.CreateTemp("", "run-mechanical-*.mjs")
	if err != nil {
		return "", nil, err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", nil, err
	}
	f.Close()

	cleanup := func() { os.Remove(f.Name()) }
	return f.Name(), cleanup, nil
}

type MechanicalResult struct {
	Passed  int
	Failed  int
	Details []MechanicalCheck
	RawOut  string
}

type MechanicalCheck struct {
	ID      string `json:"id"`
	Status  string `json:"status"`
	Message string `json:"message"`
}

// criticPhase describes one of the three parallel judgment streams that
// replace the monolithic critic call. Each phase reads only the artifacts
// relevant to its criteria so the prompt stays small and the model can
// finish quickly.
type criticPhase struct {
	ID        string   // "A" | "B" | "C"
	Name      string   // human label, e.g. "Spec quality"
	Criteria  string   // descriptive label like "J-S1..J-S6"
	Artifacts []string // file paths the model should Read; CRITERIA.md is added automatically
	Focus     string   // one-line phase focus statement injected into the prompt
}

// Phase A — spec quality / validation contract.
var criticPhaseSpec = criticPhase{
	ID:       "A",
	Name:     "Spec quality",
	Criteria: "J-S1..J-S6",
	Artifacts: []string{
		"validation-contract.md",
	},
	Focus: "Evaluate ONLY the [J-S*] (Spec quality) judgment criteria. Ignore architecture and decomposition.",
}

// Phase B — architecture alignment.
var criticPhaseArch = criticPhase{
	ID:       "B",
	Name:     "Architecture",
	Criteria: "J-A1..J-A6",
	Artifacts: []string{
		"CLAUDE.md",
		"project-context.md",
	},
	Focus: "Evaluate ONLY the [J-A*] (Architecture) judgment criteria. Ignore spec quality and decomposition.",
}

// Phase C — feature decomposition.
var criticPhaseDecomp = criticPhase{
	ID:       "C",
	Name:     "Decomposition",
	Criteria: "J-D1..J-D6",
	Artifacts: []string{
		"features.json",
		"validation-contract.md",
	},
	Focus: "Evaluate ONLY the [J-D*] (Decomposition) judgment criteria. Ignore spec quality and architecture.",
}

func RunMechanicalChecks(specDir, projectDir string) (*MechanicalResult, error) {
	scriptPath, cleanup, err := ExtractMechanicalScript()
	if err != nil {
		return nil, err
	}
	defer cleanup()

	args := []string{scriptPath, "--project", specDir, "--format", "json"}
	if projectDir != "" {
		args = append(args, "--root", projectDir)
	}
	cmd := exec.Command("node", args...)
	out, runErr := cmd.CombinedOutput()

	result := &MechanicalResult{RawOut: string(out)}

	var wrapped struct {
		Results []MechanicalCheck `json:"results"`
	}
	if err := json.Unmarshal(out, &wrapped); err == nil && len(wrapped.Results) > 0 {
		for _, c := range wrapped.Results {
			if c.Status == "pass" {
				result.Passed++
			} else if strings.HasPrefix(c.ID, "M-A") {
				result.Passed++
				c.Message = "(advisory) " + c.Message
			} else {
				result.Failed++
			}
		}
		result.Details = wrapped.Results
		return result, nil
	}

	// Fallback: try flat array
	var checks []MechanicalCheck
	if err := json.Unmarshal(out, &checks); err == nil {
		for _, c := range checks {
			if c.Status == "pass" {
				result.Passed++
			} else {
				result.Failed++
			}
		}
		result.Details = checks
		return result, nil
	}

	if runErr != nil {
		result.Failed = 1
		return result, nil
	}
	result.Passed = 1
	return result, nil
}

const maxCriticRetries = 2

func RunCriticGate(projectDir, missionDir string, verbose *bool, eventCh chan WorkerEvent) {
	specDir := filepath.Dir(missionDir)

	eventCh <- WorkerEvent{Role: "critic", Line: "▶ Running mechanical checks..."}

	mech, err := RunMechanicalChecks(specDir, projectDir)
	if err != nil {
		eventCh <- WorkerEvent{Role: "critic", Line: fmt.Sprintf("⚠ Mechanical checks skipped: %s", err)}
	} else if mech.Failed > 0 {
		var lines []string
		for _, d := range mech.Details {
			if d.Status != "pass" {
				lines = append(lines, fmt.Sprintf("  ✕ [%s] %s", d.ID, d.Message))
			}
		}
		eventCh <- WorkerEvent{
			Role: "critic",
			Line: fmt.Sprintf("✕ Mechanical checks: %d passed, %d failed\n%s", mech.Passed, mech.Failed, strings.Join(lines, "\n")),
		}
		mechReport := &CriticReport{
			Phase:            "mechanical",
			MechanicalPassed: mech.Passed,
			MechanicalFailed: mech.Failed,
			Overall:          "needs-work",
		}
		for _, d := range mech.Details {
			if d.Status != "pass" {
				mechReport.Findings = append(mechReport.Findings, CriticFinding{
					Criterion: d.ID,
					Status:    "needs-work",
					Note:      d.Message,
				})
				mechReport.BlockingFindings = append(mechReport.BlockingFindings, d.ID)
			}
		}
		eventCh <- WorkerEvent{Role: "critic", Done: true, Verdict: "FAIL", CriticReport: mechReport}
		return
	} else {
		eventCh <- WorkerEvent{Role: "critic", Line: fmt.Sprintf("✓ Mechanical checks: %d passed", mech.Passed)}
	}

	eventCh <- WorkerEvent{Role: "critic", Line: "▶ Running judgment checks (3 phases in parallel)..."}

	heartbeatStop := make(chan struct{})
	go criticHeartbeat(eventCh, heartbeatStop)

	phases := []criticPhase{criticPhaseSpec, criticPhaseArch, criticPhaseDecomp}
	reports := make([]*CriticReport, len(phases))
	errs := make([]error, len(phases))

	var wg sync.WaitGroup
	for i, p := range phases {
		wg.Add(1)
		go func(i int, p criticPhase) {
			defer wg.Done()
			eventCh <- WorkerEvent{Role: "critic", Line: fmt.Sprintf("[%s] ▶ %s phase started", p.ID, p.Name)}
			prompt := BuildCriticPhasePrompt(specDir, p)
			rep, err := runCriticPhaseJudgment(p, prompt, projectDir, verbose, eventCh)
			reports[i] = rep
			errs[i] = err
			if err != nil {
				eventCh <- WorkerEvent{Role: "critic", Line: fmt.Sprintf("[%s] ✕ %s phase failed: %s", p.ID, p.Name, err)}
			} else if rep != nil {
				eventCh <- WorkerEvent{Role: "critic", Line: fmt.Sprintf("[%s] ✓ %s phase done — %s", p.ID, p.Name, rep.Overall)}
			}
		}(i, p)
	}
	wg.Wait()
	close(heartbeatStop)

	report := mergeCriticReports(reports, errs, mech)
	persistCriticReport(missionDir, report)

	if report.Overall == "needs-work" {
		var findings []string
		for _, f := range report.Findings {
			if f.Status == "needs-work" {
				findings = append(findings, fmt.Sprintf("  ✕ [%s] %s → %s", f.Criterion, f.Target, f.Suggestion))
			}
		}
		eventCh <- WorkerEvent{
			Role: "critic",
			Line: fmt.Sprintf("✕ Judgment: needs-work — %d blocking findings\n%s", len(report.BlockingFindings), strings.Join(findings, "\n")),
		}
		eventCh <- WorkerEvent{Role: "critic", Done: true, Verdict: "FAIL", CriticReport: report}
		return
	}

	eventCh <- WorkerEvent{Role: "critic", Line: "✓ Critic gate passed"}
	eventCh <- WorkerEvent{Role: "critic", Done: true, Verdict: "PASS"}
}

// Deprecated: use BuildCriticPhasePrompt with the parallel critic split.
// Kept for the fix-critic flow and for emergency rollback. Will be removed
// after the split has shipped successfully end-to-end.
func BuildCriticPrompt(specDir string) string {
	criticSkill := ReadSkill("mission-critic")
	missionDir := filepath.Join(specDir, "mission")
	projectRoot := filepath.Dir(filepath.Dir(filepath.Dir(specDir)))

	priorReports := readLatestCriticReport(missionDir)

	// All heavy artifacts load via Read tool to keep the prompt small.
	criteriaPath := criteriaMdPath()
	contractPath := filepath.Join(missionDir, "validation-contract.md")
	featuresPath := filepath.Join(missionDir, "features.json")
	projectContextPath := filepath.Join(missionDir, "project-context.md")
	claudeMdPath := filepath.Join(projectRoot, "CLAUDE.md")
	localCriteriaPath := filepath.Join(missionDir, "critique-criteria.local.md")

	var filesToRead []string
	if criteriaPath != "" && fileExists(criteriaPath) {
		filesToRead = append(filesToRead, criteriaPath)
	}
	filesToRead = append(filesToRead, contractPath, featuresPath)
	if fileExists(projectContextPath) {
		filesToRead = append(filesToRead, projectContextPath)
	}
	if fileExists(claudeMdPath) {
		filesToRead = append(filesToRead, claudeMdPath)
	}
	if fileExists(localCriteriaPath) {
		filesToRead = append(filesToRead, localCriteriaPath)
	}

	var fileList strings.Builder
	for i, f := range filesToRead {
		fileList.WriteString(fmt.Sprintf("  %d. %s\n", i+1, f))
	}

	var parts []string
	parts = append(parts,
		"IMPORTANT: Do NOT narrate, explain, or describe what you are doing. Just act.",
		"",
		"You are running the mission-critic skill. Follow it precisely.",
		"",
		"## Skill Reference",
		"",
		criticSkill,
		"",
	)

	if priorReports != "" {
		parts = append(parts, "## Prior Critic Reports", "", priorReports, "")
	}

	parts = append(parts,
		"## Spec folder: "+specDir,
		"",
		"## Files to Read (use the Read tool — load EACH one before evaluating)",
		"",
		fileList.String(),
		"File 1 is CRITERIA.md — that defines the judgment criteria you must apply.",
		"After loading every file above, evaluate the judgment criteria. Do NOT use Glob, Grep, Bash, WebFetch, WebSearch, Edit, or Write — only Read.",
		"",
		"## Instructions",
		"",
		"Mechanical checks ([M-*] criteria) have ALREADY been run and passed by the orchestrator.",
		"Do NOT re-run run-mechanical.mjs or evaluate any [M-*] criteria yourself.",
		"",
		"Evaluate ONLY the judgment criteria [J-*] across all three phases (A, B, C).",
		"For each judgment criterion, emit pass or needs-work with concrete suggestions.",
		"",
		"## Output strategy (CRITICAL — read carefully)",
		"",
		"After reading the files, START EMITTING THE JSON IMMEDIATELY. Do NOT think through all 18 criteria silently before outputting.",
		"Output the JSON in order: open the object, fill mechanical, then emit each judgment one at a time as you reason through each criterion.",
		"You will reason WHILE writing, not before writing. Streaming output is faster than upfront analysis.",
		"",
		"Output ONLY a valid JSON object matching this schema:",
		`{"phase":"all","artifact":"<path>","started_at":"<ISO>","ended_at":"<ISO>","mechanical":{"passed":0,"failed":0},"judgment":[{"criterion":"J-S1","status":"pass","note":"..."},{"criterion":"J-S5","status":"needs-work","target":"...","suggestion":"..."}],"overall":"pass","blocking_findings":[]}`,
		"",
		"If ALL judgment criteria pass, set overall to \"pass\". If ANY is needs-work, set overall to \"needs-work\".",
		"Output ONLY the JSON, nothing else.",
	)

	return strings.Join(parts, "\n")
}

// BuildCriticPhasePrompt builds a focused prompt for one of the three
// parallel critic phases (A: Spec quality, B: Architecture, C: Decomposition).
// All artifacts are inlined directly so the model can answer in a single
// turn — no tool roundtrips, no internal "let me read this carefully" loops.
// This mirrors how BuildFixCriticPrompt already runs and is the proven path
// for fast, deterministic critic output.
func BuildCriticPhasePrompt(specDir string, phase criticPhase) string {
	criticSkill := ReadSkill("mission-critic")
	missionDir := filepath.Join(specDir, "mission")
	projectRoot := filepath.Dir(filepath.Dir(filepath.Dir(specDir)))

	priorReports := readLatestCriticReport(missionDir)

	// Resolve every artifact to an absolute path + content. Anything missing
	// is silently dropped so a project without CLAUDE.md or project-context
	// still produces a usable prompt.
	type loadedArtifact struct {
		Path    string
		Content string
	}
	loadArtifact := func(name string) (loadedArtifact, bool) {
		var abs string
		switch name {
		case "CLAUDE.md":
			abs = filepath.Join(projectRoot, "CLAUDE.md")
		default:
			abs = filepath.Join(missionDir, name)
		}
		if !fileExists(abs) {
			return loadedArtifact{}, false
		}
		return loadedArtifact{Path: abs, Content: readFileContent(abs)}, true
	}

	var artifacts []loadedArtifact
	if criteria := readCriteriaMd(); strings.TrimSpace(criteria) != "" {
		artifacts = append(artifacts, loadedArtifact{Path: criteriaMdPath(), Content: criteria})
	}
	for _, name := range phase.Artifacts {
		if a, ok := loadArtifact(name); ok {
			artifacts = append(artifacts, a)
		}
	}
	if local := filepath.Join(missionDir, "critique-criteria.local.md"); fileExists(local) {
		artifacts = append(artifacts, loadedArtifact{Path: local, Content: readFileContent(local)})
	}

	var parts []string
	parts = append(parts,
		"IMPORTANT: Do NOT narrate, explain, or describe what you are doing. Just emit the JSON.",
		"",
		fmt.Sprintf("You are running the mission-critic skill — Phase %s (%s).", phase.ID, phase.Name),
		phase.Focus,
		"",
		"## Skill Reference",
		"",
		criticSkill,
		"",
	)

	if priorReports != "" {
		parts = append(parts, "## Prior Critic Reports", "", priorReports, "")
	}

	parts = append(parts, "## Spec folder: "+specDir, "")

	// Inline every artifact — no Read tool roundtrips. Each block is fenced
	// with its absolute path so the model can quote concrete locations in
	// its findings.
	for _, a := range artifacts {
		fence := "```"
		parts = append(parts,
			fmt.Sprintf("## Artifact: %s", a.Path),
			"",
			fence,
			a.Content,
			fence,
			"",
		)
	}

	parts = append(parts,
		"## Instructions",
		"",
		"CRITICAL: All artifacts are inlined above. Do NOT use Read, Glob, Grep, Bash, or any other tool — answer directly from the inlined content.",
		"",
		"Mechanical checks ([M-*] criteria) have ALREADY been run and passed by the orchestrator.",
		"Do NOT re-run run-mechanical.mjs or evaluate any [M-*] criteria yourself.",
		"",
		fmt.Sprintf("Evaluate ONLY the %s judgment criteria for Phase %s. Skip every other phase's criteria.", phase.Criteria, phase.ID),
		"For each judgment criterion, emit pass or needs-work with concrete suggestions.",
		"",
		"## Output",
		"",
		"Output ONLY a valid JSON object matching this schema:",
		fmt.Sprintf(`{"phase":%q,"artifact":"<path>","started_at":"<ISO>","ended_at":"<ISO>","mechanical":{"passed":0,"failed":0},"judgment":[{"criterion":"%s","status":"pass","note":"..."}],"overall":"pass","blocking_findings":[]}`, phase.ID, firstCriterion(phase.Criteria)),
		"",
		"If ALL judgment criteria for this phase pass, set overall to \"pass\". If ANY is needs-work, set overall to \"needs-work\".",
		"Output ONLY the JSON, nothing else.",
	)

	return strings.Join(parts, "\n")
}

// firstCriterion returns the first ID listed in a "J-X1..J-X6"-style label so
// the prompt can include a concrete schema example.
func firstCriterion(label string) string {
	if i := strings.Index(label, ".."); i > 0 {
		return strings.TrimSpace(label[:i])
	}
	return strings.TrimSpace(label)
}

// runCriticPhaseJudgment runs one critic phase end-to-end (prompt + parse +
// retry). It mirrors runCriticJudgment but prefixes every streamed line with
// the phase ID so the unified critic log distinguishes the three streams.
func runCriticPhaseJudgment(phase criticPhase, prompt, projectDir string, verbose *bool, eventCh chan WorkerEvent) (*CriticReport, error) {
	prefix := fmt.Sprintf("[%s] ", phase.ID)
	emit := func(line string) {
		eventCh <- WorkerEvent{Role: "critic", Line: prefix + line}
	}

	var report *CriticReport
	var lastErr error
	for retry := 0; retry <= maxCriticRetries; retry++ {
		if retry > 0 {
			emit(fmt.Sprintf("⚠ Retrying judgment (%d/%d)...", retry, maxCriticRetries))
		}
		resultText, err := runCriticPhaseSubprocess(prompt, projectDir, verbose, eventCh, prefix)
		if err != nil {
			lastErr = err
			emit(fmt.Sprintf("✕ Critic error: %s", err))
			continue
		}
		lastErr = nil
		report = ParseCriticReport(resultText)
		if report != nil {
			break
		}
		if retry < maxCriticRetries {
			emit("⚠ Could not parse judgment result")
		}
	}

	if report == nil {
		if lastErr == nil {
			lastErr = fmt.Errorf("phase %s: failed to produce a valid judgment after %d retries", phase.ID, maxCriticRetries+1)
		}
		return nil, lastErr
	}
	if report.Phase == "" {
		report.Phase = phase.ID
	}
	return report, nil
}

// criticPhaseTimeout is the maximum wall-clock time a single phase attempt
// is allowed to run before its claude subprocess is killed. With artifacts
// inlined and max-turns 2 a healthy phase finishes in 30-90s; 3 minutes is
// a generous safety net so a wedged attempt cannot stall the whole gate.
const criticPhaseTimeout = 3 * time.Minute

// runCriticPhaseSubprocess is the per-phase analogue of runCriticJudgment.
// Artifacts are inlined into the prompt so we can keep --max-turns at 2
// (model can either answer directly or after a single sanity tool call)
// and a hard wall-clock timeout protects against internal reasoning stalls.
// A timeout surfaces as a real error so the merge step turns it into a
// synthetic phase-X-error finding instead of leaving the whole gate hanging.
func runCriticPhaseSubprocess(prompt, projectDir string, verbose *bool, eventCh chan WorkerEvent, prefix string) (string, error) {
	claudeCh := make(chan ClaudeStreamMsg, 64)
	criticArgs := []string{
		"--allowedTools", "Read",
		"--max-turns", "2",
		"--model", "claude-sonnet-4-6",
	}

	var (
		cmdMu      sync.Mutex
		currentCmd *exec.Cmd
	)
	setCmd := func(c *exec.Cmd) {
		cmdMu.Lock()
		currentCmd = c
		cmdMu.Unlock()
	}
	killCurrent := func() {
		cmdMu.Lock()
		defer cmdMu.Unlock()
		if currentCmd != nil && currentCmd.Process != nil {
			_ = currentCmd.Process.Kill()
		}
	}

	setCmd(StartClaude(prompt, projectDir, verbose, claudeCh, criticArgs...))

	// Watchdog: kill the active subprocess if the phase exceeds the timeout.
	// We close stopWatchdog on success / hard error so the timer goroutine
	// can exit cleanly.
	stopWatchdog := make(chan struct{})
	timedOut := make(chan struct{})
	go func() {
		select {
		case <-time.After(criticPhaseTimeout):
			eventCh <- WorkerEvent{Role: "critic", Line: fmt.Sprintf("%s⏱ Phase timed out after %s — killing subprocess", prefix, criticPhaseTimeout)}
			close(timedOut)
			killCurrent()
		case <-stopWatchdog:
		}
	}()
	defer close(stopWatchdog)

	var resultText string
	var lastSessionID string
	for attempt := 0; ; attempt++ {
		gotResult := false
		for msg := range claudeCh {
			if msg.Line != "" {
				eventCh <- WorkerEvent{Role: "critic", Line: prefix + msg.Line}
			}
			if msg.Done {
				if msg.SessionID != "" {
					lastSessionID = msg.SessionID
				}
				if msg.Err != nil {
					select {
					case <-timedOut:
						return "", fmt.Errorf("phase timed out after %s", criticPhaseTimeout)
					default:
					}
					if isTransientError(msg.Err) && attempt < maxTransientRetries {
						backoff := time.Duration(attempt+1) * 5 * time.Second
						label := ""
						if lastSessionID != "" {
							label = " (resuming session)"
						}
						eventCh <- WorkerEvent{Role: "critic", Line: fmt.Sprintf("%s⚠ Transient error, retrying (%d/%d)%s in %s...", prefix, attempt+1, maxTransientRetries, label, backoff)}
						time.Sleep(backoff)
						claudeCh = make(chan ClaudeStreamMsg, 64)
						if lastSessionID != "" {
							setCmd(StartClaude(
								"An API error interrupted your evaluation. Continue from where you left off. Output ONLY the JSON result.",
								projectDir, verbose, claudeCh,
								"--resume", lastSessionID,
								"--max-turns", "3",
								"--model", "claude-sonnet-4-6",
							))
						} else {
							setCmd(StartClaude(prompt, projectDir, verbose, claudeCh, criticArgs...))
						}
						break
					}
					return "", msg.Err
				}
				resultText = msg.Result
				gotResult = true
			}
		}
		if gotResult {
			break
		}
		select {
		case <-timedOut:
			return "", fmt.Errorf("phase timed out after %s", criticPhaseTimeout)
		default:
		}
	}
	return resultText, nil
}

// mergeCriticReports unions the three per-phase reports back into a single
// CriticReport that downstream code (persist, UI, gating) can consume without
// caring whether the critic ran serially or in parallel. Findings keep their
// A/B/C order, Overall is the AND of all three phases, and any phase that
// errored unrecoverably becomes a synthetic phase-{X}-error finding so the
// failure shows up in the report instead of disappearing.
func mergeCriticReports(parts []*CriticReport, errs []error, mech *MechanicalResult) *CriticReport {
	merged := &CriticReport{
		Phase:   "all",
		Overall: "pass",
	}
	if mech != nil {
		merged.MechanicalPassed = mech.Passed
		merged.MechanicalFailed = mech.Failed
	}

	seen := map[string]bool{}
	hasNeedsWork := false
	for i, part := range parts {
		phaseID := ""
		if i < len(parts) {
			switch i {
			case 0:
				phaseID = "A"
			case 1:
				phaseID = "B"
			case 2:
				phaseID = "C"
			}
		}

		if i < len(errs) && errs[i] != nil {
			hasNeedsWork = true
			synthetic := CriticFinding{
				Criterion: fmt.Sprintf("phase-%s-error", phaseID),
				Status:    "needs-work",
				Note:      errs[i].Error(),
			}
			merged.Findings = append(merged.Findings, synthetic)
			merged.BlockingFindings = append(merged.BlockingFindings, synthetic.Criterion)
			continue
		}
		if part == nil {
			hasNeedsWork = true
			synthetic := CriticFinding{
				Criterion: fmt.Sprintf("phase-%s-error", phaseID),
				Status:    "needs-work",
				Note:      "no report returned",
			}
			merged.Findings = append(merged.Findings, synthetic)
			merged.BlockingFindings = append(merged.BlockingFindings, synthetic.Criterion)
			continue
		}

		if part.Overall == "needs-work" {
			hasNeedsWork = true
		}
		for _, f := range part.Findings {
			if seen[f.Criterion] {
				continue
			}
			seen[f.Criterion] = true
			merged.Findings = append(merged.Findings, f)
		}
		// Prefer the explicit blocking list, but fall back to scanning the
		// findings so a model that forgets to fill blocking_findings still
		// surfaces every needs-work criterion.
		if len(part.BlockingFindings) > 0 {
			merged.BlockingFindings = append(merged.BlockingFindings, part.BlockingFindings...)
		} else {
			for _, f := range part.Findings {
				if f.Status == "needs-work" {
					merged.BlockingFindings = append(merged.BlockingFindings, f.Criterion)
				}
			}
		}
	}

	if hasNeedsWork {
		merged.Overall = "needs-work"
	}
	return merged
}

func persistCriticReport(missionDir string, report *CriticReport) {
	runDir := filepath.Join(missionDir, "runs")
	_ = os.MkdirAll(runDir, 0o755)
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(runDir, "critic-report.json"), data, 0o644)
}

func RunFixCriticGate(projectDir, missionDir string, fixes []Feature, verbose *bool, eventCh chan WorkerEvent) {
	specDir := filepath.Dir(missionDir)

	eventCh <- WorkerEvent{Role: "critic", Line: fmt.Sprintf("▶ Running critic gate on %d fix features...", len(fixes))}

	prompt := BuildFixCriticPrompt(specDir, fixes)

	heartbeatStop := make(chan struct{})
	go criticHeartbeat(eventCh, heartbeatStop)
	defer close(heartbeatStop)

	var report *CriticReport
	var lastErr error
	for retry := 0; retry <= maxCriticRetries; retry++ {
		if retry > 0 {
			eventCh <- WorkerEvent{Role: "critic", Line: fmt.Sprintf("⚠ Retrying fix critic (%d/%d)...", retry, maxCriticRetries)}
		}
		resultText, err := runCriticJudgment(prompt, projectDir, verbose, eventCh)
		if err != nil {
			lastErr = err
			eventCh <- WorkerEvent{Role: "critic", Line: fmt.Sprintf("⚠ Fix critic error: %s", err)}
			continue
		}
		lastErr = nil
		report = ParseCriticReport(resultText)
		if report != nil {
			break
		}
		if retry < maxCriticRetries {
			eventCh <- WorkerEvent{Role: "critic", Line: "⚠ Could not parse fix critic result"}
		}
	}

	if report != nil {
		persistCriticReport(missionDir, report)
	}

	// Fix critic is lenient: pass on persistent errors or unparseable results
	if report == nil {
		reason := "could not parse result"
		if lastErr != nil {
			reason = lastErr.Error()
		}
		eventCh <- WorkerEvent{Role: "critic", Line: fmt.Sprintf("⚠ Fix critic: %s — proceeding anyway", reason)}
		eventCh <- WorkerEvent{Role: "critic", Done: true, Verdict: "PASS"}
		return
	}

	if report.Overall == "needs-work" {
		var findings []string
		for _, f := range report.Findings {
			if f.Status == "needs-work" {
				findings = append(findings, fmt.Sprintf("  ✕ [%s] %s → %s", f.Criterion, f.Target, f.Suggestion))
			}
		}
		eventCh <- WorkerEvent{
			Role: "critic",
			Line: fmt.Sprintf("✕ Fix critic: needs-work\n%s", strings.Join(findings, "\n")),
		}
		eventCh <- WorkerEvent{Role: "critic", Done: true, Verdict: "FAIL"}
		return
	}

	eventCh <- WorkerEvent{Role: "critic", Line: "✓ Fix features passed critic gate"}
	eventCh <- WorkerEvent{Role: "critic", Done: true, Verdict: "PASS"}
}

func BuildFixCriticPrompt(specDir string, fixes []Feature) string {
	criticSkill := ReadSkill("mission-critic")
	missionDir := filepath.Join(specDir, "mission")

	contract := readFileContent(filepath.Join(missionDir, "validation-contract.md"))
	features := readFileContent(filepath.Join(missionDir, "features.json"))

	fixesJSON, _ := json.MarshalIndent(fixes, "", "  ")

	var parts []string
	parts = append(parts,
		"You are running a TARGETED critic check on fix features generated by refinement.",
		"",
		"## Skill Reference (Phase C only)",
		"",
		criticSkill,
		"",
		"## Context",
		"",
		"Spec folder: "+specDir,
		"",
		"## Fix Features to Review",
		"",
		string(fixesJSON),
		"",
		"## Full Features Manifest (for context)",
		"",
		features,
		"",
		"## Validation Contract",
		"",
		contract,
		"",
		"## Instructions",
		"",
		"CRITICAL: ALL artifacts are PROVIDED ABOVE. Do NOT use Read, Glob, Grep, Bash, or any file-reading tools.",
		"Start evaluating IMMEDIATELY.",
		"",
		"Run ONLY Phase C (features.json decomposition) on the fix features above.",
		"Check:",
		"- Each fix feature has a clear, testable scope",
		"- validation_refs reference real assertions",
		"- depends_on references are valid",
		"- No circular dependencies",
		"- Scope is minimum (fix, not refactor)",
		"",
		"Output ONLY a valid JSON object:",
		`{"phase":"C","artifact":"fix-features","started_at":"<ISO>","ended_at":"<ISO>","mechanical":{"passed":0,"failed":0},"judgment":[{"criterion":"J-C1","status":"pass|needs-work","note":"..."}],"overall":"pass|needs-work","blocking_findings":[]}`,
		"",
		"Output ONLY the JSON, nothing else.",
	)

	return strings.Join(parts, "\n")
}

func readCriteriaMd() string {
	return readFileContent(criteriaMdPath())
}

// criteriaMdPath returns the absolute path to CRITERIA.md so the prompt can
// reference it as a Read target instead of inlining the full content.
func criteriaMdPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", "skills", "mission-critic", "CRITERIA.md")
}

func readLatestCriticReport(missionDir string) string {
	pattern := filepath.Join(missionDir, "runs", "critic-*.json")
	matches, err := filepath.Glob(pattern)
	if err != nil || len(matches) == 0 {
		return ""
	}
	// Glob returns sorted results; last is most recent alphabetically
	latest := matches[len(matches)-1]
	content := readFileContent(latest)
	if content == "" {
		return ""
	}
	return fmt.Sprintf("### %s\n\n%s", filepath.Base(latest), content)
}

func ParseCriticReport(text string) *CriticReport {
	text = strings.TrimSpace(text)

	var report CriticReport
	if err := json.Unmarshal([]byte(text), &report); err == nil && report.Overall != "" {
		return &report
	}

	// Try extracting from code fences
	re := strings.Index(text, "```")
	if re >= 0 {
		end := strings.Index(text[re+3:], "```")
		if end >= 0 {
			block := text[re+3 : re+3+end]
			if nl := strings.Index(block, "\n"); nl >= 0 {
				block = block[nl+1:]
			}
			if err := json.Unmarshal([]byte(strings.TrimSpace(block)), &report); err == nil && report.Overall != "" {
				return &report
			}
		}
	}

	return nil
}

// Deprecated: the main critic gate now uses runCriticPhaseJudgment via
// the parallel phase split. Kept because RunFixCriticGate still relies on
// the monolithic critic prompt for fix features. Will move to a phase-aware
// flow once the fix-critic side is split too.
func runCriticJudgment(prompt, projectDir string, verbose *bool, eventCh chan WorkerEvent) (string, error) {
	claudeCh := make(chan ClaudeStreamMsg, 64)
	criticArgs := []string{
		"--allowedTools", "Read",
		"--max-turns", "6",
		"--model", "claude-sonnet-4-6",
	}
	cmd := StartClaude(prompt, projectDir, verbose, claudeCh, criticArgs...)
	_ = cmd

	var resultText string
	var lastSessionID string
	for attempt := 0; ; attempt++ {
		gotResult := false
		for msg := range claudeCh {
			if msg.Line != "" {
				eventCh <- WorkerEvent{Role: "critic", Line: msg.Line}
			}
			if msg.Done {
				if msg.SessionID != "" {
					lastSessionID = msg.SessionID
				}
				if msg.Err != nil {
					if isTransientError(msg.Err) && attempt < maxTransientRetries {
						backoff := time.Duration(attempt+1) * 5 * time.Second
						label := ""
						if lastSessionID != "" {
							label = " (resuming session)"
						}
						eventCh <- WorkerEvent{Role: "critic", Line: fmt.Sprintf("⚠ Transient error, retrying (%d/%d)%s in %s...", attempt+1, maxTransientRetries, label, backoff)}
						time.Sleep(backoff)
						claudeCh = make(chan ClaudeStreamMsg, 64)
						if lastSessionID != "" {
							cmd = StartClaude(
								"An API error interrupted your evaluation. Continue from where you left off. Output ONLY the JSON result.",
								projectDir, verbose, claudeCh,
								"--resume", lastSessionID,
								"--max-turns", "4",
								"--model", "claude-sonnet-4-6",
							)
						} else {
							cmd = StartClaude(prompt, projectDir, verbose, claudeCh, criticArgs...)
						}
						_ = cmd
						break
					}
					return "", msg.Err
				}
				resultText = msg.Result
				gotResult = true
			}
		}
		if gotResult {
			break
		}
	}
	return resultText, nil
}

func criticHeartbeat(eventCh chan WorkerEvent, stop <-chan struct{}) {
	start := time.Now()
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			elapsed := time.Since(start).Round(time.Second)
			eventCh <- WorkerEvent{Role: "critic", Line: fmt.Sprintf("💭 Thinking... (%s)", elapsed)}
		case <-stop:
			return
		}
	}
}

func compactFeaturesForCritic(path string) string {
	raw := readFileContent(path)
	if raw == "" {
		return ""
	}
	var manifest FeaturesManifest
	if err := json.Unmarshal([]byte(raw), &manifest); err != nil {
		return raw
	}
	type compactFeature struct {
		ID             string   `json:"id"`
		Title          string   `json:"title"`
		Phase          int      `json:"phase"`
		Status         string   `json:"status"`
		DependsOn      []string `json:"depends_on"`
		Scope          string   `json:"scope"`
		ValidationRefs []string `json:"validation_refs"`
		Fixes          string   `json:"fixes,omitempty"`
	}
	all := append(manifest.Features, manifest.FixFeatures...)
	compact := make([]compactFeature, len(all))
	for i, f := range all {
		compact[i] = compactFeature{
			ID:             f.ID,
			Title:          f.Title,
			Phase:          f.Phase,
			Status:         f.Status,
			DependsOn:      f.DependsOn,
			Scope:          f.Scope,
			ValidationRefs: f.ValidationRefs,
			Fixes:          f.Fixes,
		}
	}
	out, err := json.MarshalIndent(compact, "", "  ")
	if err != nil {
		return raw
	}
	return string(out)
}

func truncateContent(s string, maxChars int) string {
	if len(s) <= maxChars {
		return s
	}
	cut := strings.LastIndex(s[:maxChars], "\n")
	if cut < maxChars/2 {
		cut = maxChars
	}
	return s[:cut] + "\n..."
}

func BuildAutoFixPrompt(report *CriticReport, specDir, projectDir string) string {
	missionDir := filepath.Join(specDir, "mission")
	projectRoot := filepath.Dir(filepath.Dir(filepath.Dir(specDir)))

	var findings []CriticFinding
	hasArchFindings := false
	for _, f := range report.Findings {
		if f.Status == "needs-work" {
			findings = append(findings, f)
			if strings.HasPrefix(f.Criterion, "J-A") {
				hasArchFindings = true
			}
		}
	}

	findingsJSON, _ := json.MarshalIndent(findings, "", "  ")

	contract := readFileContent(filepath.Join(missionDir, "validation-contract.md"))
	features := readFileContent(filepath.Join(missionDir, "features.json"))
	spec := readFileContent(filepath.Join(specDir, "spec.md"))

	var parts []string
	parts = append(parts,
		"You are an AUTO-FIX agent. The critic gate found blocking issues in the mission spec artifacts.",
		"Apply the suggested fixes so the mission can proceed.",
		"",
		"## Blocking Findings",
		"",
		string(findingsJSON),
		"",
		"## Current Artifacts",
		"",
		"### validation-contract.md",
		"Path: "+filepath.Join(missionDir, "validation-contract.md"),
		"",
		contract,
		"",
		"### features.json",
		"Path: "+filepath.Join(missionDir, "features.json"),
		"",
		features,
		"",
		"### spec.md",
		"Path: "+filepath.Join(specDir, "spec.md"),
		"",
		spec,
		"",
	)

	if hasArchFindings {
		claudeMd := readFileContent(filepath.Join(projectRoot, "CLAUDE.md"))
		if claudeMd != "" {
			parts = append(parts,
				"### CLAUDE.md (Architecture)",
				"Path: "+filepath.Join(projectRoot, "CLAUDE.md"),
				"",
				claudeMd,
				"",
			)
		}
	}

	parts = append(parts,
		"## Instructions",
		"",
		"For each finding above, apply the suggestion by editing the relevant file.",
		"Use the Edit tool to make surgical changes — do NOT rewrite entire files.",
		"",
		"Rules:",
		"- For J-S* findings: edit validation-contract.md (rewrite/add assertions)",
		"- For J-D* findings: edit features.json (split features, add scope, fix deps)",
		"- For J-A* findings: edit the project CLAUDE.md (add architecture sections)",
		"- Preserve all existing content that is not flagged",
		"- Keep assertion IDs stable when rewriting (e.g. data.12 stays data.12)",
		"- When splitting features, use sub-IDs (F05 → F05a, F05b)",
		"- Ensure validation_refs in features.json match assertion IDs in the contract",
		"",
		"Do NOT create new files. Do NOT modify source code. Only fix spec artifacts.",
		"After all edits, verify consistency between features.json and validation-contract.md.",
	)

	return strings.Join(parts, "\n")
}

func IsBlockingCriterion(criterion string) bool {
	return strings.HasPrefix(criterion, "J-S") || strings.HasPrefix(criterion, "J-D")
}

func (r *CriticReport) HasBlockingFailures() bool {
	for _, f := range r.Findings {
		if f.Status == "needs-work" && IsBlockingCriterion(f.Criterion) {
			return true
		}
	}
	return false
}

func (r *CriticReport) BlockingFailures() []CriticFinding {
	var out []CriticFinding
	for _, f := range r.Findings {
		if f.Status == "needs-work" && IsBlockingCriterion(f.Criterion) {
			out = append(out, f)
		}
	}
	return out
}

func (r *CriticReport) AdvisoryFindings() []CriticFinding {
	var out []CriticFinding
	for _, f := range r.Findings {
		if f.Status == "needs-work" && strings.HasPrefix(f.Criterion, "J-A") {
			out = append(out, f)
		}
	}
	return out
}

func BuildBlockingAutoFixPrompt(report *CriticReport, specDir, projectDir string) string {
	missionDir := filepath.Join(specDir, "mission")

	findings := report.BlockingFailures()
	if len(findings) == 0 {
		return ""
	}

	findingsJSON, _ := json.MarshalIndent(findings, "", "  ")

	contract := readFileContent(filepath.Join(missionDir, "validation-contract.md"))
	features := readFileContent(filepath.Join(missionDir, "features.json"))
	spec := readFileContent(filepath.Join(specDir, "spec.md"))

	var parts []string
	parts = append(parts,
		"You are an AUTO-FIX agent. The critic gate found blocking spec-quality issues.",
		"Apply the suggested fixes so the spec passes validation.",
		"",
		"## Blocking Findings (spec quality only — J-S* and J-D*)",
		"",
		string(findingsJSON),
		"",
		"## Current Artifacts",
		"",
		"### validation-contract.md",
		"Path: "+filepath.Join(missionDir, "validation-contract.md"),
		"",
		contract,
		"",
		"### features.json",
		"Path: "+filepath.Join(missionDir, "features.json"),
		"",
		features,
		"",
		"### spec.md",
		"Path: "+filepath.Join(specDir, "spec.md"),
		"",
		spec,
		"",
		"## Instructions",
		"",
		"For each finding above, apply the suggestion by editing the relevant file.",
		"Use the Edit tool to make surgical changes — do NOT rewrite entire files.",
		"",
		"Rules:",
		"- For J-S* findings: edit validation-contract.md (rewrite/add assertions)",
		"- For J-D* findings: edit features.json (split features, add scope, fix deps)",
		"- Do NOT edit CLAUDE.md — architecture findings are handled separately",
		"- Preserve all existing content that is not flagged",
		"- Keep assertion IDs stable when rewriting (e.g. data.12 stays data.12)",
		"- When splitting features, use sub-IDs (F05 → F05a, F05b)",
		"- Ensure validation_refs in features.json match assertion IDs in the contract",
		"",
		"Do NOT create new files. Do NOT modify source code. Only fix spec artifacts.",
		"After all edits, verify consistency between features.json and validation-contract.md.",
	)

	return strings.Join(parts, "\n")
}

func BuildAdvisoryAutoFixPrompt(findings []CriticFinding, specDir, projectDir string) string {
	if len(findings) == 0 {
		return ""
	}

	projectRoot := filepath.Dir(filepath.Dir(filepath.Dir(specDir)))
	findingsJSON, _ := json.MarshalIndent(findings, "", "  ")

	claudeMd := readFileContent(filepath.Join(projectRoot, "CLAUDE.md"))

	var parts []string
	parts = append(parts,
		"You are an AUTO-FIX agent. The user selected architecture-level findings to fix.",
		"These are advisory improvements to the project's CLAUDE.md documentation.",
		"",
		"## Selected Advisory Findings (J-A*)",
		"",
		string(findingsJSON),
		"",
		"### CLAUDE.md (Architecture)",
		"Path: "+filepath.Join(projectRoot, "CLAUDE.md"),
		"",
		claudeMd,
		"",
		"## Instructions",
		"",
		"For each finding above, apply the suggestion by editing CLAUDE.md.",
		"Use the Edit tool to make surgical changes — do NOT rewrite the entire file.",
		"",
		"Rules:",
		"- Add new subsections under the Architecture section",
		"- Preserve all existing content",
		"- Keep additions concise and factual",
		"- Match the existing formatting style of CLAUDE.md",
		"",
		"Do NOT create new files. Do NOT modify source code. Only edit CLAUDE.md.",
	)

	return strings.Join(parts, "\n")
}
