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
)

var TabOrder = []Tab{TabOverview, TabKanban, TabLog, TabDiagram}

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
	ValidationRefs []string `json:"validation_refs"`
	Fixes          string   `json:"fixes,omitempty"`
	Addresses      []string `json:"addresses,omitempty"`
	Resolution     string   `json:"resolution,omitempty"`
	ResolvedBy     string   `json:"resolved_by,omitempty"`
	ResolvedAt     string   `json:"resolved_at,omitempty"`
	Tainted        bool     `json:"tainted,omitempty"`
}

type Assertion struct {
	Category string   `json:"category"`
	Items    []string `json:"items"`
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

type MissionStats struct {
	Total              int
	Done               int
	DoneDirect         int
	DoneViaFix         int
	InProgress         int
	Blocked            int
	BlockedUnresolved  int
	BlockedResolved    int
	BlockedTainted     int
	Pending            int
	AwaitingValidation int
	Validating         int
	Refining           int
}

type MissionState struct {
	Exists   bool
	Project  string
	Owner    string
	Features []Feature
	Stats    MissionStats
	Path     string
}

type ClaudeStreamMsg struct {
	Line      string
	Done      bool
	Result    string
	Err       error
	SessionID string
}

type WorkerEvent struct {
	FeatureID    string
	Line         string
	Done         bool
	Result       string
	Err          error
	AllDone      bool
	Phase        int
	Role         string // "", "critic", "validator", "refinement"
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
