package internal

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

func BuildRefinementPrompt(feature Feature, report ValidatorReport, missionDir, specDir string) string {
	refinementSkill := ReadSkill("quest-refinement")

	contract := readFileContent(filepath.Join(missionDir, "validation-contract.md"))
	knowledge := readFileContent(filepath.Join(missionDir, "knowledge-base.md"))
	projectContext := readFileContent(filepath.Join(missionDir, "project-context.md"))

	reportJSON, _ := json.MarshalIndent(report, "", "  ")

	var failedRefs []string
	for _, a := range report.Assertions {
		if a.Result == "FAIL" || a.Result == "BLOCKED" {
			failedRefs = append(failedRefs, a.ID)
		}
	}
	filteredContract := FilterContractAssertions(contract, failedRefs)

	var parts []string
	parts = append(parts,
		"You are running the quest-refinement skill. Follow it precisely.",
		"",
		"## Skill Reference",
		"",
		refinementSkill,
		"",
		"---",
		"",
	)

	if projectContext != "" {
		parts = append(parts, "## Project Context", "", projectContext, "")
	}

	parts = append(parts,
		fmt.Sprintf("## Failed Feature: %s — %s", feature.ID, feature.Title),
		fmt.Sprintf("Spec folder: %s", specDir),
		fmt.Sprintf("Scope: %s", feature.Scope),
		"",
		"## Validator Report (FAILs to address)",
		"",
		string(reportJSON),
		"",
		"## Validation Contract (failed assertions)",
		"",
		filteredContract,
		"",
	)

	if knowledge != "" {
		parts = append(parts, "## Knowledge Base", "", CompactKnowledge(knowledge), "")
	}

	parts = append(parts,
		"## Instructions",
		"",
		"1. Analyze each FAIL assertion — find the root cause, not surface cause",
		"2. Generate minimum-scope fix features",
		"3. Each fix feature must have: id, title, status (pending), depends_on, scope, validation_refs, fixes, addresses",
		"",
		"Output ONLY a valid JSON array of fix features:",
		fmt.Sprintf(`[{"id":"%s-fix-1","title":"...","status":"pending","phase":%d,"depends_on":["%s"],"scope":"...","validation_refs":["..."],"fixes":"%s","addresses":["..."]}]`, feature.ID, feature.Phase, feature.ID, feature.ID),
		"",
		"Output ONLY the JSON array, nothing else.",
	)

	return strings.Join(parts, "\n")
}

func ParseFixFeatures(text string) []Feature {
	text = strings.TrimSpace(text)

	var features []Feature
	if err := json.Unmarshal([]byte(text), &features); err == nil && len(features) > 0 {
		return features
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
			if err := json.Unmarshal([]byte(strings.TrimSpace(block)), &features); err == nil && len(features) > 0 {
				return features
			}
		}
	}

	// Try finding array in text
	start := strings.Index(text, "[")
	if start >= 0 {
		depth := 0
		for i := start; i < len(text); i++ {
			if text[i] == '[' {
				depth++
			} else if text[i] == ']' {
				depth--
				if depth == 0 {
					if err := json.Unmarshal([]byte(text[start:i+1]), &features); err == nil && len(features) > 0 {
						return features
					}
					break
				}
			}
		}
	}

	return nil
}

func AddFixFeatures(missionDir string, fixes []Feature, originalID string, fileMu *sync.Mutex) error {
	fileMu.Lock()
	defer fileMu.Unlock()

	path := filepath.Join(missionDir, "features.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	var manifest FeaturesManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return err
	}

	rootIDs := make(map[string]struct{}, len(manifest.Features))
	for _, f := range manifest.Features {
		rootIDs[f.ID] = struct{}{}
	}

	normalizedFixes, err := normalizeFixFeatures(manifest.FixFeatures, rootIDs)
	if err != nil {
		return err
	}
	manifest.FixFeatures = normalizedFixes

	// Original feature may be either a root feature or a fix feature (nested refinements).
	for i := range manifest.Features {
		if manifest.Features[i].ID == originalID {
			manifest.Features[i].Status = "blocked"
		}
	}
	for i := range manifest.FixFeatures {
		if manifest.FixFeatures[i].ID == originalID {
			manifest.FixFeatures[i].Status = "blocked"
		}
	}

	for i := range fixes {
		if fixes[i].Status == "" {
			fixes[i].Status = "pending"
		}

		if _, collidesWithRoot := rootIDs[fixes[i].ID]; collidesWithRoot {
			return fmt.Errorf("fix id %q collides with root feature id", fixes[i].ID)
		}
	}

	indexByID := make(map[string]int, len(manifest.FixFeatures))
	for i := range manifest.FixFeatures {
		indexByID[manifest.FixFeatures[i].ID] = i
	}

	for _, fix := range fixes {
		if idx, exists := indexByID[fix.ID]; exists {
			manifest.FixFeatures[idx] = mergeFixFeatureEntries(manifest.FixFeatures[idx], fix)
			continue
		}

		manifest.FixFeatures = append(manifest.FixFeatures, fix)
		indexByID[fix.ID] = len(manifest.FixFeatures) - 1
	}

	if err := validateUniqueFeatureIDs(manifest.Features, manifest.FixFeatures); err != nil {
		return err
	}

	out, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, out, 0o644)
}

func normalizeFixFeatures(fixes []Feature, rootIDs map[string]struct{}) ([]Feature, error) {
	if len(fixes) == 0 {
		return nil, nil
	}

	seen := make(map[string]int, len(fixes))
	normalized := make([]Feature, 0, len(fixes))

	for _, f := range fixes {
		if f.ID == "" {
			return nil, fmt.Errorf("fix feature with empty id")
		}
		if _, collidesWithRoot := rootIDs[f.ID]; collidesWithRoot {
			return nil, fmt.Errorf("fix id %q collides with root feature id", f.ID)
		}

		if idx, exists := seen[f.ID]; exists {
			normalized[idx] = mergeFixFeatureEntries(normalized[idx], f)
			continue
		}

		seen[f.ID] = len(normalized)
		normalized = append(normalized, f)
	}

	return normalized, nil
}

func mergeFixFeatureEntries(base Feature, incoming Feature) Feature {
	merged := base

	if incoming.Title != "" {
		merged.Title = incoming.Title
	}
	if incoming.Phase != 0 || base.Phase == 0 {
		merged.Phase = incoming.Phase
	}
	if len(incoming.DependsOn) > 0 {
		merged.DependsOn = incoming.DependsOn
	}
	if incoming.Scope != "" {
		merged.Scope = incoming.Scope
	}
	if len(incoming.ValidationRefs) > 0 {
		merged.ValidationRefs = incoming.ValidationRefs
	}
	if incoming.Fixes != "" {
		merged.Fixes = incoming.Fixes
	}
	if len(incoming.Addresses) > 0 {
		merged.Addresses = incoming.Addresses
	}
	if incoming.Resolution != "" {
		merged.Resolution = incoming.Resolution
	}
	if incoming.ResolvedBy != "" {
		merged.ResolvedBy = incoming.ResolvedBy
	}
	if incoming.ResolvedAt != "" {
		merged.ResolvedAt = incoming.ResolvedAt
	}
	if incoming.Tainted {
		merged.Tainted = true
	}

	merged.Status = pickHigherPriorityStatus(base.Status, incoming.Status)
	return merged
}

func pickHigherPriorityStatus(a, b string) string {
	if b == "" {
		return a
	}
	if a == "" {
		return b
	}

	priority := map[string]int{
		"pending":             0,
		"in_progress":         1,
		"awaiting_validation": 2,
		"validating":          3,
		"refining":            4,
		"blocked":             5,
		"done":                6,
		"validated":           6,
	}

	pa, okA := priority[a]
	pb, okB := priority[b]
	if !okA {
		pa = -1
	}
	if !okB {
		pb = -1
	}

	if pb > pa {
		return b
	}
	return a
}

func validateUniqueFeatureIDs(features []Feature, fixFeatures []Feature) error {
	seen := make(map[string]string, len(features)+len(fixFeatures))

	for _, f := range features {
		if f.ID == "" {
			return fmt.Errorf("feature with empty id")
		}
		if prev, exists := seen[f.ID]; exists {
			return fmt.Errorf("duplicate feature id %q in %s and features", f.ID, prev)
		}
		seen[f.ID] = "features"
	}

	for _, f := range fixFeatures {
		if f.ID == "" {
			return fmt.Errorf("fix feature with empty id")
		}
		if prev, exists := seen[f.ID]; exists {
			return fmt.Errorf("duplicate feature id %q in %s and fix_features", f.ID, prev)
		}
		seen[f.ID] = "fix_features"
	}

	return nil
}
