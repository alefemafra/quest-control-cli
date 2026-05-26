package internal

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
)

func BuildValidatorPrompt(feature Feature, missionDir, specDir string) string {
	validatorSkill := ReadSkill("mission-validator")

	contract := readFileContent(filepath.Join(missionDir, "validation-contract.md"))
	filtered := FilterContractAssertions(contract, feature.ValidationRefs)
	spec := readFileContent(filepath.Join(specDir, "spec.md"))
	knowledge := readFileContent(filepath.Join(missionDir, "knowledge-base.md"))
	projectContext := readFileContent(filepath.Join(missionDir, "project-context.md"))
	claudeMd := readFileContent(filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(specDir))), "CLAUDE.md"))

	var parts []string
	parts = append(parts,
		"You are running the mission-validator skill. Follow it precisely.",
		"",
		"## Skill Reference",
		"",
		validatorSkill,
		"",
		"---",
	)

	if projectContext != "" {
		parts = append(parts, "", "## Project Context", "", projectContext, "")
	}

	parts = append(parts,
		fmt.Sprintf("## Feature: %s — %s", feature.ID, feature.Title),
		fmt.Sprintf("Spec folder: %s", specDir),
		"",
		"Assertions to validate:",
	)
	for _, ref := range feature.ValidationRefs {
		parts = append(parts, "- "+ref)
	}

	parts = append(parts,
		"",
		"## CRITICAL: Black-box Rule",
		"",
		"DO NOT read any worker output, diff, PR, commits, or report.",
		"DO NOT read any file in mission/runs/*-worker.json.",
		"You MUST exercise the system independently and collect concrete evidence.",
		"",
		"## Validation Contract (filtered)",
		"",
		filtered,
		"",
	)

	if spec != "" {
		parts = append(parts, "## Spec", "", spec, "")
	}
	if claudeMd != "" {
		parts = append(parts, "## CLAUDE.md", "", claudeMd, "")
	}
	if knowledge != "" {
		parts = append(parts, "## Knowledge Base", "", CompactKnowledge(knowledge), "")
	}

	parts = append(parts,
		"## Instructions",
		"",
		"1. Read the assertions above",
		"2. Exercise the system as a real user (CLI, HTTP, DB queries, etc.)",
		"3. For each assertion: collect concrete evidence and decide PASS/FAIL/BLOCKED",
		"4. If you discover anything useful (gotchas, edge cases, environment quirks, ambiguities),",
		"   APPEND an entry to "+filepath.Join(missionDir, "knowledge-base.md")+" formatted as",
		"   `## YYYY-MM-DD — title`. Do NOT edit existing entries.",
		"5. Write the structured report to "+filepath.Join(missionDir, "runs", feature.ID)+"/",
		"",
		"After exercising the system, output ONLY a valid JSON object:",
		fmt.Sprintf(`{"feature_id":"%s","role":"validator","started_at":"<ISO>","ended_at":"<ISO>","verdict":"PASS|FAIL|BLOCKED","assertions":[{"id":"<ref>","result":"PASS|FAIL|BLOCKED","evidence":"<proof>"}],"notes":[]}`, feature.ID),
		"",
		"Set verdict to PASS only if ALL assertions pass. Set FAIL if ANY fails. Set BLOCKED if impossible to test.",
		"Output ONLY the JSON, nothing else.",
	)

	return strings.Join(parts, "\n")
}

func FilterContractAssertions(contract string, refs []string) string {
	if len(refs) == 0 {
		return contract
	}

	refSet := make(map[string]bool)
	prefixSet := make(map[string]bool)
	for _, r := range refs {
		refSet[r] = true
		if dot := strings.Index(r, "."); dot >= 0 {
			prefixSet[r[:dot]] = true
		}
	}

	var result strings.Builder
	var currentCategory string
	var categoryIncluded bool
	var categoryLines []string

	flush := func() {
		if categoryIncluded && len(categoryLines) > 0 {
			result.WriteString(fmt.Sprintf("## %s\n\n", currentCategory))
			for _, line := range categoryLines {
				result.WriteString(line + "\n")
			}
			result.WriteString("\n")
		}
		categoryLines = nil
		categoryIncluded = false
	}

	for _, line := range strings.Split(contract, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "## ") {
			flush()
			currentCategory = strings.TrimPrefix(trimmed, "## ")
			continue
		}
		if strings.HasPrefix(trimmed, "- **") {
			categoryLines = append(categoryLines, line)
			for _, ref := range refs {
				if strings.Contains(trimmed, ref) {
					categoryIncluded = true
					break
				}
			}
		}
	}
	flush()

	if result.Len() == 0 {
		return contract
	}
	return result.String()
}

func ParseValidatorReport(text string) *ValidatorReport {
	text = strings.TrimSpace(text)

	var report ValidatorReport
	if err := json.Unmarshal([]byte(text), &report); err == nil && report.Verdict != "" {
		return &report
	}

	// Try code fence extraction
	re := strings.Index(text, "```")
	if re >= 0 {
		end := strings.Index(text[re+3:], "```")
		if end >= 0 {
			block := text[re+3 : re+3+end]
			if nl := strings.Index(block, "\n"); nl >= 0 {
				block = block[nl+1:]
			}
			if err := json.Unmarshal([]byte(strings.TrimSpace(block)), &report); err == nil && report.Verdict != "" {
				return &report
			}
		}
	}

	// Fallback: scan for verdict keywords in the text
	upper := strings.ToUpper(text)
	if strings.Contains(upper, "\"VERDICT\"") || strings.Contains(upper, "VERDICT:") {
		if strings.Contains(upper, "FAIL") {
			return &ValidatorReport{Verdict: "FAIL"}
		}
		if strings.Contains(upper, "PASS") {
			return &ValidatorReport{Verdict: "PASS"}
		}
		if strings.Contains(upper, "BLOCKED") {
			return &ValidatorReport{Verdict: "BLOCKED"}
		}
	}

	return nil
}
