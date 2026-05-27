package internal

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestAutonomousRuntimeState_SaveAndLoad(t *testing.T) {
	missionDir := t.TempDir()
	state := AutonomousRuntimeState{
		LastSessionIDs: map[string]string{
			"worker:F01":   "sess-worker",
			"validator:F1": "sess-validator",
		},
		FailureSignatures: map[string]int{
			"F01|phase-1:f01": 2,
		},
		RecoveryLevel: map[string]int{
			"F01": 3,
		},
		CriticTransientAttempt: map[string]int{
			"quest": 2,
		},
		CriticStructuralTry: map[string]int{
			"quest": 1,
		},
		CriticAutoFixAttempt: map[string]int{
			"quest": 3,
		},
		CriticBypassCount: map[string]int{
			"quest": 4,
		},
		AutoResetCount: 1,
		AutoRegenCount: 0,
	}

	if err := saveAutonomousRuntimeState(missionDir, state); err != nil {
		t.Fatalf("save autonomous state: %v", err)
	}

	loaded := loadAutonomousRuntimeState(missionDir)
	if got := loaded.LastSessionIDs["worker:F01"]; got != "sess-worker" {
		t.Fatalf("expected persisted worker session, got %q", got)
	}
	if got := loaded.FailureSignatures["F01|phase-1:f01"]; got != 2 {
		t.Fatalf("expected failure signature count=2, got %d", got)
	}
	if got := loaded.RecoveryLevel["F01"]; got != 3 {
		t.Fatalf("expected recovery level=3, got %d", got)
	}
	if loaded.AutoResetCount != 1 {
		t.Fatalf("expected auto reset count=1, got %d", loaded.AutoResetCount)
	}
	if got := loaded.CriticTransientAttempt["quest"]; got != 2 {
		t.Fatalf("expected critic transient attempt=2, got %d", got)
	}
	if got := loaded.CriticStructuralTry["quest"]; got != 1 {
		t.Fatalf("expected critic structural attempt=1, got %d", got)
	}
	if got := loaded.CriticAutoFixAttempt["quest"]; got != 3 {
		t.Fatalf("expected critic auto-fix attempt=3, got %d", got)
	}
	if got := loaded.CriticBypassCount["quest"]; got != 4 {
		t.Fatalf("expected critic bypass count=4, got %d", got)
	}
	if loaded.UpdatedAt == "" {
		t.Fatalf("expected UpdatedAt to be populated")
	}
}

func TestUpdateAutonomousRuntimeState_CreatesFile(t *testing.T) {
	missionDir := t.TempDir()
	_, err := updateAutonomousRuntimeState(missionDir, func(state *AutonomousRuntimeState) {
		state.LastSessionIDs[autonomousSessionKey("critic", "phase-A")] = "critic-session"
		state.FailureSignatures["F01|sig"] = 1
		state.RecoveryLevel["F01"] = 1
		state.CriticTransientAttempt["quest"] = 1
		state.CriticStructuralTry["quest"] = 1
		state.CriticAutoFixAttempt["quest"] = 1
		state.CriticBypassCount["quest"] = 1
	})
	if err != nil {
		t.Fatalf("update autonomous state: %v", err)
	}

	if !fileExists(filepath.Join(missionDir, "runs", autonomousStateFileName)) {
		t.Fatalf("expected autonomous-state.json file to be created")
	}

	loaded := loadAutonomousRuntimeState(missionDir)
	if loaded.LastSessionIDs[autonomousSessionKey("critic", "phase-A")] != "critic-session" {
		t.Fatalf("expected critic session in persisted state")
	}
}

func TestRewriteFixFeaturesForRoot_ReusesCanonicalIDs(t *testing.T) {
	wp := &WorkerPool{}
	sourceID := "F01-fix-legacy"
	rewritten := wp.rewriteFixFeaturesForRoot("F01", sourceID, []Feature{
		{
			ID:        "F01-fix-legacy-fix-1",
			Title:     "first",
			Phase:     1,
			DependsOn: []string{sourceID},
			Fixes:     sourceID,
		},
		{
			ID:        "F01-fix-legacy-fix-2",
			Title:     "second",
			Phase:     1,
			DependsOn: []string{"F01-fix-legacy-fix-1"},
			Fixes:     sourceID,
		},
	})

	if len(rewritten) != 2 {
		t.Fatalf("expected 2 rewritten fixes, got %d", len(rewritten))
	}
	if rewritten[0].ID != "F01-fix-01" || rewritten[1].ID != "F01-fix-02" {
		t.Fatalf("unexpected canonical IDs: %q %q", rewritten[0].ID, rewritten[1].ID)
	}
	if rewritten[0].Fixes != "F01" || rewritten[1].Fixes != "F01" {
		t.Fatalf("expected rewritten fixes to target root F01")
	}
	if len(rewritten[0].DependsOn) != 1 || rewritten[0].DependsOn[0] != "F01" {
		t.Fatalf("first fix should depend on root, got %+v", rewritten[0].DependsOn)
	}
	if len(rewritten[1].DependsOn) != 1 || rewritten[1].DependsOn[0] != "F01-fix-01" {
		t.Fatalf("second fix should depend on canonical first fix, got %+v", rewritten[1].DependsOn)
	}
	if rewritten[0].Status != "pending" || rewritten[1].Status != "pending" {
		t.Fatalf("rewritten fixes should default to pending status")
	}
}

func TestCriticReportNormalizedFailureSignature(t *testing.T) {
	report := &CriticReport{
		Findings: []CriticFinding{
			{Criterion: "J-D3", Status: "needs-work"},
			{Criterion: "J-D1", Status: "needs-work"},
			{Criterion: "J-D3", Status: "needs-work"},
		},
	}
	sig := report.NormalizedFailureSignature("fix-critic:F01")
	want := "fix-critic:f01|j-d1,j-d3"
	if sig != want {
		t.Fatalf("expected %q, got %q", want, sig)
	}
}

func TestRecordFailureSignature_IncrementsAndCapsRecoveryLevel(t *testing.T) {
	missionDir := t.TempDir()
	wp := &WorkerPool{
		missionDir:      missionDir,
		autonomousState: newAutonomousRuntimeState(),
	}

	var count, level int
	for i := 1; i <= 7; i++ {
		count, level = wp.recordFailureSignature("F01", "phase-1:f01")
		if count != i {
			t.Fatalf("expected count=%d, got %d", i, count)
		}
	}

	if level != 5 {
		t.Fatalf("expected capped recovery level=5, got %d", level)
	}

	loaded := loadAutonomousRuntimeState(missionDir)
	gotCount := loaded.FailureSignatures["F01|phase-1:f01"]
	if gotCount != 7 {
		t.Fatalf("expected persisted failure count=7, got %d", gotCount)
	}
	if gotLevel := loaded.RecoveryLevel["F01"]; gotLevel != 5 {
		t.Fatalf("expected persisted recovery level=5, got %d", gotLevel)
	}
}

func TestAutoFullResetAndRestart_ClearsFixesAndResetsRoots(t *testing.T) {
	missionDir := t.TempDir()
	featuresPath := filepath.Join(missionDir, "features.json")

	manifest := FeaturesManifest{
		Project: "demo",
		Owner:   "owner",
		Features: []Feature{
			{
				ID:         "F01",
				Title:      "root",
				Phase:      0,
				Status:     "done",
				Resolution: ResolutionResolvedViaFix,
				ResolvedBy: "F01-fix-01",
				ResolvedAt: "2026-01-01T00:00:00Z",
				Tainted:    true,
			},
		},
		FixFeatures: []Feature{
			{
				ID:         "F01-fix-01",
				Title:      "fix",
				Phase:      0,
				Status:     "blocked",
				Fixes:      "F01",
				Resolution: ResolutionUnresolved,
			},
		},
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if err := os.WriteFile(featuresPath, data, 0o644); err != nil {
		t.Fatalf("write features manifest: %v", err)
	}

	wp := &WorkerPool{
		missionDir: missionDir,
		workers:    map[string]*FeatureWorker{"F01": {Feature: manifest.Features[0], Status: WorkerDone}},
		phases:     map[int][]string{0: {"F01"}},
		eventCh:    make(chan WorkerEvent, 8),
		retries:    map[string]int{"F01": 2},
		transientRetries: map[string]int{
			"F01": 1,
		},
		validatorRetries: map[string]int{"F01": 1},
		refinementCount:  map[string]int{"F01": 1},
		phaseRetries:     map[int]int{0: 1},
		fixAttemptsByRoot: map[string]int{
			"F01": 3,
		},
		tainted: map[string]bool{"F01": true},
		stopped: true, // Prevent runPhase side effects in test.
		autonomousState: AutonomousRuntimeState{
			LastSessionIDs:    map[string]string{"worker:F01": "sess"},
			FailureSignatures: map[string]int{"F01|sig": 1},
			RecoveryLevel:     map[string]int{"F01": 2},
			AutoResetCount:    0,
		},
	}
	logger, err := NewMissionLogger(missionDir)
	if err != nil {
		t.Fatalf("create logger: %v", err)
	}
	defer logger.Close()
	wp.logger = logger

	if err := wp.autoFullResetAndRestart(0, ""); err != nil {
		t.Fatalf("autoFullResetAndRestart: %v", err)
	}

	resetData, err := os.ReadFile(featuresPath)
	if err != nil {
		t.Fatalf("read reset features manifest: %v", err)
	}
	var resetManifest FeaturesManifest
	if err := json.Unmarshal(resetData, &resetManifest); err != nil {
		t.Fatalf("unmarshal reset manifest: %v", err)
	}

	if len(resetManifest.FixFeatures) != 0 {
		t.Fatalf("expected fix_features to be cleared, got %d", len(resetManifest.FixFeatures))
	}
	if len(resetManifest.Features) != 1 {
		t.Fatalf("expected one root feature, got %d", len(resetManifest.Features))
	}

	root := resetManifest.Features[0]
	if root.Status != "pending" {
		t.Fatalf("expected root status pending after reset, got %q", root.Status)
	}
	if root.Resolution != "" || root.ResolvedBy != "" || root.ResolvedAt != "" || root.Tainted {
		t.Fatalf("expected root resolution metadata cleared after reset, got %+v", root)
	}

	if wp.autonomousState.AutoResetCount != 1 {
		t.Fatalf("expected auto reset count=1 in memory, got %d", wp.autonomousState.AutoResetCount)
	}
	loaded := loadAutonomousRuntimeState(missionDir)
	if loaded.AutoResetCount != 1 {
		t.Fatalf("expected persisted auto reset count=1, got %d", loaded.AutoResetCount)
	}
}

func TestCriticTransientReportClassification(t *testing.T) {
	report := &CriticReport{
		Findings: []CriticFinding{
			{Criterion: "phase-A-error", Status: "needs-work", Note: "phase timed out after 3m"},
		},
	}
	if !isCriticTransientReport(report) {
		t.Fatalf("expected timeout report to be classified as transient")
	}

	nonTransient := &CriticReport{
		Findings: []CriticFinding{
			{Criterion: "J-D3", Status: "needs-work", Note: "scope overlaps with existing fix"},
		},
	}
	if isCriticTransientReport(nonTransient) {
		t.Fatalf("expected decomposition report to be non-transient")
	}
}

func TestDecideCriticRecoveryAction_BoundedThenBypass(t *testing.T) {
	missionDir := t.TempDir()
	wp := &WorkerPool{
		missionDir:      missionDir,
		autonomousState: newAutonomousRuntimeState(),
	}
	report := &CriticReport{
		Findings: []CriticFinding{
			{Criterion: "J-D3", Status: "needs-work", Note: "duplicate scope"},
		},
	}

	for i := 1; i <= criticStructuralBudget; i++ {
		action, attempt, _ := wp.decideCriticRecoveryAction("quest", report)
		if action != criticRecoveryRetryStructural {
			t.Fatalf("expected structural recovery action at step %d, got %v", i, action)
		}
		if attempt != i {
			t.Fatalf("expected structural attempt=%d, got %d", i, attempt)
		}
	}

	action, bypassCount, _ := wp.decideCriticRecoveryAction("quest", report)
	if action != criticRecoveryBypass {
		t.Fatalf("expected critic bypass action after structural budget, got %v", action)
	}
	if bypassCount != 1 {
		t.Fatalf("expected first bypass count=1, got %d", bypassCount)
	}

	loaded := loadAutonomousRuntimeState(missionDir)
	if loaded.CriticStructuralTry["quest"] != criticStructuralBudget+1 {
		t.Fatalf("expected persisted structural attempts=%d, got %d", criticStructuralBudget+1, loaded.CriticStructuralTry["quest"])
	}
	if loaded.CriticBypassCount["quest"] != 1 {
		t.Fatalf("expected persisted bypass count=1, got %d", loaded.CriticBypassCount["quest"])
	}
}

func TestDecideCriticRecoveryAction_TransientThenStructural(t *testing.T) {
	missionDir := t.TempDir()
	wp := &WorkerPool{
		missionDir:      missionDir,
		autonomousState: newAutonomousRuntimeState(),
	}
	transientReport := &CriticReport{
		Findings: []CriticFinding{
			{Criterion: "phase-B-error", Status: "needs-work", Note: "socket connection was closed"},
		},
	}

	for i := 1; i <= criticTransientBudget; i++ {
		action, attempt, _ := wp.decideCriticRecoveryAction("quest", transientReport)
		if action != criticRecoveryRetryTransient {
			t.Fatalf("expected transient retry at %d, got %v", i, action)
		}
		if attempt != i {
			t.Fatalf("expected transient attempt=%d, got %d", i, attempt)
		}
	}

	action, attempt, _ := wp.decideCriticRecoveryAction("quest", transientReport)
	if action != criticRecoveryRetryStructural {
		t.Fatalf("expected structural escalation after transient budget, got %v", action)
	}
	if attempt != 1 {
		t.Fatalf("expected first structural attempt=1, got %d", attempt)
	}
}

func TestTryAutonomousCriticAutoFix_SuccessInvalidatesOnlyImpactedPhase(t *testing.T) {
	specDir := makeCriticSpecTree(t, map[string]string{
		"validation-contract.md": "# contract",
		"features.json":          `{"features":[],"fix_features":[]}`,
		"CLAUDE.md":              "# architecture",
		"project-context.md":     "# context",
	})
	missionDir := ResolveArtifactDir(specDir)
	rootID := "quest"

	state := newCriticPhaseState()
	state.Phases["A"] = criticPhaseCacheEntry{
		PhaseID:   "A",
		InputHash: "hA",
		Overall:   "pass",
		Report:    &CriticReport{Phase: "A", Overall: "pass"},
	}
	state.Phases["B"] = criticPhaseCacheEntry{
		PhaseID:   "B",
		InputHash: "hB",
		Overall:   "pass",
		Report:    &CriticReport{Phase: "B", Overall: "pass"},
	}
	state.Phases["C"] = criticPhaseCacheEntry{
		PhaseID:   "C",
		InputHash: "hC",
		Overall:   "pass",
		Report:    &CriticReport{Phase: "C", Overall: "pass"},
	}
	if err := saveCriticPhaseState(missionDir, state); err != nil {
		t.Fatalf("save critic phase state: %v", err)
	}

	logger, err := NewMissionLogger(missionDir)
	if err != nil {
		t.Fatalf("new mission logger: %v", err)
	}
	defer logger.Close()

	wp := &WorkerPool{
		missionDir:      missionDir,
		projectDir:      t.TempDir(),
		logger:          logger,
		eventCh:         make(chan WorkerEvent, 64),
		autonomousState: newAutonomousRuntimeState(),
	}
	wp.autonomousState.LastSessionIDs[autonomousSessionKey("critic", "phase-A")] = "sess-A"
	wp.autonomousState.LastSessionIDs[autonomousSessionKey("critic", "phase-B")] = "sess-B"
	wp.autonomousState.LastSessionIDs[autonomousSessionKey("critic", "phase-C")] = "sess-C"
	wp.criticAutoFixFn = func(report *CriticReport) error {
		// Simulate auto-fix touching only decomposition artifact.
		return os.WriteFile(filepath.Join(missionDir, "features.json"), []byte(`{"features":[{"id":"F01"}],"fix_features":[]}`), 0o644)
	}

	report := &CriticReport{
		Findings: []CriticFinding{
			{Criterion: "J-D2", Status: "needs-work", Note: "split feature"},
		},
	}

	rerun := wp.tryAutonomousCriticAutoFix(rootID, report)
	if !rerun {
		t.Fatalf("expected auto-fix success to force rerun")
	}

	cleared := loadCriticPhaseState(missionDir)
	if _, ok := cleared.Phases["C"]; ok {
		t.Fatalf("expected phase C invalidated after features change")
	}
	if _, ok := cleared.Phases["A"]; !ok {
		t.Fatalf("expected phase A cache preserved")
	}
	if _, ok := cleared.Phases["B"]; !ok {
		t.Fatalf("expected phase B cache preserved")
	}
	if got := wp.autonomousState.LastSessionIDs[autonomousSessionKey("critic", "phase-A")]; got != "sess-A" {
		t.Fatalf("expected phase-A session preserved")
	}
	if got := wp.autonomousState.LastSessionIDs[autonomousSessionKey("critic", "phase-B")]; got != "sess-B" {
		t.Fatalf("expected phase-B session preserved")
	}
	if _, ok := wp.autonomousState.LastSessionIDs[autonomousSessionKey("critic", "phase-C")]; ok {
		t.Fatalf("expected phase-C session cleared")
	}
	if got := wp.autonomousState.CriticAutoFixAttempt[rootID]; got != 1 {
		t.Fatalf("expected auto-fix attempt counter=1, got %d", got)
	}
}

func TestTryAutonomousCriticAutoFix_NoArtifactChangeKeepsCache(t *testing.T) {
	specDir := makeCriticSpecTree(t, map[string]string{
		"validation-contract.md": "# contract",
		"features.json":          `{"features":[],"fix_features":[]}`,
		"CLAUDE.md":              "# architecture",
		"project-context.md":     "# context",
	})
	missionDir := ResolveArtifactDir(specDir)
	rootID := "quest"

	state := newCriticPhaseState()
	state.Phases["A"] = criticPhaseCacheEntry{PhaseID: "A", InputHash: "hA", Overall: "pass", Report: &CriticReport{Phase: "A", Overall: "pass"}}
	state.Phases["B"] = criticPhaseCacheEntry{PhaseID: "B", InputHash: "hB", Overall: "pass", Report: &CriticReport{Phase: "B", Overall: "pass"}}
	state.Phases["C"] = criticPhaseCacheEntry{PhaseID: "C", InputHash: "hC", Overall: "pass", Report: &CriticReport{Phase: "C", Overall: "pass"}}
	if err := saveCriticPhaseState(missionDir, state); err != nil {
		t.Fatalf("save critic phase state: %v", err)
	}

	wp := &WorkerPool{
		missionDir:      missionDir,
		projectDir:      t.TempDir(),
		eventCh:         make(chan WorkerEvent, 64),
		autonomousState: newAutonomousRuntimeState(),
	}
	wp.autonomousState.LastSessionIDs[autonomousSessionKey("critic", "phase-A")] = "sess-A"
	wp.autonomousState.LastSessionIDs[autonomousSessionKey("critic", "phase-B")] = "sess-B"
	wp.autonomousState.LastSessionIDs[autonomousSessionKey("critic", "phase-C")] = "sess-C"
	wp.criticAutoFixFn = func(report *CriticReport) error { return nil }

	report := &CriticReport{
		Findings: []CriticFinding{
			{Criterion: "J-D2", Status: "needs-work", Note: "split feature"},
		},
	}

	rerun := wp.tryAutonomousCriticAutoFix(rootID, report)
	if !rerun {
		t.Fatalf("expected auto-fix success to continue rerun")
	}

	loaded := loadCriticPhaseState(missionDir)
	if len(loaded.Phases) != 3 {
		t.Fatalf("expected cache preserved when no artifact changed, got %d phases", len(loaded.Phases))
	}
	if got := wp.autonomousState.LastSessionIDs[autonomousSessionKey("critic", "phase-C")]; got != "sess-C" {
		t.Fatalf("expected phase-C session preserved when no invalidation")
	}
}

func TestTryAutonomousCriticAutoFix_BudgetExhaustedFallsBack(t *testing.T) {
	rootID := "quest"
	called := false

	wp := &WorkerPool{
		missionDir:      t.TempDir(),
		eventCh:         make(chan WorkerEvent, 64),
		autonomousState: newAutonomousRuntimeState(),
	}
	wp.autonomousState.CriticAutoFixAttempt[rootID] = criticAutoFixBudget
	wp.criticAutoFixFn = func(report *CriticReport) error {
		called = true
		return nil
	}

	report := &CriticReport{
		Findings: []CriticFinding{
			{Criterion: "J-S1", Status: "needs-work", Note: "assertion too vague"},
		},
	}

	rerun := wp.tryAutonomousCriticAutoFix(rootID, report)
	if rerun {
		t.Fatalf("expected no rerun when auto-fix budget is exhausted")
	}
	if called {
		t.Fatalf("expected auto-fix runner not to be called after budget exhaustion")
	}
	if got := wp.autonomousState.CriticAutoFixAttempt[rootID]; got != criticAutoFixBudget+1 {
		t.Fatalf("expected auto-fix attempt counter to increment to %d, got %d", criticAutoFixBudget+1, got)
	}
}

func TestTryAutonomousCriticAutoFix_AdvisoryOnlySkipsAutoFix(t *testing.T) {
	rootID := "quest"
	called := false

	wp := &WorkerPool{
		missionDir:      t.TempDir(),
		eventCh:         make(chan WorkerEvent, 64),
		autonomousState: newAutonomousRuntimeState(),
	}
	wp.criticAutoFixFn = func(report *CriticReport) error {
		called = true
		return nil
	}

	report := &CriticReport{
		Findings: []CriticFinding{
			{Criterion: "J-A1", Status: "needs-work", Note: "architecture docs can improve"},
		},
	}

	rerun := wp.tryAutonomousCriticAutoFix(rootID, report)
	if rerun {
		t.Fatalf("expected advisory-only report to skip auto-fix rerun")
	}
	if called {
		t.Fatalf("expected auto-fix runner not called for advisory-only findings")
	}
	if got := wp.autonomousState.CriticAutoFixAttempt[rootID]; got != 0 {
		t.Fatalf("expected auto-fix attempt counter to remain zero, got %d", got)
	}
}
