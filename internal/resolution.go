package internal

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

const (
	ResolutionOpen            = "open"
	ResolutionResolvedViaFix  = "resolved_via_fix"
	ResolutionResolvedTainted = "resolved_tainted"
	ResolutionUnresolved      = "unresolved"
)

type FeatureOutcome struct {
	EffectiveDone bool
	Resolution    string
	ResolvedBy    string
	Tainted       bool
}

type outcomeResolver struct {
	featuresByID map[string]Feature
	fixesByRoot  map[string][]string
	tainted      map[string]bool
	memo         map[string]FeatureOutcome
	visiting     map[string]bool
}

func buildFeatureOutcomes(features []Feature, tainted map[string]bool) map[string]FeatureOutcome {
	resolver := &outcomeResolver{
		featuresByID: make(map[string]Feature, len(features)),
		fixesByRoot:  make(map[string][]string),
		tainted:      tainted,
		memo:         make(map[string]FeatureOutcome, len(features)),
		visiting:     make(map[string]bool),
	}

	for _, f := range features {
		if _, exists := resolver.featuresByID[f.ID]; !exists {
			resolver.featuresByID[f.ID] = f
		}
	}

	for id, f := range resolver.featuresByID {
		if f.Fixes != "" {
			resolver.fixesByRoot[f.Fixes] = append(resolver.fixesByRoot[f.Fixes], id)
		}
	}

	outcomes := make(map[string]FeatureOutcome, len(resolver.featuresByID))
	for id := range resolver.featuresByID {
		outcomes[id] = resolver.resolve(id)
	}
	return outcomes
}

func (r *outcomeResolver) resolve(featureID string) FeatureOutcome {
	if out, ok := r.memo[featureID]; ok {
		return out
	}
	if r.visiting[featureID] {
		// Defensive cycle breaker.
		return FeatureOutcome{EffectiveDone: false, Resolution: ResolutionUnresolved}
	}

	f, ok := r.featuresByID[featureID]
	if !ok {
		return FeatureOutcome{EffectiveDone: true, Resolution: ResolutionOpen}
	}

	r.visiting[featureID] = true
	defer delete(r.visiting, featureID)

	switch f.Status {
	case "done", "validated":
		out := FeatureOutcome{
			EffectiveDone: true,
			Resolution:    ResolutionOpen,
			ResolvedBy:    featureID,
			Tainted:       r.tainted[featureID],
		}
		if out.Tainted {
			out.Resolution = ResolutionResolvedTainted
		}
		r.memo[featureID] = out
		return out
	case "blocked":
		children := r.fixesByRoot[featureID]
		if len(children) == 0 {
			out := FeatureOutcome{EffectiveDone: false, Resolution: ResolutionUnresolved}
			r.memo[featureID] = out
			return out
		}

		allDone := true
		anyTainted := false
		resolvedBy := ""
		for _, childID := range children {
			child := r.resolve(childID)
			if !child.EffectiveDone {
				allDone = false
			}
			if child.Tainted {
				anyTainted = true
			}
			if resolvedBy == "" && child.ResolvedBy != "" {
				resolvedBy = child.ResolvedBy
			}
		}

		if allDone {
			out := FeatureOutcome{
				EffectiveDone: true,
				Resolution:    ResolutionResolvedViaFix,
				ResolvedBy:    resolvedBy,
				Tainted:       anyTainted,
			}
			if anyTainted {
				out.Resolution = ResolutionResolvedTainted
			}
			r.memo[featureID] = out
			return out
		}

		out := FeatureOutcome{EffectiveDone: false, Resolution: ResolutionUnresolved}
		r.memo[featureID] = out
		return out
	default:
		out := FeatureOutcome{EffectiveDone: false, Resolution: ResolutionOpen}
		r.memo[featureID] = out
		return out
	}
}

func loadTaintedFeatureIDs(missionDir string, features []Feature) map[string]bool {
	tainted := make(map[string]bool)
	if missionDir == "" || len(features) == 0 {
		return tainted
	}

	seen := make(map[string]struct{}, len(features))
	for _, f := range features {
		if f.ID == "" {
			continue
		}
		if _, ok := seen[f.ID]; ok {
			continue
		}
		seen[f.ID] = struct{}{}

		path := filepath.Join(missionDir, "runs", f.ID+"-validator.json")
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var report ValidatorReport
		if err := json.Unmarshal(data, &report); err != nil {
			continue
		}
		if reportHasTaintedNote(report) {
			tainted[f.ID] = true
		}
	}

	return tainted
}

func reportHasTaintedNote(report ValidatorReport) bool {
	for _, note := range report.Notes {
		if strings.Contains(strings.ToUpper(note), "TAINTED") {
			return true
		}
	}
	return false
}
