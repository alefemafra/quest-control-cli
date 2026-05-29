package internal

import (
	"path/filepath"
	"strings"
	"testing"
)

// resultEvent builds a stream-json `result` event the way encoding/json would
// produce it (all numbers as float64).
func resultEvent(cost float64, in, out, cacheCreate, cacheRead float64) map[string]any {
	return map[string]any{
		"type":           "result",
		"total_cost_usd": cost,
		"usage": map[string]any{
			"input_tokens":                in,
			"output_tokens":               out,
			"cache_creation_input_tokens": cacheCreate,
			"cache_read_input_tokens":     cacheRead,
		},
	}
}

func TestExtractStreamUsage(t *testing.T) {
	u := extractStreamUsage(resultEvent(0.1234, 100, 250, 30, 5000))
	if u.cost != 0.1234 {
		t.Errorf("cost = %v, want 0.1234", u.cost)
	}
	if u.input != 100 || u.output != 250 || u.cacheCreation != 30 || u.cacheRead != 5000 {
		t.Errorf("tokens = (%d,%d,%d,%d), want (100,250,30,5000)", u.input, u.output, u.cacheCreation, u.cacheRead)
	}
}

func TestExtractStreamUsage_LegacyCostField(t *testing.T) {
	ev := map[string]any{"type": "result", "cost_usd": 0.5}
	if u := extractStreamUsage(ev); u.cost != 0.5 {
		t.Errorf("legacy cost_usd not read: got %v, want 0.5", u.cost)
	}
}

func TestExtractStreamUsage_Empty(t *testing.T) {
	if u := extractStreamUsage(map[string]any{"type": "result"}); u.cost != 0 || u.input != 0 {
		t.Errorf("expected zero usage, got %+v", u)
	}
}

func TestExtractInitModel(t *testing.T) {
	init := map[string]any{"type": "system", "subtype": "init", "model": "claude-opus-4-8"}
	if m := extractInitModel(init); m != "claude-opus-4-8" {
		t.Errorf("init model = %q, want claude-opus-4-8", m)
	}
	// Non-init events carry no model.
	if m := extractInitModel(map[string]any{"type": "result", "model": "x"}); m != "" {
		t.Errorf("non-init event returned model %q, want empty", m)
	}
	if m := extractInitModel(map[string]any{"type": "system", "subtype": "other"}); m != "" {
		t.Errorf("non-init system event returned model %q, want empty", m)
	}
}

func TestCostTrackerRecordAndPersist(t *testing.T) {
	dir := t.TempDir()
	c := LoadCostTracker(dir)

	c.Record(CostRecord{Model: "opus", Role: "worker", FeatureID: "F1", Phase: 1, InputTokens: 100, OutputTokens: 50, CostUSD: 0.2})
	c.Record(CostRecord{Model: "sonnet", Role: "validator", FeatureID: "F1", Phase: 1, InputTokens: 10, OutputTokens: 5, CostUSD: 0.01})
	// Zero-usage records are dropped.
	c.Record(CostRecord{Model: "opus", Role: "worker"})

	if got := len(c.Records()); got != 2 {
		t.Fatalf("records = %d, want 2 (zero-usage record should be dropped)", got)
	}

	// Round-trip: a fresh tracker on the same dir sees the persisted records.
	reloaded := LoadCostTracker(dir)
	recs := reloaded.Records()
	if len(recs) != 2 {
		t.Fatalf("reloaded records = %d, want 2", len(recs))
	}
	if recs[0].Model != "opus" || recs[0].CostUSD != 0.2 {
		t.Errorf("reloaded record[0] = %+v, want opus/0.2", recs[0])
	}
	if reloaded.path != filepath.Join(dir, costFileName) {
		t.Errorf("tracker path = %q, want %q", reloaded.path, filepath.Join(dir, costFileName))
	}
}

func sampleCostRecords() []CostRecord {
	return []CostRecord{
		{Model: "opus", Role: "worker", FeatureID: "F1", Phase: 0, InputTokens: 100, OutputTokens: 200, CacheReadTokens: 10, CostUSD: 0.30},
		{Model: "opus", Role: "worker", FeatureID: "F2", Phase: 1, InputTokens: 100, OutputTokens: 100, CostUSD: 0.20},
		{Model: "sonnet", Role: "validator", FeatureID: "F1", Phase: 0, InputTokens: 50, OutputTokens: 20, CostUSD: 0.05},
		{Model: "", Role: "discovery", FeatureID: "", Phase: -1, InputTokens: 40, OutputTokens: 10, CostUSD: 0.02},
	}
}

func TestCostTotals(t *testing.T) {
	total := CostTotals(sampleCostRecords())
	if total.Calls != 4 {
		t.Errorf("calls = %d, want 4", total.Calls)
	}
	if total.Input != 290 || total.Output != 330 || total.Cache != 10 {
		t.Errorf("tokens = (%d,%d,%d), want (290,330,10)", total.Input, total.Output, total.Cache)
	}
	if got := total.Cost; got < 0.569 || got > 0.571 {
		t.Errorf("cost = %v, want ~0.57", got)
	}
}

func TestAggregateCostBy_Model(t *testing.T) {
	rows := AggregateCostBy(sampleCostRecords(), func(r CostRecord) string {
		if r.Model == "" {
			return "(default)"
		}
		return r.Model
	})
	if len(rows) != 3 {
		t.Fatalf("model groups = %d, want 3 (opus, sonnet, default)", len(rows))
	}
	// Sorted by cost desc: opus (0.50) first.
	if rows[0].Key != "opus" {
		t.Errorf("top model = %q, want opus", rows[0].Key)
	}
	if rows[0].Calls != 2 || rows[0].Cost < 0.499 || rows[0].Cost > 0.501 {
		t.Errorf("opus rollup = %+v, want 2 calls / ~0.50", rows[0])
	}
}

func TestRenderCostTab_EmptyAndPopulated(t *testing.T) {
	m := NewModel(t.TempDir(), false, "")

	// Empty state must not panic and should signal there's no data yet.
	if out := m.renderCostTab(); out == "" {
		t.Error("empty cost tab rendered nothing")
	}

	dir := t.TempDir()
	tracker := LoadCostTracker(dir)
	for _, r := range sampleCostRecords() {
		tracker.Record(r)
	}
	m.costTracker = tracker
	m.mission.Features = []Feature{{ID: "F1", Title: "First feature"}, {ID: "F2", Title: "Second"}}

	out := m.renderCostTab()
	for _, want := range []string{"By model", "By role", "By phase", "By feature", "opus", "First feature"} {
		if !strings.Contains(out, want) {
			t.Errorf("cost tab missing %q", want)
		}
	}
}

func TestAggregateCostBy_SkipsEmptyKey(t *testing.T) {
	// By feature: the discovery record has an empty FeatureID and must be excluded.
	rows := AggregateCostBy(sampleCostRecords(), func(r CostRecord) string { return r.FeatureID })
	for _, r := range rows {
		if r.Key == "" {
			t.Fatalf("aggregation kept an empty key: %+v", rows)
		}
	}
	if len(rows) != 2 {
		t.Errorf("feature groups = %d, want 2 (F1, F2)", len(rows))
	}
}
