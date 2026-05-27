package internal

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func TestBuildCriticPhaseExecutionPlan_NoCacheRunsAll(t *testing.T) {
	specDir := makeCriticSpecTree(t, map[string]string{
		"validation-contract.md": "# contract v1\n- [data.1] one",
		"features.json":          `{"features":[],"fix_features":[]}`,
		"CLAUDE.md":              "# architecture v1",
		"project-context.md":     "# context v1",
	})
	missionDir := ResolveArtifactDir(specDir)
	phases := []criticPhase{criticPhaseSpec, criticPhaseArch, criticPhaseDecomp}

	plans, _ := buildCriticPhaseExecutionPlan(specDir, missionDir, phases)
	if len(plans) != len(phases) {
		t.Fatalf("expected %d plans, got %d", len(phases), len(plans))
	}
	for _, plan := range plans {
		if plan.ReuseCached {
			t.Fatalf("expected phase %s to rerun without cache", plan.Phase.ID)
		}
		if plan.CachedReport != nil {
			t.Fatalf("expected no cached report for phase %s", plan.Phase.ID)
		}
	}
}

func TestBuildCriticPhaseExecutionPlan_ReuseOnlyStablePassingPhases(t *testing.T) {
	specDir := makeCriticSpecTree(t, map[string]string{
		"validation-contract.md": "# contract v1\n- [data.1] one",
		"features.json":          `{"features":[],"fix_features":[]}`,
		"CLAUDE.md":              "# architecture v1",
		"project-context.md":     "# context v1",
	})
	missionDir := ResolveArtifactDir(specDir)
	phases := []criticPhase{criticPhaseSpec, criticPhaseArch, criticPhaseDecomp}

	state := newCriticPhaseState()
	state.Phases["A"] = criticPhaseCacheEntry{
		PhaseID:   "A",
		InputHash: computeCriticPhaseInputHash(specDir, criticPhaseSpec),
		Overall:   "pass",
		Report: &CriticReport{
			Phase:   "A",
			Overall: "pass",
		},
	}
	state.Phases["B"] = criticPhaseCacheEntry{
		PhaseID:   "B",
		InputHash: computeCriticPhaseInputHash(specDir, criticPhaseArch),
		Overall:   "pass",
		Report: &CriticReport{
			Phase:   "B",
			Overall: "pass",
		},
	}
	state.Phases["C"] = criticPhaseCacheEntry{
		PhaseID:   "C",
		InputHash: computeCriticPhaseInputHash(specDir, criticPhaseDecomp),
		Overall:   "needs-work",
		Report: &CriticReport{
			Phase:   "C",
			Overall: "needs-work",
		},
	}
	if err := saveCriticPhaseState(missionDir, state); err != nil {
		t.Fatalf("save critic phase state: %v", err)
	}

	plans, _ := buildCriticPhaseExecutionPlan(specDir, missionDir, phases)
	phaseReuse := map[string]bool{}
	for _, plan := range plans {
		phaseReuse[plan.Phase.ID] = plan.ReuseCached
	}

	if !phaseReuse["A"] {
		t.Fatalf("expected phase A to be reused")
	}
	if !phaseReuse["B"] {
		t.Fatalf("expected phase B to be reused")
	}
	if phaseReuse["C"] {
		t.Fatalf("expected phase C to rerun")
	}
}

func TestBuildCriticPhaseExecutionPlan_ContractChangeInvalidatesAAndC(t *testing.T) {
	specDir := makeCriticSpecTree(t, map[string]string{
		"validation-contract.md": "# contract v1\n- [data.1] one",
		"features.json":          `{"features":[],"fix_features":[]}`,
		"CLAUDE.md":              "# architecture v1",
		"project-context.md":     "# context v1",
	})
	missionDir := ResolveArtifactDir(specDir)
	phases := []criticPhase{criticPhaseSpec, criticPhaseArch, criticPhaseDecomp}

	state := newCriticPhaseState()
	for _, phase := range phases {
		state.Phases[phase.ID] = criticPhaseCacheEntry{
			PhaseID:   phase.ID,
			InputHash: computeCriticPhaseInputHash(specDir, phase),
			Overall:   "pass",
			Report: &CriticReport{
				Phase:   phase.ID,
				Overall: "pass",
			},
		}
	}
	if err := saveCriticPhaseState(missionDir, state); err != nil {
		t.Fatalf("save critic phase state: %v", err)
	}

	contractPath := filepath.Join(missionDir, "validation-contract.md")
	if err := os.WriteFile(contractPath, []byte("# contract v2\n- [data.1] changed\n"), 0o644); err != nil {
		t.Fatalf("write contract: %v", err)
	}

	plans, _ := buildCriticPhaseExecutionPlan(specDir, missionDir, phases)
	phaseReuse := map[string]bool{}
	for _, plan := range plans {
		phaseReuse[plan.Phase.ID] = plan.ReuseCached
	}

	if phaseReuse["A"] {
		t.Fatalf("expected phase A to rerun after contract change")
	}
	if !phaseReuse["B"] {
		t.Fatalf("expected phase B to be reused after contract-only change")
	}
	if phaseReuse["C"] {
		t.Fatalf("expected phase C to rerun after contract change")
	}
}

func TestBuildCriticPhaseExecutionPlan_ArchitectureChangeInvalidatesOnlyB(t *testing.T) {
	specDir := makeCriticSpecTree(t, map[string]string{
		"validation-contract.md": "# contract v1\n- [data.1] one",
		"features.json":          `{"features":[],"fix_features":[]}`,
		"CLAUDE.md":              "# architecture v1",
		"project-context.md":     "# context v1",
	})
	missionDir := ResolveArtifactDir(specDir)
	phases := []criticPhase{criticPhaseSpec, criticPhaseArch, criticPhaseDecomp}

	state := newCriticPhaseState()
	for _, phase := range phases {
		state.Phases[phase.ID] = criticPhaseCacheEntry{
			PhaseID:   phase.ID,
			InputHash: computeCriticPhaseInputHash(specDir, phase),
			Overall:   "pass",
			Report: &CriticReport{
				Phase:   phase.ID,
				Overall: "pass",
			},
		}
	}
	if err := saveCriticPhaseState(missionDir, state); err != nil {
		t.Fatalf("save critic phase state: %v", err)
	}

	projectRoot := filepath.Dir(filepath.Dir(filepath.Dir(specDir)))
	claudePath := filepath.Join(projectRoot, "CLAUDE.md")
	if err := os.WriteFile(claudePath, []byte("# architecture v2\n"), 0o644); err != nil {
		t.Fatalf("write CLAUDE.md: %v", err)
	}

	plans, _ := buildCriticPhaseExecutionPlan(specDir, missionDir, phases)
	phaseReuse := map[string]bool{}
	for _, plan := range plans {
		phaseReuse[plan.Phase.ID] = plan.ReuseCached
	}

	if !phaseReuse["A"] {
		t.Fatalf("expected phase A to be reused after architecture-only change")
	}
	if phaseReuse["B"] {
		t.Fatalf("expected phase B to rerun after architecture change")
	}
	if !phaseReuse["C"] {
		t.Fatalf("expected phase C to be reused after architecture-only change")
	}
}

func TestBuildCriticPhaseExecutionPlan_CorruptCacheFallsBackToRerunAll(t *testing.T) {
	specDir := makeCriticSpecTree(t, map[string]string{
		"validation-contract.md": "# contract v1\n- [data.1] one",
		"features.json":          `{"features":[],"fix_features":[]}`,
		"CLAUDE.md":              "# architecture v1",
		"project-context.md":     "# context v1",
	})
	missionDir := ResolveArtifactDir(specDir)
	phases := []criticPhase{criticPhaseSpec, criticPhaseArch, criticPhaseDecomp}

	if err := os.MkdirAll(filepath.Join(missionDir, "runs"), 0o755); err != nil {
		t.Fatalf("mkdir runs: %v", err)
	}
	if err := os.WriteFile(criticPhaseStatePath(missionDir), []byte("{not-json"), 0o644); err != nil {
		t.Fatalf("write corrupt state: %v", err)
	}

	plans, _ := buildCriticPhaseExecutionPlan(specDir, missionDir, phases)
	for _, plan := range plans {
		if plan.ReuseCached {
			t.Fatalf("expected phase %s to rerun when cache is corrupt", plan.Phase.ID)
		}
	}
}

func TestCaptureCriticArtifactSnapshot_DetectsFeatureOnlyChange(t *testing.T) {
	specDir := makeCriticSpecTree(t, map[string]string{
		"validation-contract.md": "# contract",
		"features.json":          `{"features":[],"fix_features":[]}`,
		"CLAUDE.md":              "# claude",
		"project-context.md":     "# context",
	})
	missionDir := ResolveArtifactDir(specDir)

	before, err := captureCriticArtifactSnapshot(missionDir)
	if err != nil {
		t.Fatalf("capture before snapshot: %v", err)
	}
	if err := os.WriteFile(filepath.Join(missionDir, "features.json"), []byte(`{"features":[{"id":"F01"}],"fix_features":[]}`), 0o644); err != nil {
		t.Fatalf("rewrite features: %v", err)
	}
	after, err := captureCriticArtifactSnapshot(missionDir)
	if err != nil {
		t.Fatalf("capture after snapshot: %v", err)
	}

	changed, err := changedCriticArtifacts(before, after)
	if err != nil {
		t.Fatalf("diff snapshots: %v", err)
	}
	if len(changed) != 1 || changed[0] != criticArtifactFeatures {
		t.Fatalf("expected only features artifact changed, got %v", changed)
	}
}

func TestDetermineCriticPhaseInvalidation_FromChangedArtifacts(t *testing.T) {
	phases, fallback := determineCriticPhaseInvalidation([]string{criticArtifactFeatures})
	if fallback {
		t.Fatalf("did not expect fallback for known feature artifact")
	}
	if len(phases) != 1 || phases[0] != "C" {
		t.Fatalf("expected only phase C invalidation, got %v", phases)
	}

	phases, fallback = determineCriticPhaseInvalidation([]string{criticArtifactValidationContract})
	if fallback {
		t.Fatalf("did not expect fallback for known contract artifact")
	}
	if len(phases) != 2 || phases[0] != "A" || phases[1] != "C" {
		t.Fatalf("expected phases A and C invalidated, got %v", phases)
	}
}

func TestDetermineCriticPhaseInvalidation_UnknownArtifactFallsBack(t *testing.T) {
	phases, fallback := determineCriticPhaseInvalidation([]string{"unknown-artifact"})
	if !fallback {
		t.Fatalf("expected fallback for unknown artifact")
	}
	if len(phases) != 0 {
		t.Fatalf("expected no explicit phase list on fallback, got %v", phases)
	}
}

func TestInvalidateCriticPhaseExecutionState_OnlyRemovesTargetedPhase(t *testing.T) {
	specDir := makeCriticSpecTree(t, map[string]string{
		"validation-contract.md": "# contract",
		"features.json":          `{"features":[],"fix_features":[]}`,
		"CLAUDE.md":              "# claude",
		"project-context.md":     "# context",
	})
	missionDir := ResolveArtifactDir(specDir)

	state := newCriticPhaseState()
	state.Phases["A"] = criticPhaseCacheEntry{PhaseID: "A", InputHash: "hash-A", Overall: "pass", Report: &CriticReport{Phase: "A", Overall: "pass"}}
	state.Phases["B"] = criticPhaseCacheEntry{PhaseID: "B", InputHash: "hash-B", Overall: "pass", Report: &CriticReport{Phase: "B", Overall: "pass"}}
	state.Phases["C"] = criticPhaseCacheEntry{PhaseID: "C", InputHash: "hash-C", Overall: "pass", Report: &CriticReport{Phase: "C", Overall: "pass"}}
	if err := saveCriticPhaseState(missionDir, state); err != nil {
		t.Fatalf("save critic phase state: %v", err)
	}

	if err := invalidateCriticPhaseExecutionState(missionDir, []string{"C"}); err != nil {
		t.Fatalf("invalidate critic phase state: %v", err)
	}

	loaded := loadCriticPhaseState(missionDir)
	if _, ok := loaded.Phases["C"]; ok {
		t.Fatalf("expected phase C removed from cache")
	}
	keys := make([]string, 0, len(loaded.Phases))
	for k := range loaded.Phases {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) != 2 || keys[0] != "A" || keys[1] != "B" {
		t.Fatalf("expected phases A/B preserved, got %v", keys)
	}
}
