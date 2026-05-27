package internal

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

func BuildValidatorPrompt(feature Feature, missionDir, specDir string) string {
	validatorSkill := ReadSkill("quest-validator")

	contract := readFileContent(filepath.Join(missionDir, "validation-contract.md"))
	filtered := FilterContractAssertions(contract, feature.ValidationRefs)
	spec := readFileContent(filepath.Join(specDir, "spec.md"))
	knowledge := readFileContent(filepath.Join(missionDir, "knowledge-base.md"))
	projectContext := readFileContent(filepath.Join(missionDir, "project-context.md"))
	claudeMd := readFileContent(filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(specDir))), "CLAUDE.md"))

	var parts []string
	parts = append(parts,
		"You are running the quest-validator skill. Follow it precisely.",
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
		"0. Run lint and unit tests for this project using Project Context / CLAUDE.md commands. Both must pass to allow final PASS.",
		"1. Read the assertions above",
		"2. Exercise the system as a real user (CLI, HTTP, DB queries, browser/UI flows via DevTools MCP + Claude Chrome when applicable)",
		"3. For each assertion: collect concrete evidence and decide PASS/FAIL/BLOCKED",
		"4. If lint or unit tests fail, include evidence and set verdict to FAIL.",
		"5. If you discover anything useful (gotchas, edge cases, environment quirks, ambiguities),",
		"   APPEND an entry to "+filepath.Join(missionDir, "knowledge-base.md")+" formatted as",
		"   `## YYYY-MM-DD — title`. Do NOT edit existing entries.",
		"6. Write the structured report to "+filepath.Join(missionDir, "runs", feature.ID)+"/",
		"",
		"After exercising the system, output ONLY a valid JSON object:",
		fmt.Sprintf(`{"feature_id":"%s","role":"validator","started_at":"<ISO>","ended_at":"<ISO>","verdict":"PASS|FAIL|BLOCKED","assertions":[{"id":"<ref>","result":"PASS|FAIL|BLOCKED","evidence":"<proof>"}],"notes":[]}`, feature.ID),
		"",
		"Set verdict to PASS only if lint + unit tests pass AND ALL assertions pass. Set FAIL if ANY fails. Set BLOCKED if impossible to test.",
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
		// #region agent log
		emitDebugLog(debugRunIDValidatorVerdictV1, "H1", "internal/validator.go:ParseValidatorReport:directJSON", "validator_parse_direct_json_success", map[string]any{
			"verdict":    report.Verdict,
			"textLen":    len(text),
			"assertions": len(report.Assertions),
			"notes":      len(report.Notes),
		})
		// #endregion
		return &report
	}

	// #region agent log
	emitDebugLog(debugRunIDValidatorVerdictV1, "H1", "internal/validator.go:ParseValidatorReport:directJSON", "validator_parse_direct_json_failed", map[string]any{
		"textLen":            len(text),
		"hasCodeFence":       strings.Contains(text, "```"),
		"containsVerdictKey": strings.Contains(strings.ToUpper(text), "\"VERDICT\""),
		"containsPASS":       strings.Contains(strings.ToUpper(text), "PASS"),
		"containsFAIL":       strings.Contains(strings.ToUpper(text), "FAIL"),
	})
	// #endregion

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
				// #region agent log
				emitDebugLog(debugRunIDValidatorVerdictV1, "H1", "internal/validator.go:ParseValidatorReport:codeFence", "validator_parse_code_fence_success", map[string]any{
					"verdict": report.Verdict,
				})
				// #endregion
				return &report
			}
		}
	}

	// Try extracting a JSON object from mixed prose + JSON output.
	// Claude sometimes prepends narration before the final object.
	if idx := strings.Index(text, "{"); idx >= 0 {
		candidate := strings.TrimSpace(text[idx:])
		dec := json.NewDecoder(strings.NewReader(candidate))
		var embedded ValidatorReport
		if err := dec.Decode(&embedded); err == nil && embedded.Verdict != "" {
			// #region agent log
			emitDebugLog(debugRunIDValidatorVerdictV1, "H2", "internal/validator.go:ParseValidatorReport:embeddedJSON", "validator_parse_embedded_json_success", map[string]any{
				"verdict": embedded.Verdict,
				"textLen": len(text),
			})
			// #endregion
			return &embedded
		}
		// #region agent log
		emitDebugLog(debugRunIDValidatorVerdictV1, "H2", "internal/validator.go:ParseValidatorReport:embeddedJSON", "validator_parse_embedded_json_failed", map[string]any{
			"textLen": len(text),
		})
		// #endregion
	}

	// Parse explicit verdict token before coarse keyword fallback.
	verdictRe := regexp.MustCompile(`(?i)"verdict"\s*:\s*"(PASS|FAIL|BLOCKED)"|(?i)\bverdict\s*:\s*(PASS|FAIL|BLOCKED)\b`)
	if m := verdictRe.FindStringSubmatch(text); len(m) > 0 {
		verdict := ""
		for i := 1; i < len(m); i++ {
			if strings.TrimSpace(m[i]) != "" {
				verdict = strings.ToUpper(strings.TrimSpace(m[i]))
				break
			}
		}
		if verdict != "" {
			// #region agent log
			emitDebugLog(debugRunIDValidatorVerdictV1, "H1", "internal/validator.go:ParseValidatorReport:regexVerdict", "validator_parse_regex_verdict", map[string]any{
				"verdict": verdict,
				"textLen": len(text),
			})
			// #endregion
			return &ValidatorReport{Verdict: verdict}
		}
	}

	// Fallback: scan for verdict keywords in the text
	upper := strings.ToUpper(text)
	if strings.Contains(upper, "\"VERDICT\"") || strings.Contains(upper, "VERDICT:") {
		if strings.Contains(upper, "FAIL") {
			// #region agent log
			emitDebugLog(debugRunIDValidatorVerdictV1, "H1", "internal/validator.go:ParseValidatorReport:fallback", "validator_parse_fallback_keyword_fail", map[string]any{
				"containsPASS":    strings.Contains(upper, "PASS"),
				"containsFAIL":    true,
				"containsBLOCKED": strings.Contains(upper, "BLOCKED"),
			})
			// #endregion
			return &ValidatorReport{Verdict: "FAIL"}
		}
		if strings.Contains(upper, "PASS") {
			// #region agent log
			emitDebugLog(debugRunIDValidatorVerdictV1, "H1", "internal/validator.go:ParseValidatorReport:fallback", "validator_parse_fallback_keyword_pass", map[string]any{
				"containsPASS":    true,
				"containsFAIL":    strings.Contains(upper, "FAIL"),
				"containsBLOCKED": strings.Contains(upper, "BLOCKED"),
			})
			// #endregion
			return &ValidatorReport{Verdict: "PASS"}
		}
		if strings.Contains(upper, "BLOCKED") {
			// #region agent log
			emitDebugLog(debugRunIDValidatorVerdictV1, "H1", "internal/validator.go:ParseValidatorReport:fallback", "validator_parse_fallback_keyword_blocked", map[string]any{
				"containsPASS":    strings.Contains(upper, "PASS"),
				"containsFAIL":    strings.Contains(upper, "FAIL"),
				"containsBLOCKED": true,
			})
			// #endregion
			return &ValidatorReport{Verdict: "BLOCKED"}
		}
	}

	return nil
}
