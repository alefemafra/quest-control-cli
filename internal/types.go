package internal

type Phase int

const (
	PhaseSpecSelect Phase = iota
	PhaseDiscovery
	PhaseRunning
	PhaseReview
	PhaseDashboard
)

type Tab int

const (
	TabOverview Tab = iota
	TabKanban
	TabLog
	TabDiagram
	TabCost
)

var TabOrder = []Tab{TabOverview, TabKanban, TabLog, TabDiagram, TabCost}

type ReviewTab int

const (
	ReviewTabChat ReviewTab = iota
	ReviewTabPlan
	ReviewTabSpec
	ReviewTabContract
	ReviewTabCritic
)

var ReviewTabOrder = []ReviewTab{ReviewTabChat, ReviewTabPlan, ReviewTabSpec, ReviewTabContract, ReviewTabCritic}

type ChatMessage struct {
	Role string // "user", "assistant", "system"
	Text string
}

type SpecEntry struct {
	Slug     string
	SpecPath string
	Title    string
	Mission  MissionState
}

type Feature struct {
	ID             string   `json:"id"`
	Title          string   `json:"title"`
	Phase          int      `json:"phase"`
	Status         string   `json:"status"`
	DependsOn      []string `json:"depends_on"`
	Scope          string   `json:"scope"`
	Description    string   `json:"description,omitempty"`
	ValidationRefs []string `json:"validation_refs"`
	Fixes          string   `json:"fixes,omitempty"`
	Addresses      []string `json:"addresses,omitempty"`
	RootCauseHypothesis string   `json:"root_cause_hypothesis,omitempty"`
	Evidence            []string `json:"evidence,omitempty"`
	DoneWhen            []string `json:"done_when,omitempty"`
	NonGoals            []string `json:"non_goals,omitempty"`
	RegressionGuards    []string `json:"regression_guards,omitempty"`
	Resolution     string   `json:"resolution,omitempty"`
	ResolvedBy     string   `json:"resolved_by,omitempty"`
	ResolvedAt     string   `json:"resolved_at,omitempty"`
	Tainted        bool     `json:"tainted,omitempty"`
}

type Assertion struct {
	Category string   `json:"category"`
	Items    []string `json:"items"`
}

// AssertionDeltaItem is the compact unit used by Changes mode (C) to update
// validation-contract assertions without regenerating the entire document.
type AssertionDeltaItem struct {
	ID        string `json:"id"`
	Category  string `json:"category"`
	Assertion string `json:"assertion"`
}

type AssertionDelta struct {
	Upsert []AssertionDeltaItem `json:"upsert"`
	Remove []string             `json:"remove"`
}

type FeatureDelta struct {
	Upsert []Feature `json:"upsert"`
	Remove []string  `json:"remove"`
}

type PlanData struct {
	Slug       string      `json:"slug"`
	Spec       string      `json:"spec,omitempty"`
	Project    string      `json:"project"`
	Owner      string      `json:"owner"`
	Features   []Feature   `json:"features"`
	Assertions []Assertion `json:"assertions"`
	Knowledge  []string    `json:"knowledge"`
}

type FeaturesManifest struct {
	Spec            string    `json:"spec,omitempty"`
	StatusLifecycle []string  `json:"status_lifecycle,omitempty"`
	Project         string    `json:"project"`
	Owner           string    `json:"owner"`
	Features        []Feature `json:"features"`
	FixFeatures     []Feature `json:"fix_features"`
}

type QuestStats struct {
	Total              int
	Done               int
	DoneDirect         int
	DoneViaFix         int
	DoneWaived         int
	InProgress         int
	Blocked            int
	BlockedUnresolved  int
	BlockedResolved    int
	BlockedTainted     int
	BlockedWaived      int
	Pending            int
	AwaitingValidation int
	Validating         int
	Refining           int
}

type MissionStats = QuestStats

type QuestState struct {
	Exists   bool
	Project  string
	Owner    string
	Features []Feature
	Stats    QuestStats
	Path     string
}

type MissionState = QuestState

type ClaudeStreamMsg struct {
	Line      string
	Done      bool
	Result    string
	Err       error
	SessionID string

	// Cost/usage captured from the stream-json `system/init` (model) and
	// `result` (usage + total_cost_usd) events. Populated only on Done.
	Model               string
	InputTokens         int
	OutputTokens        int
	CacheCreationTokens int
	CacheReadTokens     int
	CostUSD             float64
}

type WorkerEvent struct {
	FeatureID    string
	Line         string
	Done         bool
	Result       string
	Err          error
	AllDone      bool
	Phase        int
	Role         string // "", "critic", "critic-fix", "validator", "refinement"
	Verdict      string // "PASS", "FAIL", "BLOCKED" (validator only)
	CriticReport *CriticReport
}

type autoFixEventMsg struct {
	line string
	done bool
	err  error
}

type ValidatorAssertion struct {
	ID       string `json:"id"`
	Result   string `json:"result"`
	Evidence string `json:"evidence"`
}

type ValidatorReport struct {
	FeatureID  string               `json:"feature_id"`
	Role       string               `json:"role"`
	StartedAt  string               `json:"started_at"`
	EndedAt    string               `json:"ended_at"`
	Verdict    string               `json:"verdict"`
	Assertions []ValidatorAssertion `json:"assertions"`
	Notes      []string             `json:"notes"`
}

type QualityCommandCandidate struct {
	Command string `json:"command"`
	Scope   string `json:"scope"` // "targeted" | "root"
	Source  string `json:"source"`
}

type QualityCommandPlan struct {
	LintCommands []QualityCommandCandidate `json:"lint_commands"`
	TestCommands []QualityCommandCandidate `json:"test_commands"`
}

type QualityCommandRun struct {
	Command    string `json:"command"`
	Scope      string `json:"scope"`
	Source     string `json:"source"`
	Kind       string `json:"kind"` // "lint" | "test"
	Passed     bool   `json:"passed"`
	Output     string `json:"output,omitempty"`
	DurationMs int64  `json:"duration_ms"`
}

type QualityGateResult struct {
	StartedAt  string              `json:"started_at"`
	EndedAt    string              `json:"ended_at"`
	Passed     bool                `json:"passed"`
	LintPassed bool                `json:"lint_passed"`
	TestPassed bool                `json:"test_passed"`
	LintRuns   []QualityCommandRun `json:"lint_runs"`
	TestRuns   []QualityCommandRun `json:"test_runs"`
	Notes      []string            `json:"notes,omitempty"`
}

type CriticFinding struct {
	Criterion  string `json:"criterion"`
	Status     string `json:"status"`
	Note       string `json:"note,omitempty"`
	Target     string `json:"target,omitempty"`
	Suggestion string `json:"suggestion,omitempty"`
}

type CriticReport struct {
	Phase            string          `json:"phase"`
	Artifact         string          `json:"artifact"`
	StartedAt        string          `json:"started_at"`
	EndedAt          string          `json:"ended_at"`
	MechanicalPassed int             `json:"mechanical_passed"`
	MechanicalFailed int             `json:"mechanical_failed"`
	Findings         []CriticFinding `json:"judgment"`
	Overall          string          `json:"overall"`
	BlockingFindings []string        `json:"blocking_findings"`
}

type LogEntry struct {
	Time      string
	FeatureID string
	Text      string
}

type GenPhase int

const (
	GenPhaseNone       GenPhase = iota
	GenPhaseAnalysis            // Phase 1: Sonnet codebase analysis
	GenPhaseAssertions          // Phase 2: Opus assertions (spec-to-assertions skill)
	GenPhaseFeatures            // Phase 3: Opus feature decomposition only (spec-to-features skill)
	GenPhaseKnowledge           // Phase 4: Sonnet knowledge synthesis (spec-to-knowledge skill)
	GenPhaseCritic              // Phase 5: Critic validation
	GenPhaseFixLoop             // Phase 6: Auto-fix loop
)

type retryDelayMsg struct{}

type criticStreamMsg struct {
	line string
}

type criticLoopMsg struct {
	report   *CriticReport
	passed   bool
	advisory []CriticFinding
	blocking []CriticFinding
	err      error
	attempt  int
}

type criticFixDoneMsg struct {
	err error
}

type advisoryFixDoneMsg struct {
	err error
}
