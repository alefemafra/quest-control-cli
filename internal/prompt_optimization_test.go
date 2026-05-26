package internal

import (
	"strings"
	"testing"
)

func TestCompactKnowledge_SimpleEntries(t *testing.T) {
	input := `# Knowledge Base — TestProject

Workers and validators accumulate findings here.

## How to contribute

Each entry starts with ` + "`## YYYY-MM-DD — short title`" + `.
Workers and validators APPEND; they DO NOT edit others' entries.

---

- Use @sitickets/datetime for UTC conversion
- activeTenant is a string, not an object
- The events API requires pagination params`

	result := CompactKnowledge(input)

	if strings.Contains(result, "# Knowledge Base") {
		t.Error("should strip title header")
	}
	if strings.Contains(result, "How to contribute") {
		t.Error("should strip How to contribute section")
	}
	if strings.Contains(result, "---") {
		t.Error("should strip separators")
	}
	if strings.Contains(result, "accumulate findings") {
		t.Error("should strip metadata paragraph")
	}
	if !strings.Contains(result, "Use @sitickets/datetime") {
		t.Error("should keep knowledge entries")
	}
	if !strings.Contains(result, "activeTenant is a string") {
		t.Error("should keep knowledge entries")
	}
	if !strings.Contains(result, "events API requires pagination") {
		t.Error("should keep knowledge entries")
	}

	lines := strings.Split(result, "\n")
	if len(lines) != 3 {
		t.Errorf("expected 3 lines, got %d: %q", len(lines), result)
	}
}

func TestCompactKnowledge_DatedEntries(t *testing.T) {
	input := `# Knowledge Base — boe-plus

Workers and validators accumulate findings here.

## How to contribute

Each entry starts with heading.

---

- Schema uses UUID for primary keys
- Use barrel exports from src/modules/core/

## 2026-05-18 — Adoption of Missions Architecture

Project pre-existed. Stack: TypeScript monorepo with NestJS backend.`

	result := CompactKnowledge(input)

	if strings.Contains(result, "2026-05-18") {
		t.Error("should strip dated headers")
	}
	if !strings.Contains(result, "Schema uses UUID") {
		t.Error("should keep dash-prefixed entries")
	}
	if !strings.Contains(result, "barrel exports") {
		t.Error("should keep dash-prefixed entries")
	}
	if !strings.Contains(result, "Project pre-existed") {
		t.Error("should keep body text under dated headers")
	}
}

func TestCompactKnowledge_Empty(t *testing.T) {
	result := CompactKnowledge("")
	if result != "" {
		t.Errorf("expected empty string, got %q", result)
	}
}

func TestCompactKnowledge_FallbackWhenNoEntries(t *testing.T) {
	input := `# Knowledge Base

Some metadata only, no actual entries.`

	result := CompactKnowledge(input)
	if result != input {
		t.Errorf("should return original when compaction strips everything, got %q", result)
	}
}

func TestFilterContractAssertions_FiltersByCategory(t *testing.T) {
	contract := `# Validation Contract — Test

## ui

- **ui.1: Form renders with all required fields**
- **ui.2: Error messages display on invalid input**
- **ui.3: Event list renders with pagination**

## data

- **data.1: API returns paginated response**
- **data.2: Schema validates required fields**

## perf

- **perf.1: Page loads under 200ms**
`

	refs := []string{"ui.3", "data.1"}
	result := FilterContractAssertions(contract, refs)

	// FilterContractAssertions includes entire categories when any assertion matches
	if !strings.Contains(result, "ui.1") {
		t.Error("should contain ui.1 (entire ui category included because ui.3 matched)")
	}
	if !strings.Contains(result, "ui.3") {
		t.Error("should contain ui.3")
	}
	if !strings.Contains(result, "data.1") {
		t.Error("should contain data.1")
	}
	if !strings.Contains(result, "data.2") {
		t.Error("should contain data.2 (entire data category included because data.1 matched)")
	}
	// perf category has no matching refs — should be excluded
	if strings.Contains(result, "perf.1") {
		t.Error("should not contain perf.1 (perf category has no matching refs)")
	}
}

func TestFilterContractAssertions_EmptyRefsReturnsFull(t *testing.T) {
	contract := `## ui

- **ui.1: Form renders**
`
	result := FilterContractAssertions(contract, nil)
	if result != contract {
		t.Error("empty refs should return full contract")
	}
}

func TestFilterContractAssertions_NoMatchReturnsFull(t *testing.T) {
	contract := `## ui

- **ui.1: Form renders**
`
	result := FilterContractAssertions(contract, []string{"nonexistent.99"})
	if !strings.Contains(result, "ui.1") {
		t.Error("no-match should fall back to full contract")
	}
}
