package internal

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var transientStreamPatterns = []string{
	"socket connection was closed",
	"connection reset by peer",
	"overloaded_error",
	"rate_limit",
}

func StartClaude(prompt, cwd string, verbose *bool, ch chan ClaudeStreamMsg, extraArgs ...string) *exec.Cmd {
	args := []string{
		"-p", prompt,
		"--output-format", "stream-json",
		"--verbose",
	}
	hasAllowedTools := false
	for _, a := range extraArgs {
		if a == "--allowedTools" {
			hasAllowedTools = true
		}
	}
	if !hasAllowedTools {
		args = append(args, "--allowedTools", "Read,Bash,Glob,Grep,WebFetch,WebSearch")
	}
	for _, a := range extraArgs {
		if a != "--no-verbose" {
			args = append(args, a)
		}
	}
	cmd := exec.Command("claude", args...)
	cmd.Dir = cwd

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		go func() { ch <- ClaudeStreamMsg{Done: true, Err: err} }()
		return cmd
	}
	var stderrBuf strings.Builder
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		go func() { ch <- ClaudeStreamMsg{Done: true, Err: err} }()
		return cmd
	}

	go func() {
		defer close(ch)
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
		var resultText string
		var sessionID string
		var streamError string
		parser := &streamParser{verbose: verbose}

		for scanner.Scan() {
			line := scanner.Text()
			if strings.TrimSpace(line) == "" {
				continue
			}

			var ev map[string]any
			if err := json.Unmarshal([]byte(line), &ev); err != nil {
				continue
			}

			if sid, ok := ev["session_id"].(string); ok && sessionID == "" {
				sessionID = sid
			}

			if evType, _ := ev["type"].(string); evType == "result" {
				if r, ok := ev["result"].(string); ok {
					resultText = r
				}
			}

			lines := parser.parse(ev)
			for _, l := range lines {
				if l != "" {
					ch <- ClaudeStreamMsg{Line: l}
					if streamError == "" {
						lower := strings.ToLower(l)
						for _, p := range transientStreamPatterns {
							if strings.Contains(lower, p) {
								streamError = l
								break
							}
						}
					}
				}
			}
		}

		waitErr := cmd.Wait()
		if waitErr != nil {
			if resultText != "" && streamError == "" {
				ch <- ClaudeStreamMsg{Done: true, Result: resultText, SessionID: sessionID}
				return
			}
			errMsg := fmt.Sprintf("claude process exited: %v", waitErr)
			if streamError != "" {
				errMsg = streamError
			} else if stderr := strings.TrimSpace(stderrBuf.String()); stderr != "" {
				lines := strings.Split(stderr, "\n")
				last := lines[len(lines)-1]
				if len(last) > 200 {
					last = last[:200] + "..."
				}
				errMsg = last
			}
			ch <- ClaudeStreamMsg{Done: true, Err: fmt.Errorf("%s", errMsg), Result: resultText, SessionID: sessionID}
		} else {
			ch <- ClaudeStreamMsg{Done: true, Result: resultText, SessionID: sessionID}
		}
	}()

	return cmd
}

func StopClaude(cmd *exec.Cmd) {
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

type streamParser struct {
	toolName      string
	blockType     string // "text", "tool_use", or "thinking"
	inputBuf      strings.Builder
	textBuf       strings.Builder
	lastProgressN int
	thinkingSize  int
	verbose       *bool
}

func (p *streamParser) isVerbose() bool {
	return p.verbose != nil && *p.verbose
}

func (p *streamParser) parse(ev map[string]any) []string {
	evType, _ := ev["type"].(string)

	switch evType {
	case "system":
		if sub, _ := ev["subtype"].(string); sub == "init" {
			return []string{"◆ Session started"}
		}

	case "user":
		if !p.isVerbose() {
			return nil
		}
		if msg, ok := ev["message"].(map[string]any); ok {
			if content, ok := msg["content"].([]any); ok {
				var lines []string
				for _, c := range content {
					cm, ok := c.(map[string]any)
					if !ok {
						continue
					}
					if cmType, _ := cm["type"].(string); cmType == "tool_result" {
						toolID, _ := cm["tool_use_id"].(string)
						if toolID != "" {
							lines = append(lines, fmt.Sprintf("  ← [result for %s]", toolID[:min(len(toolID), 12)]))
						}
						lines = append(lines, p.extractToolResultLines(cm)...)
					}
				}
				return lines
			}
		}

	case "assistant":
		if msg, ok := ev["message"].(map[string]any); ok {
			if content, ok := msg["content"].([]any); ok {
				var lines []string
				for _, c := range content {
					cm, ok := c.(map[string]any)
					if !ok {
						continue
					}
					cmType, _ := cm["type"].(string)
					switch cmType {
					case "thinking":
						if p.isVerbose() {
							if t, ok := cm["thinking"].(string); ok {
								text := strings.TrimSpace(t)
								if text != "" {
									lines = append(lines, "💭 "+strings.Split(text, "\n")[0])
									for _, tl := range strings.Split(text, "\n")[1:] {
										lines = append(lines, "  "+tl)
									}
								}
							}
						}
					case "text":
						if t, ok := cm["text"].(string); ok {
							text := strings.TrimSpace(t)
							if text != "" {
								if !p.isVerbose() && len(text) > 200 {
									text = text[:200] + "..."
								}
								if p.isVerbose() {
									for _, tl := range strings.Split(text, "\n") {
										lines = append(lines, tl)
									}
								} else {
									lines = append(lines, text)
								}
							}
						}
					case "tool_use":
						name, _ := cm["name"].(string)
						if name != "" {
							if input, ok := cm["input"]; ok {
								inputJSON, _ := json.Marshal(input)
								if p.isVerbose() {
									lines = append(lines, fmt.Sprintf("▸ %s %s", name, string(inputJSON)))
								} else {
									detail := extractToolDetail(string(inputJSON))
									lines = append(lines, fmt.Sprintf("▸ %s%s", name, detail))
								}
							} else {
								lines = append(lines, fmt.Sprintf("▸ %s", name))
							}
						}
					case "tool_result":
						if p.isVerbose() {
							lines = append(lines, p.extractToolResultLines(cm)...)
						} else {
							text := extractToolResultText(cm)
							if text != "" {
								if len(text) > 200 {
									text = text[:200] + "…"
								}
								lines = append(lines, fmt.Sprintf("  ← %s", text))
							}
						}
					}
				}
				return lines
			}
		}

	case "content_block_start":
		if cb, ok := ev["content_block"].(map[string]any); ok {
			cbType, _ := cb["type"].(string)
			p.blockType = cbType
			if cbType == "tool_use" {
				if name, ok := cb["name"].(string); ok {
					p.toolName = name
					p.inputBuf.Reset()
				}
			} else if cbType == "text" {
				p.textBuf.Reset()
				p.lastProgressN = 0
			} else if cbType == "thinking" {
				p.thinkingSize = 0
				return []string{"💭 Thinking..."}
			}
		}

	case "content_block_delta":
		if delta, ok := ev["delta"].(map[string]any); ok {
			deltaType, _ := delta["type"].(string)
			switch deltaType {
			case "input_json_delta":
				if partial, ok := delta["partial_json"].(string); ok {
					p.inputBuf.WriteString(partial)
				}
			case "thinking_delta":
				if text, ok := delta["thinking"].(string); ok {
					p.thinkingSize += len(text)
					progressN := p.thinkingSize / 2000
					if progressN > p.lastProgressN {
						p.lastProgressN = progressN
						return []string{fmt.Sprintf("💭 Thinking... (%dK chars)", p.thinkingSize/1000)}
					}
				}
			case "text_delta":
				if text, ok := delta["text"].(string); ok {
					p.textBuf.WriteString(text)
					if p.isVerbose() {
						chunk := strings.TrimRight(text, "\n")
						if chunk != "" {
							return []string{chunk}
						}
					} else {
						size := p.textBuf.Len()
						progressN := size / 2000
						if progressN > p.lastProgressN {
							p.lastProgressN = progressN
							return []string{fmt.Sprintf("⟳ Writing response... (%dK chars)", size/1000)}
						}
					}
				}
			}
		}

	case "content_block_stop":
		var lines []string
		if p.blockType == "tool_use" && p.toolName != "" {
			if p.isVerbose() {
				lines = append(lines, fmt.Sprintf("▸ %s %s", p.toolName, p.inputBuf.String()))
			} else {
				detail := extractToolDetail(p.inputBuf.String())
				lines = append(lines, fmt.Sprintf("▸ %s%s", p.toolName, detail))
			}
			p.toolName = ""
			p.inputBuf.Reset()
		} else if p.blockType == "text" {
			if !p.isVerbose() {
				text := strings.TrimSpace(p.textBuf.String())
				if text != "" {
					if len(text) > 200 {
						text = text[:200] + "..."
					}
					lines = append(lines, text)
				}
			}
		}
		p.textBuf.Reset()
		p.blockType = ""
		return lines

	case "result":
		turns := ""
		if n, ok := ev["num_turns"].(float64); ok {
			turns = fmt.Sprintf(" (%d turns)", int(n))
		}
		cost := ""
		if c, ok := ev["cost_usd"].(float64); ok {
			cost = fmt.Sprintf(" · $%.2f", c)
		}
		return []string{fmt.Sprintf("✓ Done%s%s", turns, cost)}

	default:
		if p.isVerbose() && evType != "" {
			return p.parseVerboseEvent(evType, ev)
		}
	}

	return nil
}

func (p *streamParser) parseVerboseEvent(evType string, ev map[string]any) []string {
	var lines []string

	switch evType {
	case "tool_use":
		name, _ := ev["name"].(string)
		if name == "" {
			break
		}
		if input, ok := ev["input"]; ok {
			inputJSON, _ := json.Marshal(input)
			lines = append(lines, fmt.Sprintf("▸ %s %s", name, string(inputJSON)))
		} else {
			lines = append(lines, fmt.Sprintf("▸ %s", name))
		}

	case "tool_result":
		lines = append(lines, p.extractToolResultLines(ev)...)

	default:
		if sub, _ := ev["subtype"].(string); sub != "" {
			lines = append(lines, fmt.Sprintf("[%s:%s]", evType, sub))
		}
	}

	return lines
}

func (p *streamParser) extractToolResultLines(ev map[string]any) []string {
	var lines []string

	content := ev["content"]
	switch c := content.(type) {
	case string:
		text := strings.TrimSpace(c)
		if text != "" {
			for _, rl := range strings.Split(text, "\n") {
				lines = append(lines, fmt.Sprintf("  ← %s", rl))
			}
		}
	case []any:
		for _, item := range c {
			block, ok := item.(map[string]any)
			if !ok {
				continue
			}
			blockType, _ := block["type"].(string)
			switch blockType {
			case "text":
				if t, ok := block["text"].(string); ok {
					text := strings.TrimSpace(t)
					for _, rl := range strings.Split(text, "\n") {
						lines = append(lines, fmt.Sprintf("  ← %s", rl))
					}
				}
			case "tool_result":
				sub := p.extractToolResultLines(block)
				lines = append(lines, sub...)
			}
		}
	}

	return lines
}

func BuildDiscoveryPrompt(description, projectDir string) string {
	specSkill := ReadSkill("quest-spec")
	projectContext := GatherProjectContext(projectDir)

	return strings.Join([]string{
		"You are helping plan a spec-driven development mission. The user described what they want:",
		"",
		fmt.Sprintf("%q", description),
		"",
		"## Project Context",
		"",
		projectContext,
		"",
		"## Skill Reference (quest-spec methodology)",
		"",
		specSkill,
		"",
		"## Your task",
		"",
		"1. Use the project context above as your baseline understanding of the codebase",
		"2. Read additional files as needed to deepen your understanding",
		"3. Based on the quest-spec methodology above, propose structured requirements for this feature",
		"4. Ask clarifying questions about anything that is unclear or could go multiple ways",
		"5. Identify potential risks, challenges, or things that might conflict with existing code",
		"",
		"Format your response as:",
		"- Brief summary of what you found in the codebase",
		"- Proposed functional requirements (numbered, each independently testable)",
		"- Proposed non-functional requirements (performance, error handling, security)",
		"- Questions or concerns",
		"- Suggested technical approach",
		"",
		"Be conversational and thorough. The user will review and refine these requirements before we generate a formal spec with validation-contract assertions.",
	}, "\n")
}

func BuildFollowUpPrompt(messages []ChatMessage, feedback, projectDir string) string {
	var history strings.Builder
	all := append(messages, ChatMessage{Role: "user", Text: feedback})
	for _, m := range all {
		role := "User"
		if m.Role == "assistant" {
			role = "Assistant"
		} else if m.Role == "system" {
			role = "System"
		}
		history.WriteString(fmt.Sprintf("%s: %s\n\n", role, m.Text))
	}

	projectContext := GatherProjectContext(projectDir)

	return strings.Join([]string{
		"Continue the requirements conversation for this spec-driven mission. Here is the full conversation so far:",
		"",
		history.String(),
		"## Project Context",
		"",
		projectContext,
		"",
		"Respond to the user's latest message. If they refined requirements, update your proposal. If they asked a question, answer it.",
		"Keep proposing structured requirements and asking clarifying questions as needed.",
		"Remember: each functional requirement must be independently testable as a black-box assertion.",
	}, "\n")
}

func BuildPlanPrompt(messages []ChatMessage, projectDir string) string {
	skill := ReadSkill("spec-to-quest")
	projectContext := GatherProjectContext(projectDir)

	var history strings.Builder
	for _, m := range messages {
		role := "User"
		if m.Role == "assistant" {
			role = "Assistant"
		} else if m.Role == "system" {
			role = "System"
		}
		history.WriteString(fmt.Sprintf("%s: %s\n\n", role, m.Text))
	}

	return strings.Join([]string{
		"Create a complete mission plan based on the approved requirements conversation below.",
		"",
		"## Project Context",
		"",
		projectContext,
		"",
		"## Skill Reference (spec-to-quest methodology)",
		"",
		"Follow the assertion derivation rules and feature decomposition principles from this skill:",
		"",
		skill,
		"",
		"## Requirements Conversation",
		"",
		history.String(),
		"## Execution",
		"",
		"1. Use the project context above — read additional files only if needed for deeper understanding",
		"2. Derive assertions from EVERY requirement discussed above (follow the skill's quality bar)",
		"3. Decompose into features with detailed scope (follow the skill's decomposition principles)",
		"4. Output ONLY the JSON object — no markdown, no explanation, no code fences",
	}, "\n")
}

func BuildAnalysisPrompt(specDir, projectDir string, hasExistingAnalysis bool) string {
	slug := filepath.Base(specDir)
	missionDir := ResolveArtifactDir(specDir)

	filesToRead := []string{filepath.Join(specDir, "spec.md")}

	if fileExists(filepath.Join(specDir, "design-prompt.md")) {
		filesToRead = append(filesToRead, filepath.Join(specDir, "design-prompt.md"))
	}
	if fileExists(filepath.Join(specDir, "implementation-plan.md")) {
		filesToRead = append(filesToRead, filepath.Join(specDir, "implementation-plan.md"))
	}
	if hasExistingAnalysis {
		filesToRead = append(filesToRead, filepath.Join(missionDir, "codebase-analysis.md"))
	}

	var fileList strings.Builder
	for i, f := range filesToRead {
		fileList.WriteString(fmt.Sprintf("  %d. %s\n", i+1, f))
	}

	if hasExistingAnalysis {
		return strings.Join([]string{
			"IMPORTANT: Do NOT narrate or explain what you are doing. Just act.",
			"",
			"You are a fast codebase scout doing a DELTA UPDATE of an existing analysis.",
			"",
			"Read these files:",
			fileList.String(),
			"The last file is the existing analysis. The project's CLAUDE.md is already in your context.",
			"",
			"Compare the existing analysis against the current spec and codebase:",
			"1. Verify the reference pattern still exists and is the best match",
			"2. Check if any files mentioned have changed or been removed",
			"3. Identify anything NEW in the spec not covered",
			"",
			"If accurate, output it as-is. If updates needed, output the FULL updated analysis.",
			"Be fast — only read files if you suspect something changed.",
		}, "\n")
	}

	return strings.Join([]string{
		"IMPORTANT: Do NOT narrate or explain what you are doing. Just act.",
		"",
		"You are a fast codebase scout for docs/specs/" + slug + "/.",
		"",
		"Read these files:",
		fileList.String(),
		"The project's CLAUDE.md is already in your context.",
		"",
		"Find ONLY what is SPECIFIC to this spec:",
		"1. ONE existing feature most similar to what this spec requires — read its route, module barrel, and main component/service. That's the pattern reference.",
		"2. Domain model: types, schemas, entities, API endpoints relevant to this spec.",
		"3. If the spec mentions modifying existing files, read those files.",
		"",
		"Do NOT read: generic UI components, test setup files, config files, providers, utilities, CSS, stories.",
		"",
		"Output a concise report with:",
		"### Reference Pattern",
		"File paths and key code snippets showing the structure.",
		"### Domain Model",
		"Relevant types, schemas, entities, API endpoints.",
		"### Files to Create/Modify",
		"List with one-line explanation each. Be specific about paths.",
		"",
		"Keep it short. The planner is Opus — it can reason from a good reference.",
	}, "\n")
}

func cleanAnalysisOutput(text string) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "## ") || trimmed == "---" {
			return strings.Join(lines[i:], "\n")
		}
	}
	return text
}

func BuildSpecToPlanPrompt(specDir, projectDir, analysisPath string) string {
	slug := filepath.Base(specDir)

	if analysisPath != "" {
		missionDir := ResolveArtifactDir(specDir)
		skillPath := filepath.Join(missionDir, "spec-to-quest-skill.md")
		os.WriteFile(skillPath, []byte(ReadSkill("spec-to-quest")), 0o644)

		filesToRead := []string{analysisPath, skillPath, filepath.Join(specDir, "spec.md")}

		if fileExists(filepath.Join(specDir, "implementation-plan.md")) {
			filesToRead = append(filesToRead, filepath.Join(specDir, "implementation-plan.md"))
		}
		if fileExists(filepath.Join(specDir, "design-prompt.md")) {
			filesToRead = append(filesToRead, filepath.Join(specDir, "design-prompt.md"))
		}

		var fileList strings.Builder
		for i, f := range filesToRead {
			fileList.WriteString(fmt.Sprintf("  %d. %s\n", i+1, f))
		}

		return strings.Join([]string{
			"Generate a mission plan for docs/specs/" + slug + "/.",
			"",
			"Read these files first:",
			fileList.String(),
			"File #1 is the codebase analysis. File #2 is the skill methodology — follow it precisely.",
			"The rest are the spec and supporting docs.",
			"",
			"After reading all files, output ONLY the JSON object — no markdown, no explanation, no code fences.",
		}, "\n")
	}

	skill := ReadSkill("spec-to-quest")
	projectContext := loadProjectContext(ResolveArtifactDir(specDir), projectDir)

	var context strings.Builder
	context.WriteString("## Spec\n\n")
	context.WriteString(readFileContent(filepath.Join(specDir, "spec.md")))

	if dp := readFileContent(filepath.Join(specDir, "design-prompt.md")); dp != "" {
		context.WriteString("\n\n## Design Prompt\n\n")
		context.WriteString(dp)
	}

	if ip := readFileContent(filepath.Join(specDir, "implementation-plan.md")); ip != "" {
		context.WriteString("\n\n## Implementation Plan\n\n")
		context.WriteString(ip)
	}

	parts := []string{
		"You are executing the spec-to-quest skill. Follow it precisely.",
		"",
		"## Project Context",
		"",
		projectContext,
		"",
		"## Skill Instructions",
		"",
		skill,
		"",
		"## Input: docs/specs/" + slug + "/",
		"",
		context.String(),
		"",
		"## Execution",
		"",
		"Follow the skill workflow exactly:",
		"1. Use the project context above — read additional files only if needed",
		"2. Read the spec above in full — every section, every table, every requirement",
		"3. Derive assertions from EVERY functional and non-functional requirement",
		"4. Decompose into features with detailed scope",
		"5. Output ONLY the JSON object — no markdown, no explanation, no code fences",
	}

	return strings.Join(parts, "\n")
}

// Deprecated: kept for standalone testing. The pipeline uses BuildSkillPrompt instead.
func BuildAssertionPrompt(specDir string) string {
	slug := filepath.Base(specDir)

	filesToRead := []string{filepath.Join(specDir, "spec.md")}
	if fileExists(filepath.Join(specDir, "design-prompt.md")) {
		filesToRead = append(filesToRead, filepath.Join(specDir, "design-prompt.md"))
	}
	if fileExists(filepath.Join(specDir, "implementation-plan.md")) {
		filesToRead = append(filesToRead, filepath.Join(specDir, "implementation-plan.md"))
	}

	var fileList strings.Builder
	for i, f := range filesToRead {
		fileList.WriteString(fmt.Sprintf("  %d. %s\n", i+1, f))
	}

	return strings.Join([]string{
		"You are deriving behavioral assertions from a spec for docs/specs/" + slug + "/.",
		"",
		"Read these files first:",
		fileList.String(),
		"## Assertion Rules",
		"",
		"For EVERY functional and non-functional requirement, derive one or more black-box assertions.",
		"",
		"Format: `category.N: precondition/input → action → observable result`",
		"",
		"Categories: `api`, `ui`, `data`, `auth`, `error`, `perf`, `a11y`, `telemetry`, `test` (add domain-specific if needed).",
		"IDs are scoped per category starting from 1.",
		"",
		"Each assertion describes an observable behavior from the outside — input + action + expected output/state.",
		"NEVER reference implementation details (class names, function names, variable names, file paths).",
		"",
		"Quality bar:",
		"- A vague requirement like 'user can manage events' is NOT testable — decompose into concrete behaviors.",
		"- Every assertion must be independently verifiable by a validator that has never seen the implementation.",
		"- If a requirement has 3+ distinct behaviors, produce 3+ assertions, not one vague one.",
		"- Cover happy path, error cases, and edge cases.",
		"",
		"Example conversions:",
		"  FR: 'User enters name (required, max 255 chars)'",
		"  → ui.1: Empty name field → submit → validation error shown",
		"  → ui.2: Name exceeding 255 chars → field enforces max length",
		"  → data.1: Event created with name matching user input exactly",
		"",
		"  NFR: 'Step transitions < 200ms'",
		"  → perf.1: Click Next on any step → next step renders within 200ms",
		"",
		"  NFR: 'All form fields have visible labels'",
		"  → a11y.1: Every input has an associated visible <label> element",
		"",
		"## Output",
		"",
		"After reading all files, output ONLY a valid JSON array of assertion groups — no markdown, no explanation, no code fences.",
		"",
		`Example format: [{"category":"data","items":["data.1: Schema parses valid record","data.2: Schema rejects missing fields"]},{"category":"ui","items":["ui.1: ..."]}]`,
		"",
		"Every functional requirement must map to at least one assertion. Be thorough.",
	}, "\n")
}

// Deprecated: split into BuildAssertionsPrompt + BuildFeaturesPrompt for output cap safety.
// Kept for standalone testing and migration fallback.
func BuildSkillPrompt(specDir, projectDir string) string {
	slug := filepath.Base(specDir)
	missionDir := ResolveArtifactDir(specDir)

		tmpFile, err := os.CreateTemp("", "spec-to-quest-*.md")
	skillPath := ""
	if err == nil {
		tmpFile.WriteString(ReadSkill("spec-to-quest"))
		tmpFile.Close()
		skillPath = tmpFile.Name()
	}

	filesToRead := []string{
		skillPath,
		filepath.Join(specDir, "spec.md"),
		filepath.Join(missionDir, "codebase-analysis.md"),
	}

	if fileExists(filepath.Join(specDir, "design-prompt.md")) {
		filesToRead = append(filesToRead, filepath.Join(specDir, "design-prompt.md"))
	}
	if fileExists(filepath.Join(specDir, "implementation-plan.md")) {
		filesToRead = append(filesToRead, filepath.Join(specDir, "implementation-plan.md"))
	}

	var fileList strings.Builder
	for i, f := range filesToRead {
		fileList.WriteString(fmt.Sprintf("  %d. %s\n", i+1, f))
	}

	return strings.Join([]string{
		"IMPORTANT: Do NOT narrate, explain, or describe what you are doing. Just act.",
		"",
		"Execute the spec-to-quest skill for docs/specs/" + slug + "/.",
		"",
		"Read these files:",
		fileList.String(),
		"File #1 is the skill — follow it exactly.",
		"File #2 is the spec. File #3 is the codebase analysis.",
		"",
		"The project's CLAUDE.md is already in your context — do not read it again.",
		"After reading all files, output ONLY the JSON object. No markdown, no code fences, no explanation.",
	}, "\n")
}

// BuildAssertionsPrompt is Call 1 of the v2 pipeline: derive assertions only.
// All inputs are inlined; the model must not use tools.
func BuildAssertionsPrompt(specDir, projectDir string, retryFeedback string) string {
	slug := filepath.Base(specDir)

	skill := ReadSkill("spec-to-assertions")
	spec := readFileContent(filepath.Join(specDir, "spec.md"))
	designPrompt := readFileContent(filepath.Join(specDir, "design-prompt.md"))
	implPlan := readFileContent(filepath.Join(specDir, "implementation-plan.md"))
	constraints := extractSpecStructure(spec)

	var parts []string
	parts = append(parts,
		"IMPORTANT: Do NOT narrate, explain, or describe what you are doing.",
		"All inputs are inlined below — do NOT use any tools (Read, Bash, Grep, etc.).",
		"Output ONLY the JSON array of assertion groups. No markdown, no code fences, no prose.",
		"",
	)

	if retryFeedback != "" {
		parts = append(parts,
			"## Previous attempt had coverage gaps. Fix these before re-emitting:",
			"",
			retryFeedback,
			"",
			"Re-emit the COMPLETE corrected JSON array (not a diff). Output ONLY JSON.",
			"",
		)
	}

	parts = append(parts,
		"## Skill: spec-to-assertions",
		"",
		skill,
		"",
		"---",
		"",
		"## Target: docs/specs/"+slug+"/",
		"",
		"## Spec",
		"",
		spec,
		"",
	)

	if designPrompt != "" {
		parts = append(parts, "## Design Prompt", "", designPrompt, "")
	}
	if implPlan != "" {
		parts = append(parts, "## Implementation Plan", "", implPlan, "")
	}

	if constraints != "" {
		parts = append(parts,
			"## Coverage Requirements (output MUST satisfy all)",
			"",
			constraints,
			"",
		)
	}

	parts = append(parts,
		"## Output",
		"",
		`Output ONLY a JSON array of {"category","items"} objects. No prose, no fences.`,
	)

	return strings.Join(parts, "\n")
}

// BuildFeaturesPrompt is Call 2 of the v2 pipeline: features + knowledge.
// Receives the assertion IDs produced by Call 1 and decomposes the spec into
// features that reference them. All inputs are inlined.
func BuildFeaturesPrompt(specDir, projectDir string, assertionIDs map[string][]string, retryFeedback string) string {
	slug := filepath.Base(specDir)
	missionDir := ResolveArtifactDir(specDir)

	skill := ReadSkill("spec-to-features")
	spec := readFileContent(filepath.Join(specDir, "spec.md"))
	analysis := readFileContent(filepath.Join(missionDir, "codebase-analysis.md"))
	designPrompt := readFileContent(filepath.Join(specDir, "design-prompt.md"))
	implPlan := readFileContent(filepath.Join(specDir, "implementation-plan.md"))

	var parts []string
	parts = append(parts,
		"IMPORTANT: Do NOT narrate, explain, or describe what you are doing.",
		"All inputs are inlined below — do NOT use any tools (Read, Bash, Grep, etc.).",
		"Output ONLY the JSON object. No markdown, no code fences, no prose.",
		"",
	)

	if retryFeedback != "" {
		parts = append(parts,
			"## Previous attempt had coverage gaps. Fix these before re-emitting:",
			"",
			retryFeedback,
			"",
			"Re-emit the COMPLETE corrected JSON object (not a diff). Output ONLY JSON.",
			"",
		)
	}

	parts = append(parts,
		"## Skill: spec-to-features",
		"",
		skill,
		"",
		"---",
		"",
		"## Target: docs/specs/"+slug+"/",
		"",
		"## Spec",
		"",
		spec,
		"",
		"## Codebase Analysis",
		"",
		analysis,
		"",
	)

	if designPrompt != "" {
		parts = append(parts, "## Design Prompt", "", designPrompt, "")
	}
	if implPlan != "" {
		parts = append(parts, "## Implementation Plan", "", implPlan, "")
	}

	parts = append(parts,
		"## Assertion IDs to cover (already authored — bucket them into features)",
		"",
		formatAssertionIDList(assertionIDs),
		"",
		"## Decomposition Requirements (output MUST satisfy all)",
		"",
		"- Every assertion ID above MUST be referenced by >=1 feature.validation_refs",
		"- Every feature MUST have non-empty scope (>=80 chars) describing schemas, validation, file paths",
		"- Every feature MUST have >=1 validation_refs entry",
		"- Order features by phase: 0 (foundation: schemas/types/fixtures) -> 1 (core: hooks/page/forms) -> 2 (integration) -> 3 (polish: a11y/perf/tests)",
		"- depends_on must list the IDs of features producing code this one consumes",
		"- DO NOT include a 'knowledge' field — knowledge is generated by a separate downstream phase that will see your features.json",
		"",
		"## Output",
		"",
		`Output ONLY a JSON object {"features":[...]}. No 'knowledge' field. No prose, no fences.`,
	)

	return strings.Join(parts, "\n")
}

// BuildKnowledgePromptV2 is Phase Knowledge of the v3 pipeline: synthesizes
// actionable knowledge entries from spec + analysis + already-decomposed
// features. Runs after Features so knowledge can anchor to specific feature
// IDs and scopes. Designed for sonnet (cheaper, faster, sufficient quality).
func BuildKnowledgePromptV2(specDir, projectDir string, features []Feature, retryFeedback string) string {
	slug := filepath.Base(specDir)
	missionDir := ResolveArtifactDir(specDir)

	skill := ReadSkill("spec-to-knowledge")
	spec := readFileContent(filepath.Join(specDir, "spec.md"))
	analysis := readFileContent(filepath.Join(missionDir, "codebase-analysis.md"))
	contract := readFileContent(filepath.Join(missionDir, "validation-contract.md"))
	designPrompt := readFileContent(filepath.Join(specDir, "design-prompt.md"))
	implPlan := readFileContent(filepath.Join(specDir, "implementation-plan.md"))

	var parts []string
	parts = append(parts,
		"IMPORTANT: Do NOT narrate, explain, or describe what you are doing.",
		"All inputs are inlined below — do NOT use any tools (Read, Bash, Grep, etc.).",
		"Output ONLY the JSON array of strings. No markdown, no code fences, no prose.",
		"",
	)

	if retryFeedback != "" {
		parts = append(parts,
			"## Previous attempt had quality gaps. Fix these before re-emitting:",
			"",
			retryFeedback,
			"",
			"Re-emit the COMPLETE corrected JSON array (not a diff). Output ONLY JSON.",
			"",
		)
	}

	parts = append(parts,
		"## Skill: spec-to-knowledge",
		"",
		skill,
		"",
		"---",
		"",
		"## Target: docs/specs/"+slug+"/",
		"",
		"## Spec",
		"",
		spec,
		"",
		"## Codebase Analysis",
		"",
		analysis,
		"",
		"## Features (already decomposed — reference these in your knowledge)",
		"",
		formatFeaturesForKnowledge(features),
		"",
	)

	if contract != "" {
		parts = append(parts,
			"## Validation Contract (already authored)",
			"",
			contract,
			"",
		)
	}

	if designPrompt != "" {
		parts = append(parts, "## Design Prompt", "", designPrompt, "")
	}
	if implPlan != "" {
		parts = append(parts, "## Implementation Plan", "", implPlan, "")
	}

	parts = append(parts,
		"## Synthesis Requirements (output MUST satisfy all)",
		"",
		"- Output a JSON array of strings — top-level array, NO {} wrapper",
		"- Target 8–18 entries; >25 is dilution",
		"- Each entry actionable: a worker takes a different action after reading it",
		"- Each entry 60–200 chars, complete sentence or punctuation-terminated phrase",
		"- No paraphrasing of the spec; no generic 'use TypeScript' / 'write tests'",
		"- Anchor entries to specific features when possible (mention pattern, schema, file path)",
		"",
		"## Output",
		"",
		`Output ONLY a JSON array of strings. No object wrapper, no prose, no fences.`,
	)

	return strings.Join(parts, "\n")
}

// formatFeaturesForKnowledge renders the feature list compactly so the
// knowledge model can reference IDs and scope without re-deriving.
func formatFeaturesForKnowledge(features []Feature) string {
	if len(features) == 0 {
		return "(no features provided)"
	}
	var b strings.Builder
	for _, f := range features {
		fid := strings.TrimSpace(f.ID)
		if fid == "" {
			fid = f.Title
		}
		b.WriteString("- ")
		b.WriteString(fid)
		if title := strings.TrimSpace(f.Title); title != "" {
			b.WriteString(" — ")
			b.WriteString(title)
		}
		if scope := strings.TrimSpace(f.Scope); scope != "" {
			b.WriteString("\n    scope: ")
			b.WriteString(truncatePreview(scope, 240))
		}
		if len(f.ValidationRefs) > 0 {
			b.WriteString("\n    refs: ")
			b.WriteString(strings.Join(f.ValidationRefs, ", "))
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// formatAssertionIDList renders the per-category map as a readable block:
//
//	ui: ui.1, ui.2, ui.3
//	data: data.1, data.2
func formatAssertionIDList(ids map[string][]string) string {
	if len(ids) == 0 {
		return "(no assertion IDs provided)"
	}
	categories := make([]string, 0, len(ids))
	for k := range ids {
		categories = append(categories, k)
	}
	sort.Strings(categories)

	var b strings.Builder
	for _, cat := range categories {
		items := ids[cat]
		if len(items) == 0 {
			continue
		}
		b.WriteString(cat)
		b.WriteString(": ")
		b.WriteString(strings.Join(items, ", "))
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// extractSpecStructure produces a coverage-requirements block derived from the
// spec.md content. Used to constrain the assertions call so the model cannot
// silently drop FRs, NFRs, or endpoints.
func extractSpecStructure(spec string) string {
	if spec == "" {
		return ""
	}

	frs := extractNumberedSection(spec, "Functional Requirements")
	nfrs := extractNumberedSection(spec, "Non-Functional Requirements")
	if len(nfrs) == 0 {
		nfrs = extractBulletSection(spec, "Non-Functional Requirements")
	}
	endpoints := extractAPIEndpoints(spec)

	var b strings.Builder

	if len(frs) > 0 {
		b.WriteString(fmt.Sprintf("- Every FR (FR1..FR%d) MUST map to >=1 assertion in some category. FRs:\n", len(frs)))
		for i, fr := range frs {
			b.WriteString(fmt.Sprintf("  FR%d: %s\n", i+1, truncatePreview(fr, 120)))
		}
	}

	if len(nfrs) > 0 {
		b.WriteString("- Every NFR below MUST map to >=1 assertion (a11y/perf/error/security/telemetry/auth as applicable):\n")
		for i, nfr := range nfrs {
			b.WriteString(fmt.Sprintf("  NFR%d: %s\n", i+1, truncatePreview(nfr, 120)))
		}
	}

	if len(endpoints) > 0 {
		b.WriteString("- Every API endpoint below MUST have >=1 happy-path AND >=1 error-path api.* assertion:\n")
		for _, ep := range endpoints {
			b.WriteString("  ")
			b.WriteString(ep)
			b.WriteString("\n")
		}
	}

	if b.Len() == 0 {
		return ""
	}

	b.WriteString("- Output ONLY a JSON array of {category, items} objects. No prose, no fences.\n")
	return strings.TrimRight(b.String(), "\n")
}

// extractNumberedSection pulls items from a "1.", "2.", ... numbered list under
// a "## <header>" markdown section. Returns trimmed item bodies (header digit
// stripped). Multi-line items are joined with single spaces.
func extractNumberedSection(spec, header string) []string {
	if spec == "" || header == "" {
		return nil
	}
	pattern := `(?ms)^##\s+` + regexp.QuoteMeta(header) + `\s*$\s*\n(.*?)(?:^##\s|\z)`
	re := regexp.MustCompile(pattern)
	m := re.FindStringSubmatch(spec)
	if len(m) < 2 {
		return nil
	}
	section := m[1]

	itemRe := regexp.MustCompile(`^\s*\d+\.\s+`)
	var items []string
	var current strings.Builder
	for _, line := range strings.Split(section, "\n") {
		if itemRe.MatchString(line) {
			if current.Len() > 0 {
				items = append(items, strings.TrimSpace(current.String()))
				current.Reset()
			}
			current.WriteString(itemRe.ReplaceAllString(line, ""))
		} else if current.Len() > 0 {
			trimmed := strings.TrimSpace(line)
			if trimmed != "" {
				current.WriteString(" ")
				current.WriteString(trimmed)
			}
		}
	}
	if current.Len() > 0 {
		items = append(items, strings.TrimSpace(current.String()))
	}
	return items
}

// extractBulletSection is a fallback for sections (typically NFRs) that use
// "- " bullets instead of numbered items. Captures one entry per top-level
// bullet, ignoring nested bullets and bold-prefixed labels.
func extractBulletSection(spec, header string) []string {
	if spec == "" || header == "" {
		return nil
	}
	pattern := `(?ms)^##\s+` + regexp.QuoteMeta(header) + `\s*$\s*\n(.*?)(?:^##\s|\z)`
	re := regexp.MustCompile(pattern)
	m := re.FindStringSubmatch(spec)
	if len(m) < 2 {
		return nil
	}
	section := m[1]

	bulletRe := regexp.MustCompile(`^-\s+`)
	var items []string
	var current strings.Builder
	for _, line := range strings.Split(section, "\n") {
		if bulletRe.MatchString(line) {
			if current.Len() > 0 {
				items = append(items, strings.TrimSpace(current.String()))
				current.Reset()
			}
			current.WriteString(bulletRe.ReplaceAllString(line, ""))
		} else if current.Len() > 0 {
			trimmed := strings.TrimSpace(line)
			// Skip nested bullets to keep top-level entries tight.
			if strings.HasPrefix(trimmed, "-") {
				continue
			}
			if trimmed != "" {
				current.WriteString(" ")
				current.WriteString(trimmed)
			}
		}
	}
	if current.Len() > 0 {
		items = append(items, strings.TrimSpace(current.String()))
	}
	return items
}

// extractAPIEndpoints scans the entire spec for HTTP method + path pairs.
// Deduplicates results while preserving first-seen order.
func extractAPIEndpoints(spec string) []string {
	if spec == "" {
		return nil
	}
	re := regexp.MustCompile("(?m)\\b(GET|POST|PUT|PATCH|DELETE)\\s+(/[^\\s`\"',()]+)")
	matches := re.FindAllStringSubmatch(spec, -1)
	seen := make(map[string]bool)
	var endpoints []string
	for _, m := range matches {
		if len(m) < 3 {
			continue
		}
		key := m[1] + " " + m[2]
		if seen[key] {
			continue
		}
		seen[key] = true
		endpoints = append(endpoints, key)
	}
	return endpoints
}

func truncatePreview(s string, max int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

func compactContract(contractPath string) string {
	assertions := parseAssertionsFromContract(contractPath)
	if len(assertions) == 0 {
		return ""
	}
	var b strings.Builder
	for _, a := range assertions {
		b.WriteString(fmt.Sprintf("### %s\n", a.Category))
		for _, item := range a.Items {
			parts := strings.SplitN(item, ":", 2)
			if len(parts) == 2 {
				id := strings.TrimSpace(parts[0])
				desc := strings.TrimSpace(parts[1])
				if idx := strings.Index(desc, ". "); idx > 0 && idx < 120 {
					desc = desc[:idx+1]
				} else if len(desc) > 120 {
					desc = desc[:120] + "..."
				}
				b.WriteString(fmt.Sprintf("- %s: %s\n", id, desc))
			} else {
				short := item
				if len(short) > 120 {
					short = short[:120] + "..."
				}
				b.WriteString(fmt.Sprintf("- %s\n", short))
			}
		}
		b.WriteString("\n")
	}
	return b.String()
}

func CompactKnowledge(knowledge string) string {
	if knowledge == "" {
		return ""
	}

	var entries []string
	inHowTo := false
	seenEntry := false

	for _, line := range strings.Split(knowledge, "\n") {
		trimmed := strings.TrimSpace(line)

		if strings.HasPrefix(trimmed, "# ") {
			continue
		}

		if strings.HasPrefix(trimmed, "## How to contribute") {
			inHowTo = true
			continue
		}

		if strings.HasPrefix(trimmed, "## ") {
			inHowTo = false
			continue
		}

		if trimmed == "---" {
			inHowTo = false
			continue
		}

		if inHowTo {
			continue
		}

		if trimmed == "" {
			continue
		}

		if !seenEntry && !strings.HasPrefix(trimmed, "- ") {
			continue
		}

		seenEntry = true
		entries = append(entries, trimmed)
	}

	if len(entries) == 0 {
		return knowledge
	}

	return strings.Join(entries, "\n")
}

// Deprecated: kept for standalone testing. The pipeline uses BuildSkillPrompt instead.
func BuildFeaturesOnlyPrompt(specDir string) string {
	slug := filepath.Base(specDir)
	missionDir := ResolveArtifactDir(specDir)

	skill := ReadSkill("feature-decomposer")
	contract := compactContract(filepath.Join(missionDir, "validation-contract.md"))
	spec := readFileContent(filepath.Join(specDir, "spec.md"))
	analysis := readFileContent(filepath.Join(missionDir, "codebase-analysis.md"))

	var parts []string
	parts = append(parts,
		skill,
		"",
		"## Target: docs/specs/"+slug+"/",
		"",
		"ALL inputs are provided below. Do NOT use Read or any tools. Output ONLY the JSON.",
		"",
		"## Spec",
		"",
		spec,
		"",
		"## Codebase Analysis",
		"",
		analysis,
		"",
	)

	if dp := readFileContent(filepath.Join(specDir, "design-prompt.md")); dp != "" {
		parts = append(parts, "## Design Prompt", "", dp, "")
	}
	if ip := readFileContent(filepath.Join(specDir, "implementation-plan.md")); ip != "" {
		parts = append(parts, "## Implementation Plan", "", ip, "")
	}

	parts = append(parts,
		"## Assertion IDs (for validation_refs)",
		"",
		contract,
		"Output ONLY the features JSON now.",
	)

	return strings.Join(parts, "\n")
}

// Deprecated: kept for standalone testing. The pipeline uses BuildSkillPrompt instead.
func BuildKnowledgePrompt(specDir string) string {
	slug := filepath.Base(specDir)
	missionDir := ResolveArtifactDir(specDir)

	spec := readFileContent(filepath.Join(specDir, "spec.md"))
	analysis := readFileContent(filepath.Join(missionDir, "codebase-analysis.md"))
	contract := readFileContent(filepath.Join(missionDir, "validation-contract.md"))

	return strings.Join([]string{
		"You are building the initial knowledge base for a mission on docs/specs/" + slug + "/.",
		"",
		"ALL artifacts are provided below. Do NOT use Read, Glob, Grep, Bash, or any tools.",
		"Start extracting knowledge IMMEDIATELY and output ONLY the JSON result.",
		"",
		"## Spec",
		"",
		spec,
		"",
		"## Codebase Analysis",
		"",
		analysis,
		"",
		"## Validation Contract",
		"",
		contract,
		"",
		"## Knowledge Base Purpose",
		"",
		"The knowledge base helps workers and validators understand critical context they won't find in the spec alone:",
		"",
		"- Architectural constraints from CLAUDE.md or the codebase that workers MUST follow",
		"- Gaps between what the spec assumes and what actually exists (e.g., missing APIs, types, providers)",
		"- Available libraries, utilities, and patterns that workers should use (not recreate)",
		"- Cross-module conventions (import paths, barrel exports, naming)",
		"- Open questions from the spec that affect implementation",
		"- Provider/hook APIs that differ from what the spec assumes",
		"- External references mentioned in the spec",
		"",
		"Each entry should be a single, actionable insight. No fluff. Workers read this before starting — every entry should save them time or prevent a mistake.",
		"",
		"## Output",
		"",
		"Output ONLY a valid JSON array of strings — no markdown, no explanation, no code fences.",
		"",
		`Example: ["activeTenant is currently a string, not an object — spec assumes .timezone exists","Use @sitickets/datetime for UTC conversion, never new Date()"]`,
	}, "\n")
}

func BuildRegenPlanPrompt(specDir, missionDir, projectDir string) string {
	skill := ReadSkill("spec-to-quest")
	projectContext := loadProjectContext(missionDir, projectDir)

	specContent := readFileContent(filepath.Join(specDir, "spec.md"))
	featuresJSON := readFileContent(filepath.Join(missionDir, "features.json"))
	contractContent := readFileContent(filepath.Join(missionDir, "validation-contract.md"))
	knowledgeContent := readFileContent(filepath.Join(missionDir, "knowledge-base.md"))

	var completedSummary strings.Builder
	state := ReadMissionState(missionDir)
	for _, f := range state.Features {
		completedSummary.WriteString(fmt.Sprintf("- %s: %q [status: %s] scope: %s\n", f.ID, f.Title, f.Status, f.Scope))
	}

	return strings.Join([]string{
		"You are regenerating a mission plan for a spec that has CHANGED since the original plan was created.",
		"Some features may already be implemented. You must diff the spec against the current plan and produce an updated plan.",
		"",
		"## Project Context",
		"",
		projectContext,
		"",
		"## Skill Instructions",
		"",
		skill,
		"",
		"## Current Spec",
		"",
		specContent,
		"",
		"## Current Mission State",
		"",
		"### features.json",
		featuresJSON,
		"",
		"### Feature Summary",
		completedSummary.String(),
		"",
		"### Validation Contract",
		contractContent,
		"",
		"### Knowledge Base",
		knowledgeContent,
		"",
		"## Regeneration Rules",
		"",
		"1. READ the spec carefully — it is the source of truth for what SHOULD exist",
		"2. COMPARE against the current features.json — understand what WAS planned",
		"3. Features with status \"done\" or \"validated\": KEEP as-is (same ID, title, scope, status). These are already implemented.",
		"4. Features with status \"pending\" or \"blocked\": RE-EVALUATE against the current spec. Keep if still needed, remove if no longer relevant, update scope if the spec changed.",
		"5. Features with status \"in_progress\", \"awaiting_validation\", \"validating\", or \"refining\": RESET to \"pending\" with updated scope if the spec changed.",
		"6. Identify NEW requirements in the spec that have no matching feature — create new features for them.",
		"7. Identify REMOVED requirements — drop features that are no longer in the spec (unless status is \"done\").",
		"8. Re-derive validation assertions from the CURRENT spec. Keep assertion IDs stable for done features.",
		"9. Maintain correct dependency ordering and phase assignments.",
		"10. Update knowledge base entries if the spec changes invalidate any prior findings.",
		"11. Preserve fix lineage: do not rename or reuse IDs from existing fix_features; avoid introducing IDs that collide with existing features/fixes.",
		"",
		"## Output",
		"",
		"Output ONLY a valid JSON object (no markdown, no explanation, no code fences) matching this schema:",
		`{"slug":"...","spec":"docs/specs/<slug>/spec.md","project":"...","owner":"...","features":[{"id":"F01","title":"...","status":"done|pending","phase":0,"depends_on":[],"scope":"...","validation_refs":["cat.1"]}],"assertions":[{"category":"cat","items":["cat.1: Assertion"]}],"knowledge":["..."]}`,
	}, "\n")
}

func BuildRefinePlanPrompt(feedback, specDir, projectDir string) string {
	missionDir := ResolveArtifactDir(specDir)
	projectContext := loadProjectContext(missionDir, projectDir)

	return strings.Join([]string{
		fmt.Sprintf("Read the current mission plan files in %s/ (features.json, validation-contract.md, knowledge-base.md).", missionDir),
		"",
		"## Project Context",
		"",
		projectContext,
		"",
		"The user wants to modify the plan. Their feedback:",
		fmt.Sprintf("%q", feedback),
		"",
		"Apply the feedback and output ONLY a valid JSON object (no markdown, no explanation, no code fences) matching this schema:",
		`{"slug":"...","spec":"docs/specs/<slug>/spec.md","project":"...","owner":"...","features":[{"id":"F01","title":"...","phase":0,"depends_on":[],"scope":"...","validation_refs":["cat.1"]}],"assertions":[{"category":"cat","items":["cat.1: Assertion"]}],"knowledge":["..."]}`,
		"",
		"Preserve existing structure unless the feedback explicitly asks to change it.",
		"Output ONLY the JSON object, nothing else.",
	}, "\n")
}

func BuildEditDiscoveryPrompt(description, specDir, projectDir string) string {
	specSkill := ReadSkill("quest-spec")
	missionDir := ResolveArtifactDir(specDir)
	projectContext := loadProjectContext(missionDir, projectDir)

	specContent := readFileContent(filepath.Join(specDir, "spec.md"))
	featuresContent := readFileContent(filepath.Join(missionDir, "features.json"))
	contractContent := readFileContent(filepath.Join(missionDir, "validation-contract.md"))

	return strings.Join([]string{
		"You are helping update an existing spec-driven development mission. The user wants to make changes:",
		"",
		fmt.Sprintf("%q", description),
		"",
		"## Project Context",
		"",
		projectContext,
		"",
		"## Existing Spec",
		"",
		specContent,
		"",
		"## Existing Features (features.json)",
		"",
		featuresContent,
		"",
		"## Existing Validation Contract",
		"",
		contractContent,
		"",
		"## Skill Reference (quest-spec methodology)",
		"",
		specSkill,
		"",
		"## Your task",
		"",
		"1. Use the project context and existing spec above",
		"2. Propose changes to the spec based on the user's request:",
		"   - New features to add",
		"   - Existing features to modify",
		"   - New assertions needed",
		"   - Scope changes",
		"3. Features with status 'done' or 'in_progress' should NOT be modified",
		"4. New features should have IDs that continue from the existing sequence",
		"",
		"Format your response as:",
		"- Summary of existing spec state",
		"- Proposed changes (additions, modifications)",
		"- New functional requirements",
		"- Questions or concerns",
		"",
		"Be conversational. The user will review before we update the spec.",
	}, "\n")
}

func BuildEditPlanPrompt(messages []ChatMessage, specDir, projectDir string) string {
	specSkill := ReadSkill("quest-spec")
	missionDir := ResolveArtifactDir(specDir)
	slug := filepath.Base(specDir)
	projectContext := loadProjectContext(missionDir, projectDir)

	featuresContent := readFileContent(filepath.Join(missionDir, "features.json"))
	contractContent := readFileContent(filepath.Join(missionDir, "validation-contract.md"))

	var history strings.Builder
	for _, m := range messages {
		role := "User"
		if m.Role == "assistant" {
			role = "Assistant"
		} else if m.Role == "system" {
			role = "System"
		}
		history.WriteString(fmt.Sprintf("%s: %s\n\n", role, m.Text))
	}

	return strings.Join([]string{
		"UPDATE the existing mission spec based on the approved changes below.",
		"",
		"## Project Context",
		"",
		projectContext,
		"",
		"## Existing Features",
		"",
		featuresContent,
		"",
		"## Existing Validation Contract",
		"",
		contractContent,
		"",
		"## Skill Reference (quest-spec methodology)",
		"",
		specSkill,
		"",
		"## Change Conversation",
		history.String(),
		"## Output",
		"",
		"Output ONLY a valid JSON object (no markdown, no explanation, no code fences) matching this exact schema:",
		`{"slug":"` + slug + `","spec":"docs/specs/` + slug + `/spec.md","project":"<name>","owner":"<owner>","features":[...],"assertions":[...],"knowledge":[...]}`,
		"",
		"Rules:",
		"- slug MUST be: " + slug,
		"- PRESERVE all features with status 'done', 'in_progress', 'awaiting_validation', or 'blocked' — do NOT remove or modify them",
		"- ADD new features with IDs continuing from the existing sequence",
		"- UPDATE assertions: keep existing ones, add new ones as needed",
		"- New features must have validation_refs pointing to assertions",
		"- Preserve fix lineage from existing features.json; do not reuse IDs that collide with existing features or fix_features",
		"- Output ONLY the JSON object, nothing else",
	}, "\n")
}

func BuildWorkerPrompt(feature Feature, siblings []string, contract, knowledge, specPath, projectDir, failureContext string) string {
	workerSkill := ReadSkill("quest-worker")
	missionDir := ResolveArtifactDir(specPath)
	projectContext := loadProjectContext(missionDir, projectDir)

	var parts []string
	parts = append(parts,
		"## Project Context",
		"",
		projectContext,
		"",
		"## Skill Reference (quest-worker methodology)",
		"",
		workerSkill,
		"",
		"---",
		"",
		fmt.Sprintf("## Feature: %s — %s", feature.ID, feature.Title),
		"",
		fmt.Sprintf("Spec folder: %s", specPath),
		fmt.Sprintf("Scope: %s", feature.Scope),
		fmt.Sprintf("Phase: %d", feature.Phase),
	)

	if len(feature.DependsOn) > 0 {
		parts = append(parts, fmt.Sprintf("Dependencies (already implemented): %s", strings.Join(feature.DependsOn, ", ")))
	}

	if len(siblings) > 0 {
		parts = append(parts, "",
			fmt.Sprintf("IMPORTANT: Other agents are working in parallel on: %s. Avoid editing files that belong to those features.", strings.Join(siblings, ", ")))
	}

	parts = append(parts, "", "Validation assertions that must pass:")
	for _, ref := range feature.ValidationRefs {
		parts = append(parts, "- "+ref)
	}

	if contract != "" {
		filtered := FilterContractAssertions(contract, feature.ValidationRefs)
		parts = append(parts, "", "## Validation Contract", "", filtered)
	}
	if knowledge != "" {
		parts = append(parts, "", "## Project Knowledge", "", CompactKnowledge(knowledge))
	}

	if failureContext != "" {
		parts = append(parts, "",
			"## ⚠ Previous Attempt Failed — Failure Analysis",
			"",
			"This feature was attempted before and FAILED. Study the analysis below carefully.",
			"Do NOT repeat the same mistakes. Adjust your approach based on these findings:",
			"",
			failureContext,
			"",
			"---",
		)
	}

	parts = append(parts, "",
		"## Instructions",
		"",
		"- Use the project context above — read additional files only if needed",
		"- Read the spec at "+specPath+"/spec.md to understand the feature context, data schemas, API endpoints, and UI requirements",
		"- Follow TDD: write tests for each assertion FIRST, then implement the ACTUAL source code",
		"- Run lint and unit tests for your stack; both must pass before you end the session",
		"- Run existing tests to verify nothing breaks",
		"- Do not ask questions — make reasonable decisions and proceed",
		"",
		"## IMPORTANT: Status management",
		"",
		"The orchestrator manages ALL status transitions in features.json.",
		"Do NOT update the status field yourself. Focus only on implementation.",
		"When you finish, the orchestrator will detect completion and update status.",
		"If you hit a blocker you cannot resolve, document it in knowledge-base.md",
		"and end your session — the orchestrator will mark the feature as blocked.",
		"",
		"## CRITICAL: Implementation completeness",
		"",
		"You MUST create all source files, not just test files. A feature is NOT done until:",
		"- Every source file (components, hooks, schemas, handlers, utilities) EXISTS on disk",
		"- Every test file imports from a source file that EXISTS and is fully implemented",
		"- Lint passes for the project/package you changed",
		"- All tests PASS (not just written — actually passing)",
		"- The feature is functionally complete as described in the scope",
		"",
		"DO NOT end your session if:",
		"- Any test imports a module that doesn't exist yet",
		"- Any component referenced in the scope hasn't been created",
		"- Tests are written but the implementation code is missing",
		"",
		"The orchestrator enforces a quality gate (lint + unit tests) before validation.",
		"If lint/tests fail, your worker will be retried automatically with failure context.",
		"Before ending, run: find the files you created, verify imports resolve, run lint, run tests.",
		"If something is incomplete, keep working.",
	)

	return strings.Join(parts, "\n")
}

func loadProjectContext(missionDir, projectDir string) string {
	if missionDir != "" {
		if cached := readFileContent(filepath.Join(missionDir, "project-context.md")); cached != "" {
			return cached
		}
	}
	if projectDir != "" {
		return GatherProjectContext(projectDir)
	}
	return ""
}

func ParseStreamLine(line string) string {
	var ev map[string]any
	if err := json.Unmarshal([]byte(line), &ev); err != nil {
		return ""
	}
	v := false
	p := &streamParser{verbose: &v}
	lines := p.parse(ev)
	if len(lines) > 0 {
		return lines[0]
	}
	return ""
}

func extractToolResultText(cm map[string]any) string {
	switch c := cm["content"].(type) {
	case string:
		return strings.TrimSpace(c)
	case []any:
		var parts []string
		for _, item := range c {
			block, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if t, ok := block["text"].(string); ok {
				parts = append(parts, strings.TrimSpace(t))
			}
		}
		return strings.Join(parts, " ")
	}
	return ""
}

func extractToolDetail(input string) string {
	var data map[string]any
	if err := json.Unmarshal([]byte(input), &data); err != nil {
		return ""
	}
	if desc, ok := data["description"].(string); ok && desc != "" {
		return " " + desc
	}
	if fp, ok := data["file_path"].(string); ok {
		return " " + fp
	}
	if cmd, ok := data["command"].(string); ok {
		return " " + cmd
	}
	if pattern, ok := data["pattern"].(string); ok {
		return " \"" + pattern + "\""
	}
	return ""
}
