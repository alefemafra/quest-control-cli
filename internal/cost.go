package internal

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

const costFileName = "cost.json"

// CostRecord is one billable Claude call, tagged with the dimensions we want to
// pivot on: model, role, and (for execution-phase calls) feature + phase.
// Discovery/plan-generation calls use Phase = -1 and an empty FeatureID.
type CostRecord struct {
	Ts                  string  `json:"ts"`
	Model               string  `json:"model"`
	Role                string  `json:"role"`
	FeatureID           string  `json:"featureId,omitempty"`
	Phase               int     `json:"phase"`
	InputTokens         int     `json:"inputTokens"`
	OutputTokens        int     `json:"outputTokens"`
	CacheCreationTokens int     `json:"cacheCreationTokens"`
	CacheReadTokens     int     `json:"cacheReadTokens"`
	CostUSD             float64 `json:"costUsd"`
}

type costFile struct {
	Records []CostRecord `json:"records"`
}

// CostTracker accumulates CostRecords for a single mission and persists them to
// <missionDir>/cost.json. It is safe for concurrent use by the WorkerPool
// goroutines and the TUI Model.
type CostTracker struct {
	mu      sync.Mutex
	path    string
	records []CostRecord
}

// LoadCostTracker creates a tracker bound to <missionDir>/cost.json, loading any
// existing records so cost accumulates across re-opened missions.
func LoadCostTracker(missionDir string) *CostTracker {
	path := filepath.Join(missionDir, costFileName)
	c := &CostTracker{path: path}
	if data, err := os.ReadFile(path); err == nil {
		var cf costFile
		if json.Unmarshal(data, &cf) == nil {
			c.records = cf.Records
		}
	}
	return c
}

// Record appends a single call's cost and persists. Calls that produced no
// tokens and no cost (e.g. a process that errored before any result) are
// dropped so they don't pollute the aggregates.
func (c *CostTracker) Record(rec CostRecord) {
	if rec.CostUSD == 0 && rec.InputTokens == 0 && rec.OutputTokens == 0 &&
		rec.CacheCreationTokens == 0 && rec.CacheReadTokens == 0 {
		return
	}
	if rec.Ts == "" {
		rec.Ts = time.Now().UTC().Format(time.RFC3339)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.records = append(c.records, rec)
	c.persistLocked()
}

// RecordMany appends a batch (used to flush buffered discovery/plan-gen records
// once the mission directory exists).
func (c *CostTracker) RecordMany(recs []CostRecord) {
	for _, r := range recs {
		c.Record(r)
	}
}

// Records returns a snapshot copy of all accumulated records.
func (c *CostTracker) Records() []CostRecord {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]CostRecord, len(c.records))
	copy(out, c.records)
	return out
}

func (c *CostTracker) persistLocked() {
	data, err := json.MarshalIndent(costFile{Records: c.records}, "", "  ")
	if err != nil {
		return
	}
	// The mission directory may not exist yet when buffered discovery/plan-gen
	// records are first flushed; make sure it does before writing.
	_ = os.MkdirAll(filepath.Dir(c.path), 0o755)
	_ = os.WriteFile(c.path, data, 0o644)
}

// CostRollup is an aggregate over a set of records sharing one key.
type CostRollup struct {
	Key    string
	Calls  int
	Input  int
	Output int
	Cache  int
	Cost   float64
}

func (r *CostRollup) add(rec CostRecord) {
	r.Calls++
	r.Input += rec.InputTokens
	r.Output += rec.OutputTokens
	r.Cache += rec.CacheCreationTokens + rec.CacheReadTokens
	r.Cost += rec.CostUSD
}

// CostTotals sums every record into a single rollup.
func CostTotals(records []CostRecord) CostRollup {
	var total CostRollup
	for _, rec := range records {
		total.add(rec)
	}
	return total
}

// AggregateCostBy groups records by keyFn and returns rollups sorted by cost
// descending (ties broken by key for stable output). Records whose key is empty
// are skipped — this lets the by-feature view naturally exclude non-feature
// calls (discovery, plan generation, app-level critic).
func AggregateCostBy(records []CostRecord, keyFn func(CostRecord) string) []CostRollup {
	byKey := map[string]*CostRollup{}
	var order []string
	for _, rec := range records {
		key := keyFn(rec)
		if key == "" {
			continue
		}
		r, ok := byKey[key]
		if !ok {
			r = &CostRollup{Key: key}
			byKey[key] = r
			order = append(order, key)
		}
		r.add(rec)
	}
	out := make([]CostRollup, 0, len(order))
	for _, key := range order {
		out = append(out, *byKey[key])
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Cost != out[j].Cost {
			return out[i].Cost > out[j].Cost
		}
		return out[i].Key < out[j].Key
	})
	return out
}
