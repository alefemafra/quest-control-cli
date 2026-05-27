package internal

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// makeCriticSpecTree builds a minimal docs/specs/<slug>/ layout that mirrors
// what BuildCriticPhasePrompt expects, returning the specDir absolute path.
// The caller passes a list of files (relative to the layout) to actually
// create on disk so each test can control which artifacts are present.
func makeCriticSpecTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	specDir := filepath.Join(root, "docs", "specs", "feature")
	artifactDir := filepath.Join(specDir, "quest")
	if err := os.MkdirAll(artifactDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for name, content := range files {
		var dest string
		switch name {
		case "CLAUDE.md":
			dest = filepath.Join(root, "CLAUDE.md")
		default:
			dest = filepath.Join(artifactDir, name)
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			t.Fatalf("mkdir parent: %v", err)
		}
		if err := os.WriteFile(dest, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return specDir
}

// resolveArtifactPaths mirrors how BuildCriticPhasePrompt builds absolute
// paths so tests can check inclusion / exclusion against the actual on-disk
// path. This isolates assertions from the embedded skill markdown, which
// also mentions these filenames.
func resolveArtifactPaths(t *testing.T, specDir string) (contract, features, claudeMd, projCtx string) {
	t.Helper()
	missionDir := ResolveArtifactDir(specDir)
	projectRoot := filepath.Dir(filepath.Dir(filepath.Dir(specDir)))
	contract = filepath.Join(missionDir, "validation-contract.md")
	features = filepath.Join(missionDir, "features.json")
	claudeMd = filepath.Join(projectRoot, "CLAUDE.md")
	projCtx = filepath.Join(missionDir, "project-context.md")
	return
}

func TestBuildCriticPhasePrompt_A_OnlyContract(t *testing.T) {
	specDir := makeCriticSpecTree(t, map[string]string{
		"validation-contract.md": "# contract",
		"features.json":          "{}",
		"CLAUDE.md":              "# claude",
		"project-context.md":     "# ctx",
	})
	contract, features, claudeMd, projCtx := resolveArtifactPaths(t, specDir)

	prompt := BuildCriticPhasePrompt(specDir, criticPhaseSpec)

	if !strings.Contains(prompt, contract) {
		t.Errorf("phase A prompt should list %s in Files to Read\n%s", contract, prompt)
	}
	if strings.Contains(prompt, features) {
		t.Errorf("phase A prompt should NOT list %s", features)
	}
	if strings.Contains(prompt, claudeMd) {
		t.Errorf("phase A prompt should NOT list %s", claudeMd)
	}
	if strings.Contains(prompt, projCtx) {
		t.Errorf("phase A prompt should NOT list %s", projCtx)
	}
	if !strings.Contains(prompt, "Phase A") {
		t.Errorf("phase A prompt should announce Phase A in the framing")
	}
	if !strings.Contains(prompt, "J-S1..J-S6") {
		t.Errorf("phase A prompt should include the J-S1..J-S6 criteria label")
	}
}

func TestBuildCriticPhasePrompt_B_OnlyArchitecture(t *testing.T) {
	specDir := makeCriticSpecTree(t, map[string]string{
		"validation-contract.md": "# contract",
		"features.json":          "{}",
		"CLAUDE.md":              "# claude",
		"project-context.md":     "# ctx",
	})
	contract, features, claudeMd, projCtx := resolveArtifactPaths(t, specDir)

	prompt := BuildCriticPhasePrompt(specDir, criticPhaseArch)

	if !strings.Contains(prompt, claudeMd) {
		t.Errorf("phase B prompt should list %s", claudeMd)
	}
	if !strings.Contains(prompt, projCtx) {
		t.Errorf("phase B prompt should list %s", projCtx)
	}
	if strings.Contains(prompt, features) {
		t.Errorf("phase B prompt should NOT list %s", features)
	}
	if strings.Contains(prompt, contract) {
		t.Errorf("phase B prompt should NOT list %s", contract)
	}
	if !strings.Contains(prompt, "Phase B") {
		t.Errorf("phase B prompt should announce Phase B in the framing")
	}
	if !strings.Contains(prompt, "J-A1..J-A6") {
		t.Errorf("phase B prompt should include the J-A1..J-A6 criteria label")
	}
}

func TestBuildCriticPhasePrompt_C_OnlyDecomp(t *testing.T) {
	specDir := makeCriticSpecTree(t, map[string]string{
		"validation-contract.md": "# contract",
		"features.json":          "{}",
		"CLAUDE.md":              "# claude",
		"project-context.md":     "# ctx",
	})
	contract, features, claudeMd, projCtx := resolveArtifactPaths(t, specDir)

	prompt := BuildCriticPhasePrompt(specDir, criticPhaseDecomp)

	if !strings.Contains(prompt, features) {
		t.Errorf("phase C prompt should list %s", features)
	}
	if !strings.Contains(prompt, contract) {
		t.Errorf("phase C prompt should list %s", contract)
	}
	if strings.Contains(prompt, claudeMd) {
		t.Errorf("phase C prompt should NOT list %s", claudeMd)
	}
	if strings.Contains(prompt, projCtx) {
		t.Errorf("phase C prompt should NOT list %s", projCtx)
	}
	if !strings.Contains(prompt, "Phase C") {
		t.Errorf("phase C prompt should announce Phase C in the framing")
	}
	if !strings.Contains(prompt, "J-D1..J-D6") {
		t.Errorf("phase C prompt should include the J-D1..J-D6 criteria label")
	}
}

func TestBuildCriticPhasePrompt_InlinesArtifactContent(t *testing.T) {
	specDir := makeCriticSpecTree(t, map[string]string{
		"validation-contract.md": "ASSERTION-MARKER-XYZ\n",
		"features.json":          "FEATURE-MARKER-XYZ\n",
		"CLAUDE.md":              "CLAUDE-MARKER-XYZ\n",
		"project-context.md":     "CTX-MARKER-XYZ\n",
	})

	cases := []struct {
		name     string
		phase    criticPhase
		expects  []string
		excludes []string
	}{
		{
			name:     "phase A inlines contract only",
			phase:    criticPhaseSpec,
			expects:  []string{"ASSERTION-MARKER-XYZ"},
			excludes: []string{"FEATURE-MARKER-XYZ", "CLAUDE-MARKER-XYZ", "CTX-MARKER-XYZ"},
		},
		{
			name:     "phase B inlines architecture artifacts only",
			phase:    criticPhaseArch,
			expects:  []string{"CLAUDE-MARKER-XYZ", "CTX-MARKER-XYZ"},
			excludes: []string{"FEATURE-MARKER-XYZ", "ASSERTION-MARKER-XYZ"},
		},
		{
			name:     "phase C inlines features + contract only",
			phase:    criticPhaseDecomp,
			expects:  []string{"FEATURE-MARKER-XYZ", "ASSERTION-MARKER-XYZ"},
			excludes: []string{"CLAUDE-MARKER-XYZ", "CTX-MARKER-XYZ"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prompt := BuildCriticPhasePrompt(specDir, tc.phase)
			for _, want := range tc.expects {
				if !strings.Contains(prompt, want) {
					t.Errorf("expected %q to be inlined into the prompt", want)
				}
			}
			for _, dont := range tc.excludes {
				if strings.Contains(prompt, dont) {
					t.Errorf("did not expect %q to appear in the prompt", dont)
				}
			}
			if !strings.Contains(prompt, "All artifacts are inlined above") {
				t.Errorf("prompt should tell the model not to use Read")
			}
		})
	}
}

func TestMergeCriticReports_AllPass(t *testing.T) {
	parts := []*CriticReport{
		{
			Phase:   "A",
			Overall: "pass",
			Findings: []CriticFinding{
				{Criterion: "J-S1", Status: "pass"},
				{Criterion: "J-S2", Status: "pass"},
			},
		},
		{
			Phase:   "B",
			Overall: "pass",
			Findings: []CriticFinding{
				{Criterion: "J-A1", Status: "pass"},
			},
		},
		{
			Phase:   "C",
			Overall: "pass",
			Findings: []CriticFinding{
				{Criterion: "J-D1", Status: "pass"},
			},
		},
	}
	mech := &MechanicalResult{Passed: 5, Failed: 0}

	merged := mergeCriticReports(parts, []error{nil, nil, nil}, mech)

	if merged.Overall != "pass" {
		t.Fatalf("expected merged Overall=pass, got %q", merged.Overall)
	}
	if merged.Phase != "all" {
		t.Errorf("expected Phase=all, got %q", merged.Phase)
	}
	if merged.MechanicalPassed != 5 || merged.MechanicalFailed != 0 {
		t.Errorf("mechanical totals wrong: %+v", merged)
	}
	if len(merged.Findings) != 4 {
		t.Fatalf("expected 4 findings (union), got %d: %+v", len(merged.Findings), merged.Findings)
	}
	want := []string{"J-S1", "J-S2", "J-A1", "J-D1"}
	for i, f := range merged.Findings {
		if f.Criterion != want[i] {
			t.Errorf("findings out of order: at %d expected %s got %s", i, want[i], f.Criterion)
		}
	}
	if len(merged.BlockingFindings) != 0 {
		t.Errorf("no blockers expected when all pass, got %v", merged.BlockingFindings)
	}
}

func TestMergeCriticReports_OneFails(t *testing.T) {
	parts := []*CriticReport{
		{Phase: "A", Overall: "pass", Findings: []CriticFinding{{Criterion: "J-S1", Status: "pass"}}},
		{
			Phase:   "B",
			Overall: "needs-work",
			Findings: []CriticFinding{
				{Criterion: "J-A2", Status: "needs-work", Suggestion: "tighten boundaries"},
			},
			BlockingFindings: []string{"J-A2"},
		},
		{Phase: "C", Overall: "pass", Findings: []CriticFinding{{Criterion: "J-D1", Status: "pass"}}},
	}

	merged := mergeCriticReports(parts, []error{nil, nil, nil}, &MechanicalResult{Passed: 5})

	if merged.Overall != "needs-work" {
		t.Fatalf("expected needs-work, got %q", merged.Overall)
	}
	if len(merged.BlockingFindings) != 1 || merged.BlockingFindings[0] != "J-A2" {
		t.Errorf("expected single blocker J-A2, got %v", merged.BlockingFindings)
	}
}

func TestMergeCriticReports_OneErrored(t *testing.T) {
	parts := []*CriticReport{
		{Phase: "A", Overall: "pass", Findings: []CriticFinding{{Criterion: "J-S1", Status: "pass"}}},
		nil,
		{Phase: "C", Overall: "pass", Findings: []CriticFinding{{Criterion: "J-D1", Status: "pass"}}},
	}
	errs := []error{nil, errors.New("subprocess crashed"), nil}

	merged := mergeCriticReports(parts, errs, &MechanicalResult{Passed: 5})

	if merged.Overall != "needs-work" {
		t.Fatalf("expected needs-work when a phase errored, got %q", merged.Overall)
	}

	var hasSynthetic bool
	for _, f := range merged.Findings {
		if f.Criterion == "phase-B-error" && strings.Contains(f.Note, "subprocess crashed") {
			hasSynthetic = true
		}
	}
	if !hasSynthetic {
		t.Errorf("expected synthetic phase-B-error finding, got %+v", merged.Findings)
	}

	var hasSyntheticBlock bool
	for _, id := range merged.BlockingFindings {
		if id == "phase-B-error" {
			hasSyntheticBlock = true
		}
	}
	if !hasSyntheticBlock {
		t.Errorf("expected phase-B-error in BlockingFindings, got %v", merged.BlockingFindings)
	}
}
