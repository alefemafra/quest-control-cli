package internal

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const criticPhaseStateFileName = "critic-phase-state.json"
const criticArtifactMissingHash = "<missing>"

const (
	criticArtifactValidationContract = "validation-contract"
	criticArtifactFeatures           = "features"
	criticArtifactProjectContext     = "project-context"
	criticArtifactLocalCriteria      = "local-criteria"
	criticArtifactClaudeMd           = "claude-md"
)

var criticTrackedArtifacts = []string{
	criticArtifactValidationContract,
	criticArtifactFeatures,
	criticArtifactProjectContext,
	criticArtifactLocalCriteria,
	criticArtifactClaudeMd,
}

type criticPhaseCacheEntry struct {
	PhaseID   string        `json:"phase_id"`
	InputHash string        `json:"input_hash"`
	Overall   string        `json:"overall"`
	Report    *CriticReport `json:"report,omitempty"`
	UpdatedAt string        `json:"updated_at,omitempty"`
}

type criticPhaseState struct {
	Phases    map[string]criticPhaseCacheEntry `json:"phases"`
	UpdatedAt string                           `json:"updated_at,omitempty"`
}

type criticPhasePlan struct {
	Phase        criticPhase
	InputHash    string
	ReuseCached  bool
	CachedReport *CriticReport
}

type criticLoadedArtifact struct {
	Path    string
	Content string
}

type criticArtifactSnapshot struct {
	Hashes map[string]string
}

var criticPhaseStateFileMu sync.Mutex

func newCriticPhaseState() criticPhaseState {
	return criticPhaseState{
		Phases: make(map[string]criticPhaseCacheEntry),
	}
}

func ensureCriticPhaseState(state *criticPhaseState) {
	if state.Phases == nil {
		state.Phases = make(map[string]criticPhaseCacheEntry)
	}
}

func criticPhaseStatePath(missionDir string) string {
	return filepath.Join(missionDir, "runs", criticPhaseStateFileName)
}

func clearCriticPhaseExecutionState(missionDir string) error {
	criticPhaseStateFileMu.Lock()
	defer criticPhaseStateFileMu.Unlock()
	if missionDir == "" {
		return nil
	}
	err := os.Remove(criticPhaseStatePath(missionDir))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func invalidateCriticPhaseExecutionState(missionDir string, phaseIDs []string) error {
	criticPhaseStateFileMu.Lock()
	defer criticPhaseStateFileMu.Unlock()
	if missionDir == "" || len(phaseIDs) == 0 {
		return nil
	}
	state := loadCriticPhaseStateLocked(missionDir)
	if len(state.Phases) == 0 {
		return nil
	}
	for _, phaseID := range phaseIDs {
		delete(state.Phases, strings.TrimSpace(phaseID))
	}
	return saveCriticPhaseStateLocked(missionDir, state)
}

func loadCriticPhaseState(missionDir string) criticPhaseState {
	criticPhaseStateFileMu.Lock()
	defer criticPhaseStateFileMu.Unlock()
	return loadCriticPhaseStateLocked(missionDir)
}

func loadCriticPhaseStateLocked(missionDir string) criticPhaseState {
	state := newCriticPhaseState()
	if missionDir == "" {
		return state
	}
	data, err := os.ReadFile(criticPhaseStatePath(missionDir))
	if err != nil {
		return state
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return newCriticPhaseState()
	}
	ensureCriticPhaseState(&state)
	return state
}

func saveCriticPhaseState(missionDir string, state criticPhaseState) error {
	criticPhaseStateFileMu.Lock()
	defer criticPhaseStateFileMu.Unlock()
	return saveCriticPhaseStateLocked(missionDir, state)
}

func saveCriticPhaseStateLocked(missionDir string, state criticPhaseState) error {
	if missionDir == "" {
		return nil
	}
	ensureCriticPhaseState(&state)
	state.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	runsDir := filepath.Join(missionDir, "runs")
	if err := os.MkdirAll(runsDir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(criticPhaseStatePath(missionDir), data, 0o644)
}

func buildCriticPhaseExecutionPlan(specDir, missionDir string, phases []criticPhase) ([]criticPhasePlan, criticPhaseState) {
	state := loadCriticPhaseState(missionDir)
	plans := make([]criticPhasePlan, 0, len(phases))
	for _, phase := range phases {
		inputHash := computeCriticPhaseInputHash(specDir, phase)
		plan := criticPhasePlan{
			Phase:     phase,
			InputHash: inputHash,
		}
		entry, ok := state.Phases[phase.ID]
		if ok &&
			entry.InputHash == inputHash &&
			strings.EqualFold(strings.TrimSpace(entry.Overall), "pass") &&
			entry.Report != nil &&
			strings.EqualFold(strings.TrimSpace(entry.Report.Overall), "pass") {
			plan.ReuseCached = true
			plan.CachedReport = normalizeCriticReportForPhase(entry.Report, phase.ID)
		}
		plans = append(plans, plan)
	}
	return plans, state
}

func persistCriticPhaseExecutionState(missionDir string, state criticPhaseState, plans []criticPhasePlan, reports []*CriticReport, errs []error) error {
	ensureCriticPhaseState(&state)
	for i, plan := range plans {
		phaseID := strings.TrimSpace(plan.Phase.ID)
		if phaseID == "" {
			continue
		}
		if i < len(errs) && errs[i] != nil {
			delete(state.Phases, phaseID)
			continue
		}
		if i >= len(reports) || reports[i] == nil {
			delete(state.Phases, phaseID)
			continue
		}
		report := normalizeCriticReportForPhase(reports[i], phaseID)
		state.Phases[phaseID] = criticPhaseCacheEntry{
			PhaseID:   phaseID,
			InputHash: plan.InputHash,
			Overall:   strings.TrimSpace(report.Overall),
			Report:    report,
			UpdatedAt: time.Now().UTC().Format(time.RFC3339),
		}
	}
	return saveCriticPhaseState(missionDir, state)
}

func normalizeCriticReportForPhase(report *CriticReport, phaseID string) *CriticReport {
	if report == nil {
		return nil
	}
	cloned := *report
	if strings.TrimSpace(cloned.Phase) == "" {
		cloned.Phase = phaseID
	}
	return &cloned
}

func computeCriticPhaseInputHash(specDir string, phase criticPhase) string {
	hasher := sha256.New()
	writePart := func(part string) {
		_, _ = hasher.Write([]byte(part))
		_, _ = hasher.Write([]byte{0})
	}

	writePart("phase:" + phase.ID)
	writePart("name:" + phase.Name)
	writePart("criteria:" + phase.Criteria)
	writePart("focus:" + phase.Focus)
	writePart("skill:" + ReadSkill("quest-critic"))

	artifacts := loadCriticPhaseArtifacts(specDir, phase)
	for _, artifact := range artifacts {
		writePart("path:" + artifact.Path)
		writePart("content:" + artifact.Content)
	}

	return hex.EncodeToString(hasher.Sum(nil))
}

func loadCriticPhaseArtifacts(specDir string, phase criticPhase) []criticLoadedArtifact {
	missionDir := ResolveArtifactDir(specDir)
	projectRoot := filepath.Dir(filepath.Dir(filepath.Dir(specDir)))

	loadArtifact := func(name string) (criticLoadedArtifact, bool) {
		var abs string
		switch name {
		case "CLAUDE.md":
			abs = filepath.Join(projectRoot, "CLAUDE.md")
		default:
			abs = filepath.Join(missionDir, name)
		}
		if !fileExists(abs) {
			return criticLoadedArtifact{}, false
		}
		return criticLoadedArtifact{
			Path:    abs,
			Content: readFileContent(abs),
		}, true
	}

	var artifacts []criticLoadedArtifact
	if criteria := readCriteriaMd(); strings.TrimSpace(criteria) != "" {
		artifacts = append(artifacts, criticLoadedArtifact{
			Path:    criteriaMdPath(),
			Content: criteria,
		})
	}
	for _, name := range phase.Artifacts {
		if artifact, ok := loadArtifact(name); ok {
			artifacts = append(artifacts, artifact)
		}
	}
	localCriteriaPath := filepath.Join(missionDir, "critique-criteria.local.md")
	if fileExists(localCriteriaPath) {
		artifacts = append(artifacts, criticLoadedArtifact{
			Path:    localCriteriaPath,
			Content: readFileContent(localCriteriaPath),
		})
	}
	return artifacts
}

func captureCriticArtifactSnapshot(missionDir string) (criticArtifactSnapshot, error) {
	specDir := filepath.Dir(missionDir)
	projectRoot := filepath.Dir(filepath.Dir(filepath.Dir(specDir)))
	artifactPaths := map[string]string{
		criticArtifactValidationContract: filepath.Join(missionDir, "validation-contract.md"),
		criticArtifactFeatures:           filepath.Join(missionDir, "features.json"),
		criticArtifactProjectContext:     filepath.Join(missionDir, "project-context.md"),
		criticArtifactLocalCriteria:      filepath.Join(missionDir, "critique-criteria.local.md"),
		criticArtifactClaudeMd:           filepath.Join(projectRoot, "CLAUDE.md"),
	}

	snapshot := criticArtifactSnapshot{Hashes: make(map[string]string, len(artifactPaths))}
	for key, path := range artifactPaths {
		hash, err := hashFileOrMissing(path)
		if err != nil {
			return snapshot, err
		}
		snapshot.Hashes[key] = hash
	}
	return snapshot, nil
}

func hashFileOrMissing(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return criticArtifactMissingHash, nil
		}
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func changedCriticArtifacts(before, after criticArtifactSnapshot) ([]string, error) {
	if before.Hashes == nil || after.Hashes == nil {
		return nil, errors.New("invalid critic artifact snapshot")
	}
	changed := make([]string, 0, len(criticTrackedArtifacts))
	for _, key := range criticTrackedArtifacts {
		beforeHash, beforeOK := before.Hashes[key]
		afterHash, afterOK := after.Hashes[key]
		if !beforeOK || !afterOK {
			return nil, errors.New("incomplete critic artifact snapshot")
		}
		if beforeHash != afterHash {
			changed = append(changed, key)
		}
	}
	sort.Strings(changed)
	return changed, nil
}

func determineCriticPhaseInvalidation(changedArtifacts []string) ([]string, bool) {
	if len(changedArtifacts) == 0 {
		return nil, false
	}
	phases := make(map[string]struct{})
	for _, artifact := range changedArtifacts {
		switch strings.TrimSpace(artifact) {
		case criticArtifactValidationContract:
			phases["A"] = struct{}{}
			phases["C"] = struct{}{}
		case criticArtifactFeatures:
			phases["C"] = struct{}{}
		case criticArtifactProjectContext, criticArtifactClaudeMd:
			phases["B"] = struct{}{}
		case criticArtifactLocalCriteria:
			phases["A"] = struct{}{}
			phases["B"] = struct{}{}
			phases["C"] = struct{}{}
		default:
			return nil, true
		}
	}

	ordered := make([]string, 0, len(phases))
	for _, phaseID := range []string{"A", "B", "C"} {
		if _, ok := phases[phaseID]; ok {
			ordered = append(ordered, phaseID)
		}
	}
	return ordered, false
}
