package internal

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- claude.go helpers ---

func TestExtractNumberedSection_FunctionalRequirements(t *testing.T) {
	spec := `# Spec — Events List

## Goal

Show users events.

## Functional Requirements

1. Render a paginated grid of events.
2. Filter by status (Draft / Published / Archived).
3. Search input debounced 250ms.

## Non-Functional Requirements

- Performance: < 200ms transitions.
`

	frs := extractNumberedSection(spec, "Functional Requirements")
	if len(frs) != 3 {
		t.Fatalf("expected 3 FRs, got %d: %#v", len(frs), frs)
	}
	if !strings.Contains(frs[0], "paginated grid") {
		t.Errorf("FR1 mismatch: %q", frs[0])
	}
	if !strings.Contains(frs[1], "Filter by status") {
		t.Errorf("FR2 mismatch: %q", frs[1])
	}
	if !strings.Contains(frs[2], "debounced 250ms") {
		t.Errorf("FR3 mismatch: %q", frs[2])
	}
}

func TestExtractNumberedSection_MultilineItems(t *testing.T) {
	spec := `## Functional Requirements

1. First requirement
   spans multiple lines
   for clarity.
2. Second requirement
   also multi-line.

## Next Section
`

	frs := extractNumberedSection(spec, "Functional Requirements")
	if len(frs) != 2 {
		t.Fatalf("expected 2 FRs, got %d: %#v", len(frs), frs)
	}
	if !strings.Contains(frs[0], "spans multiple lines") {
		t.Errorf("FR1 should join continuation lines: %q", frs[0])
	}
	if !strings.Contains(frs[0], "for clarity") {
		t.Errorf("FR1 should include all continuation lines: %q", frs[0])
	}
}

func TestExtractNumberedSection_MissingSection(t *testing.T) {
	spec := `## Goal

No FRs here.
`
	frs := extractNumberedSection(spec, "Functional Requirements")
	if len(frs) != 0 {
		t.Errorf("expected empty, got %d items: %#v", len(frs), frs)
	}
}

func TestExtractAPIEndpoints_Dedup(t *testing.T) {
	spec := `## API

- GET /api/events — list events
- POST /api/events — create event
- GET /api/events — listed again, should dedup
- DELETE /api/events/:id — remove

Body example:
` + "`PATCH /api/events/123`" + `
`
	endpoints := extractAPIEndpoints(spec)
	expected := map[string]bool{
		"GET /api/events":        true,
		"POST /api/events":       true,
		"DELETE /api/events/:id": true,
		"PATCH /api/events/123":  true,
	}

	if len(endpoints) != len(expected) {
		t.Errorf("expected %d unique endpoints, got %d: %#v", len(expected), len(endpoints), endpoints)
	}
	for _, ep := range endpoints {
		if !expected[ep] {
			t.Errorf("unexpected endpoint: %q", ep)
		}
	}
}

func TestExtractSpecStructure_IncludesAllSections(t *testing.T) {
	spec := `## Functional Requirements

1. List events.
2. Create event.

## Non-Functional Requirements

- Performance: < 200ms.
- A11y: AA contrast.

## API

- GET /api/events
- POST /api/events
`

	out := extractSpecStructure(spec)
	if !strings.Contains(out, "FR1:") || !strings.Contains(out, "FR2:") {
		t.Errorf("FRs missing: %q", out)
	}
	if !strings.Contains(out, "NFR1:") || !strings.Contains(out, "NFR2:") {
		t.Errorf("NFRs missing: %q", out)
	}
	if !strings.Contains(out, "GET /api/events") || !strings.Contains(out, "POST /api/events") {
		t.Errorf("endpoints missing: %q", out)
	}
}

func TestBuildAssertionsPrompt_NoRetryFeedback(t *testing.T) {
	tmpDir := t.TempDir()
	specDir := tmpDir + "/spec"
	if err := writeFile(specDir+"/spec.md", "## Functional Requirements\n\n1. Test FR.\n"); err != nil {
		t.Fatalf("write spec: %v", err)
	}

	out := BuildAssertionsPrompt(specDir, tmpDir, "")
	if !strings.Contains(out, "spec-to-assertions") {
		t.Error("should embed skill name/header")
	}
	if !strings.Contains(out, "Test FR.") {
		t.Error("should inline spec content")
	}
	if !strings.Contains(out, "FR1:") {
		t.Error("should append coverage requirements with FR1")
	}
	if strings.Contains(out, "Previous attempt had coverage gaps") {
		t.Error("should NOT include retry feedback when none provided")
	}
}

func TestBuildAssertionsPrompt_WithRetryFeedback(t *testing.T) {
	tmpDir := t.TempDir()
	specDir := tmpDir + "/spec"
	if err := writeFile(specDir+"/spec.md", "## Functional Requirements\n\n1. Test FR.\n"); err != nil {
		t.Fatalf("write spec: %v", err)
	}

	out := BuildAssertionsPrompt(specDir, tmpDir, "- FR1 has no matching assertion.")
	if !strings.Contains(out, "Previous attempt had coverage gaps") {
		t.Error("should include retry feedback header")
	}
	if !strings.Contains(out, "FR1 has no matching assertion") {
		t.Error("should embed feedback text")
	}
}

func TestBuildKnowledgePromptV2_RendersFeatures(t *testing.T) {
	tmpDir := t.TempDir()
	specDir := tmpDir + "/spec"
	if err := writeFile(specDir+"/spec.md", "## Goal\n\nTest spec.\n"); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	if err := writeFile(specDir+"/mission/codebase-analysis.md", "Reference pattern: foo\n"); err != nil {
		t.Fatalf("write analysis: %v", err)
	}
	if err := writeFile(specDir+"/mission/validation-contract.md", "## ui\n\n- **ui.1: Form renders**\n"); err != nil {
		t.Fatalf("write contract: %v", err)
	}

	features := []Feature{
		{ID: "F01", Title: "Schemas", Phase: 0, Scope: "Author Zod schemas at src/features/events/schema.ts.", ValidationRefs: []string{"data.1"}},
		{ID: "F02", Title: "Form", Phase: 1, Scope: "RHF form with Zod resolver under src/features/events/EventForm.tsx.", ValidationRefs: []string{"ui.1"}},
	}

	out := BuildKnowledgePromptV2(specDir, tmpDir, features, "")
	if !strings.Contains(out, "spec-to-knowledge") {
		t.Error("should embed knowledge skill")
	}
	if !strings.Contains(out, "F01 — Schemas") || !strings.Contains(out, "F02 — Form") {
		t.Errorf("should render feature ID + title, got: %q", out)
	}
	if !strings.Contains(out, "scope: Author Zod schemas") {
		t.Error("should render feature scope")
	}
	if !strings.Contains(out, "Reference pattern: foo") {
		t.Error("should inline codebase analysis")
	}
	if !strings.Contains(out, "ui.1: Form renders") {
		t.Error("should inline validation contract")
	}
}

func TestBuildFeaturesPrompt_RendersAssertionIDs(t *testing.T) {
	tmpDir := t.TempDir()
	specDir := tmpDir + "/spec"
	if err := writeFile(specDir+"/spec.md", "## Goal\n\nTest spec.\n"); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	if err := writeFile(specDir+"/mission/codebase-analysis.md", "Reference pattern: foo\n"); err != nil {
		t.Fatalf("write analysis: %v", err)
	}

	ids := map[string][]string{
		"ui":   {"ui.1", "ui.2"},
		"data": {"data.1"},
	}
	out := BuildFeaturesPrompt(specDir, tmpDir, ids, "")
	if !strings.Contains(out, "ui: ui.1, ui.2") {
		t.Errorf("should render ui category IDs: %q", out)
	}
	if !strings.Contains(out, "data: data.1") {
		t.Errorf("should render data category IDs: %q", out)
	}
	if !strings.Contains(out, "Reference pattern: foo") {
		t.Error("should inline codebase analysis")
	}
}

// --- mission.go parsers ---

func TestParseAssertionsOnlyJSON_ValidArray(t *testing.T) {
	input := `[
  {"category": "ui", "items": ["ui.1: Form renders", "ui.2: Errors shown"]},
  {"category": "data", "items": ["data.1: Schema valid"]}
]`
	assertions, ok := ParseAssertionsOnlyJSON(input)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if len(assertions) != 2 {
		t.Errorf("expected 2 categories, got %d", len(assertions))
	}
}

func TestParseAssertionsOnlyJSON_Empty(t *testing.T) {
	if _, ok := ParseAssertionsOnlyJSON(""); ok {
		t.Error("empty input should not be ok")
	}
	if _, ok := ParseAssertionsOnlyJSON("[]"); ok {
		t.Error("empty array should not be ok")
	}
}

func TestParseFeaturesOnlyJSON_WrappedObject(t *testing.T) {
	input := `{
  "features": [
    {"id": "F01", "title": "Schemas", "phase": 0, "scope": "Author Zod schemas under src/features/events/schema.ts covering all required fields and validation rules.", "validation_refs": ["data.1"]}
  ]
}`
	features, ok := ParseFeaturesOnlyJSON(input)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if len(features) != 1 || features[0].ID != "F01" {
		t.Errorf("unexpected features: %#v", features)
	}
}

func TestParseFeaturesOnlyJSON_BareArray(t *testing.T) {
	input := `[
  {"id": "F01", "title": "x", "phase": 0, "scope": "long enough scope describing what to do across multiple aspects of the feature.", "validation_refs": ["ui.1"]}
]`
	features, ok := ParseFeaturesOnlyJSON(input)
	if !ok {
		t.Fatal("expected ok=true for bare array fallback")
	}
	if len(features) != 1 {
		t.Errorf("expected 1 feature, got %d", len(features))
	}
}

func TestParseFeaturesOnlyJSON_WithCodeFence(t *testing.T) {
	input := "```json\n{\"features\":[{\"id\":\"F01\",\"title\":\"x\",\"phase\":0,\"scope\":\"long enough scope describing what to do across multiple aspects of the feature.\",\"validation_refs\":[\"ui.1\"]}]}\n```"
	features, ok := ParseFeaturesOnlyJSON(input)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if len(features) != 1 {
		t.Errorf("expected 1 feature, got %d", len(features))
	}
}

func TestParseFeaturesOnlyJSON_IgnoresUnexpectedKnowledgeKey(t *testing.T) {
	// If the model still emits a knowledge field, parser should accept the features part.
	input := `{
  "features": [{"id": "F01", "title": "x", "phase": 0, "scope": "long enough scope describing what to do across multiple aspects of the feature.", "validation_refs": ["ui.1"]}],
  "knowledge": ["should be ignored at this phase"]
}`
	features, ok := ParseFeaturesOnlyJSON(input)
	if !ok || len(features) != 1 {
		t.Errorf("expected 1 feature ignoring knowledge, got ok=%v features=%#v", ok, features)
	}
}

func TestCompactAssertionIDs_ExtractsIDs(t *testing.T) {
	assertions := []Assertion{
		{Category: "ui", Items: []string{"ui.1: form renders", "ui.2: errors shown", "trash without ID"}},
		{Category: "data", Items: []string{"data.1: schema valid"}},
		{Category: "", Items: []string{"x.1: ignored"}},
	}
	ids := CompactAssertionIDs(assertions)
	if len(ids["ui"]) != 2 || ids["ui"][0] != "ui.1" || ids["ui"][1] != "ui.2" {
		t.Errorf("ui IDs wrong: %#v", ids["ui"])
	}
	if len(ids["data"]) != 1 || ids["data"][0] != "data.1" {
		t.Errorf("data IDs wrong: %#v", ids["data"])
	}
	if _, ok := ids[""]; ok {
		t.Error("empty category should be skipped")
	}
}

// --- validation.go ---

func TestValidateAssertionsCoverage_AllCovered(t *testing.T) {
	spec := `## Functional Requirements

1. List events with pagination.
`
	assertions := []Assertion{
		{Category: "ui", Items: []string{"ui.1: User opens events page -> paginated list of events renders"}},
	}
	issues := validateAssertionsCoverage(spec, assertions)
	if len(issues) != 0 {
		t.Errorf("expected no issues, got %d: %v", len(issues), issues)
	}
}

func TestValidateAssertionsCoverage_MissingFR(t *testing.T) {
	spec := `## Functional Requirements

1. List events with pagination.
2. Search debounced input handler.
`
	assertions := []Assertion{
		{Category: "ui", Items: []string{"ui.1: Open events page -> paginated list renders"}},
	}
	issues := validateAssertionsCoverage(spec, assertions)
	if len(issues) == 0 {
		t.Fatal("expected at least 1 issue for missing FR2")
	}
	found := false
	for _, issue := range issues {
		if strings.Contains(issue, "FR2") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected FR2 issue, got: %v", issues)
	}
}

func TestValidateAssertionsCoverage_EndpointMissingErrorPath(t *testing.T) {
	spec := `## API

- POST /api/events
`
	assertions := []Assertion{
		{Category: "api", Items: []string{"api.1: POST /api/events with valid body returns 201 created"}},
	}
	issues := validateAssertionsCoverage(spec, assertions)
	hasErrorIssue := false
	for _, issue := range issues {
		if strings.Contains(issue, "error-path") && strings.Contains(issue, "POST /api/events") {
			hasErrorIssue = true
		}
	}
	if !hasErrorIssue {
		t.Errorf("expected error-path issue for POST /api/events, got: %v", issues)
	}
}

func TestValidateFeaturesCoverage_OrphanedID(t *testing.T) {
	features := []Feature{
		{ID: "F01", Scope: "Author Zod schemas covering required fields and validation rules across all entities.", ValidationRefs: []string{"ui.1"}},
	}
	knownIDs := map[string][]string{
		"ui":   {"ui.1", "ui.2"},
		"data": {"data.1"},
	}
	issues := validateFeaturesCoverage(features, knownIDs)
	if len(issues) == 0 {
		t.Fatal("expected orphan issues")
	}
	combined := strings.Join(issues, " ")
	if !strings.Contains(combined, "ui.2") || !strings.Contains(combined, "data.1") {
		t.Errorf("expected orphan IDs ui.2 and data.1, got: %v", issues)
	}
}

func TestValidateFeaturesCoverage_ShortScope(t *testing.T) {
	features := []Feature{
		{ID: "F01", Scope: "Too short.", ValidationRefs: []string{"ui.1"}},
	}
	issues := validateFeaturesCoverage(features, map[string][]string{"ui": {"ui.1"}})
	hasScopeIssue := false
	for _, issue := range issues {
		if strings.Contains(issue, "F01") && strings.Contains(issue, "scope is too short") {
			hasScopeIssue = true
		}
	}
	if !hasScopeIssue {
		t.Errorf("expected short scope issue, got: %v", issues)
	}
}

func TestValidateFeaturesCoverage_EmptyValidationRefs(t *testing.T) {
	features := []Feature{
		{ID: "F01", Scope: "Author Zod schemas covering required fields and validation rules across all entities.", ValidationRefs: []string{}},
	}
	issues := validateFeaturesCoverage(features, map[string][]string{"ui": {"ui.1"}})
	hasRefIssue := false
	for _, issue := range issues {
		if strings.Contains(issue, "F01") && strings.Contains(issue, "0 validation_refs") {
			hasRefIssue = true
		}
	}
	if !hasRefIssue {
		t.Errorf("expected empty refs issue, got: %v", issues)
	}
}

// writeFile writes content to path, creating parent directories as needed.
func writeFile(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o644)
}
