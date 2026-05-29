package internal

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

const margin = 2

const (
	changesPhaseSoftBudget        = 3*time.Minute + 30*time.Second
	changesPhaseHardBudget        = 5 * time.Minute
	changesPhaseTimeoutMaxRetries = 1
)

func sendNotification(title, message string) tea.Cmd {
	return func() tea.Msg {
		exec.Command("osascript", "-e",
			fmt.Sprintf(`display notification %q with title %q sound name "Glass"`, message, title),
		).Run()
		return nil
	}
}

type Model struct {
	width, height int
	phase         Phase
	projectDir    string
	missionDir    string
	forceSetup    bool
	ready         bool

	specs              []SpecEntry
	specCursor         int
	activeSpec         *SpecEntry
	editingSpec        bool
	genPhase           GenPhase
	fullRegenMode      bool
	regenMode          bool
	changesMode        bool
	changesScopeJSON   string
	changesPromptTight bool
	changesSoftWarned  bool
	changesTimeoutSent bool
	changesTimeouts    int
	genStartTime       time.Time
	pendingFeatures    []Feature
	knowledgeResult    *string
	verbose            bool
	confirmRegen       bool // G: full regenerate
	confirmDelta       bool // U: delta update
	confirmChanges     bool // C: changes intelligent
	confirmSkipCritic  bool // Shift+S: start all skipping critic gate
	confirmFullReset   int
	lastPrompt         string
	claudeExtraArgs    []string
	claudeRetries      int
	claudeSessionID    string
	claudeResumeHint   string

	// v2 spec-to-quest split-call state.
	assertionIDs    map[string][]string // produced by GenPhaseAssertions, consumed by GenPhaseFeatures
	coverageRetries int                 // count of retry-with-feedback attempts in the current phase

	input    textarea.Model
	viewport viewport.Model
	spinner  spinner.Model

	discoveryMsgs []ChatMessage
	streamLines   []string

	claudeCh        chan ClaudeStreamMsg
	claudeCmd       *exec.Cmd
	claudeRunning   bool
	claudeStartTime time.Time

	reviewInput textarea.Model
	reviewChat  []ChatMessage
	reviewTab   ReviewTab
	isRefining  bool

	mission   MissionState
	activeTab Tab

	workerPool       *WorkerPool
	logger           *MissionLogger
	executing        bool
	workerLogs       []string
	logFilter        int // -1 = all, 0+ = index into mission.Features
	featureCursor    int
	criticFailReport *CriticReport
	criticMenuCursor int
	autoFixRunning   bool
	autoFixCh        chan autoFixEventMsg

	criticAdvisory  []CriticFinding
	criticBlocking  []CriticFinding
	criticSelected  []bool
	criticCursor    int
	criticLoopCount int
	criticPassed    bool
	criticBypassed  bool // user explicitly chose to start without critic gate
	criticLoopCh    chan criticLoopMsg
	criticStreamCh  chan criticStreamMsg

	// Cost/token tracking. costTracker is bound to a mission once its
	// missionDir exists; records produced before that (first-time discovery /
	// plan generation) are buffered in pendingCostRecords and flushed on bind.
	costTracker        *CostTracker
	pendingCostRecords []CostRecord

	styles Styles
}

// Messages
type specsScannedMsg struct {
	specs []SpecEntry
}
type editorFinishedMsg struct {
	err  error
	path string
}
type contextReadyMsg struct {
	ch        chan ClaudeStreamMsg
	cmd       *exec.Cmd
	prompt    string
	extraArgs []string
}

func newTextArea(placeholder string, height int) textarea.Model {
	ta := textarea.New()
	ta.Placeholder = placeholder
	ta.CharLimit = 0
	ta.SetWidth(80)
	ta.SetHeight(height)
	ta.ShowLineNumbers = false
	ta.KeyMap.InsertNewline = key.NewBinding(key.WithKeys("alt+enter"))

	prompt := lipgloss.NewStyle().Foreground(lipgloss.Color("226"))
	text := lipgloss.NewStyle().Foreground(lipgloss.Color("252"))
	placeholder_ := lipgloss.NewStyle().Foreground(lipgloss.Color("243"))
	cursor := lipgloss.NewStyle().Foreground(lipgloss.Color("226"))

	ta.FocusedStyle.Prompt = prompt
	ta.FocusedStyle.Text = text
	ta.FocusedStyle.Placeholder = placeholder_
	ta.FocusedStyle.CursorLine = cursor
	ta.FocusedStyle.Base = lipgloss.NewStyle().
		BorderLeft(true).
		BorderStyle(lipgloss.ThickBorder()).
		BorderForeground(lipgloss.Color("226")).
		PaddingLeft(1)

	ta.BlurredStyle.Prompt = prompt.Foreground(lipgloss.Color("240"))
	ta.BlurredStyle.Text = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	ta.BlurredStyle.Placeholder = placeholder_
	ta.BlurredStyle.Base = lipgloss.NewStyle().
		BorderLeft(true).
		BorderStyle(lipgloss.ThickBorder()).
		BorderForeground(lipgloss.Color("240")).
		PaddingLeft(1)

	ta.Prompt = "❯ "
	ta.Focus()
	return ta
}

func NewModel(dir string, forceSetup bool, specSlug string) Model {
	ti := newTextArea("Chat with Claude about this project...", 3)
	ri := newTextArea("Type feedback or Enter to approve...", 2)

	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("226"))

	m := Model{
		phase:       PhaseSpecSelect,
		projectDir:  dir,
		forceSetup:  forceSetup,
		input:       ti,
		reviewInput: ri,
		spinner:     sp,
		logFilter:   -1,
		verbose:     true,
		styles:      NewStyles(),
	}

	if specSlug != "" {
		specDir := filepath.Join(dir, "docs", "specs", specSlug)
		missionDir := ResolveArtifactDir(specDir)
		state := ReadMissionState(missionDir)
		if state.Exists {
			m.activeSpec = &SpecEntry{Slug: specSlug, SpecPath: specDir, Mission: state}
			m.missionDir = missionDir
			m.mission = state
			m.phase = PhaseDashboard
		}
	}

	return m
}

func (m Model) Init() tea.Cmd {
	if m.phase == PhaseDashboard {
		return m.spinner.Tick
	}
	return tea.Batch(
		m.spinner.Tick,
		scanSpecsCmd(m.projectDir),
	)
}

func scanSpecsCmd(projectDir string) tea.Cmd {
	return func() tea.Msg {
		specs := ScanSpecs(projectDir)
		return specsScannedMsg{specs: specs}
	}
}

func listenClaude(ch chan ClaudeStreamMsg) tea.Cmd {
	return func() tea.Msg {
		msg, ok := <-ch
		if !ok {
			return ClaudeStreamMsg{Done: true}
		}
		return msg
	}
}

func listenAutoFix(ch chan autoFixEventMsg) tea.Cmd {
	return func() tea.Msg {
		msg, ok := <-ch
		if !ok {
			return autoFixEventMsg{done: true}
		}
		return msg
	}
}

func listenCriticStream(ch chan criticStreamMsg) tea.Cmd {
	return func() tea.Msg {
		msg, ok := <-ch
		if !ok {
			return criticStreamMsg{}
		}
		return msg
	}
}

func listenCriticFixDone(ch chan criticFixDoneMsg) tea.Cmd {
	return func() tea.Msg {
		msg, ok := <-ch
		if !ok {
			return criticFixDoneMsg{err: fmt.Errorf("fix channel closed")}
		}
		return msg
	}
}

func listenCriticLoop(ch chan criticLoopMsg) tea.Cmd {
	return func() tea.Msg {
		msg, ok := <-ch
		if !ok {
			return criticLoopMsg{err: fmt.Errorf("critic channel closed")}
		}
		return msg
	}
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		inputW := m.width - margin*2 - 4
		m.input.SetWidth(inputW)
		m.reviewInput.SetWidth(inputW)
		m.resizeInput()
		if m.isChatPhase() {
			m.viewport.SetContent(m.renderChatContent())
		} else if m.phase == PhaseReview {
			m.updateReviewContent()
		} else if m.phase == PhaseDashboard {
			m.updateDashboardContent()
		}
		if !m.ready {
			m.ready = true
		}
		return m, nil

	case specsScannedMsg:
		m.specs = msg.specs
		if m.forceSetup || len(msg.specs) == 0 {
			m.phase = PhaseDiscovery
			m.input.Focus()
		} else {
			m.phase = PhaseSpecSelect
		}
		return m, nil

	case spinner.TickMsg:
		if m.claudeRunning || m.executing || m.autoFixRunning {
			if m.inChangesTimedPhase() && !m.claudeStartTime.IsZero() {
				elapsed := time.Since(m.claudeStartTime)
				phaseIdx := m.changesPhaseIndex()
				if elapsed >= changesPhaseSoftBudget && !m.changesSoftWarned {
					m.changesSoftWarned = true
					m.discoveryMsgs = append(m.discoveryMsgs, ChatMessage{
						Role: "system",
						Text: fmt.Sprintf("PHASE_WARN_CHANGES:%d:%s:approaching 5m budget", phaseIdx, elapsed.Round(time.Second)),
					})
					m.viewport.SetContent(m.renderChatContent())
					m.viewport.GotoBottom()
				}
				if elapsed >= changesPhaseHardBudget && !m.changesTimeoutSent {
					m.changesTimeoutSent = true
					m.discoveryMsgs = append(m.discoveryMsgs, ChatMessage{
						Role: "system",
						Text: fmt.Sprintf("PHASE_TIMEOUT_CHANGES:%d:%s:5m hard budget reached", phaseIdx, elapsed.Round(time.Second)),
					})
					StopClaude(m.claudeCmd)
					m.viewport.SetContent(m.renderChatContent())
					m.viewport.GotoBottom()
				}
			}
			var cmd tea.Cmd
			m.spinner, cmd = m.spinner.Update(msg)
			cmds = append(cmds, cmd)
			if m.claudeRunning && m.isChatPhase() {
				if len(m.streamLines) > 0 {
					m.viewport.SetContent(m.renderChatContent())
				}
			}
		}
		return m, tea.Batch(cmds...)

	case ClaudeStreamMsg:
		return m.handleClaudeStream(msg)

	case retryDelayMsg:
		return m.retryClaude()

	case WorkerEvent:
		return m.handleWorkerEvent(msg)

	case autoFixEventMsg:
		if !m.autoFixRunning {
			return m, nil
		}
		if msg.done {
			m.autoFixRunning = false
			if msg.err != nil {
				m.workerLogs = append(m.workerLogs, fmt.Sprintf("[AUTOFIX] ✕ Error: %v", msg.err))
				m.criticFailReport = nil
				m.updateDashboardContent()
				return m, sendNotification("Quest", "Auto-fix failed")
			}
			m.workerLogs = append(m.workerLogs, "[AUTOFIX] ✓ Fixes applied — restarting quest...")
			m.criticFailReport = nil
			m.mission = ReadMissionState(m.missionDir)
			m.updateDashboardContent()
			return m.startWorkers()
		}
		if msg.line != "" {
			m.workerLogs = append(m.workerLogs, fmt.Sprintf("[AUTOFIX] %s", msg.line))
			if len(m.workerLogs) > 10000 {
				m.workerLogs = m.workerLogs[len(m.workerLogs)-10000:]
			}
			wasAtTop := m.viewport.AtTop()
			m.updateDashboardContent()
			if m.activeTab == TabLog && wasAtTop {
				m.viewport.GotoTop()
			}
		}
		return m, listenAutoFix(m.autoFixCh)

	case criticStreamMsg:
		if msg.line != "" {
			m.streamLines = append(m.streamLines, msg.line)
			m.viewport.SetContent(m.renderChatContent())
			m.viewport.GotoBottom()
		}
		if m.criticStreamCh != nil {
			return m, listenCriticStream(m.criticStreamCh)
		}
		return m, nil

	case criticLoopMsg:
		if !m.inCriticGenerationFlow() {
			return m, nil
		}
		return m.handleCriticLoopMsg(msg)

	case criticFixDoneMsg:
		if !m.inCriticGenerationFlow() {
			return m, nil
		}
		return m.handleCriticFixDone(msg)

	case advisoryFixDoneMsg:
		if msg.err != nil {
			m.workerLogs = append(m.workerLogs, fmt.Sprintf("[AUTOFIX] ✕ Advisory fix error: %v", msg.err))
		} else {
			m.workerLogs = append(m.workerLogs, "[AUTOFIX] ✓ Advisory fixes applied")
		}
		m.autoFixRunning = false
		return m.startWorkersAfterCritic()

	case editorFinishedMsg:
		if msg.err == nil && msg.path != "" {
			data, readErr := os.ReadFile(msg.path)
			if readErr == nil {
				text := strings.TrimRight(string(data), "\n")
				if m.phase == PhaseReview {
					m.reviewInput.SetValue(text)
					m.reviewInput.Focus()
				} else {
					m.input.SetValue(text)
					m.input.Focus()
				}
				m.resizeInput()
			}
			os.Remove(msg.path)
		}
		return m, nil

	case contextReadyMsg:
		if !m.claudeRunning || m.phase != PhaseRunning {
			StopClaude(msg.cmd)
			return m, nil
		}
		m.claudeCh = msg.ch
		m.claudeCmd = msg.cmd
		m.lastPrompt = msg.prompt
		m.claudeExtraArgs = msg.extraArgs
		m.claudeStartTime = time.Now()
		if m.changesMode {
			m.resetChangesPhaseBudgetTracking()
		}
		return m, listenClaude(msg.ch)

	case tea.KeyMsg:
		return m.handleKey(msg)

	case tea.MouseMsg:
		var cmd tea.Cmd
		m.viewport, cmd = m.viewport.Update(msg)
		return m, cmd
	}

	return m, tea.Batch(cmds...)
}

func (m Model) View() string {
	if !m.ready {
		return "Loading..."
	}

	switch m.phase {
	case PhaseSpecSelect:
		return m.specSelectView()
	case PhaseDiscovery, PhaseRunning:
		return m.chatView()
	case PhaseReview:
		return m.reviewView()
	case PhaseDashboard:
		return m.dashboardView()
	}
	return ""
}

// --- Phase helpers ---

func (m Model) isChatPhase() bool {
	return m.phase == PhaseDiscovery || m.phase == PhaseRunning
}

func (m Model) inChangesTimedPhase() bool {
	if !m.changesMode || m.phase != PhaseRunning || !m.claudeRunning {
		return false
	}
	switch m.genPhase {
	case GenPhaseAnalysis, GenPhaseAssertions, GenPhaseFeatures:
		return true
	default:
		return false
	}
}

func (m Model) changesPhaseIndex() int {
	switch m.genPhase {
	case GenPhaseAnalysis:
		return 1
	case GenPhaseAssertions:
		return 2
	case GenPhaseFeatures:
		return 3
	case GenPhaseCritic:
		return 4
	case GenPhaseFixLoop:
		return 5
	default:
		return 0
	}
}

func (m *Model) resetChangesPhaseBudgetTracking() {
	m.changesSoftWarned = false
	m.changesTimeoutSent = false
}

func (m *Model) ensureCriticSelectionState() {
	if len(m.criticAdvisory) == 0 {
		m.criticSelected = nil
		m.criticCursor = 0
		return
	}
	if len(m.criticSelected) != len(m.criticAdvisory) {
		resized := make([]bool, len(m.criticAdvisory))
		copy(resized, m.criticSelected)
		m.criticSelected = resized
	}
	if m.criticCursor < 0 {
		m.criticCursor = 0
	}
	if m.criticCursor >= len(m.criticAdvisory) {
		m.criticCursor = len(m.criticAdvisory) - 1
	}
}

func (m *Model) updateViewportSize() {
	var fixedH int
	if m.phase == PhaseDashboard {
		fixedH = 7
	} else {
		fixedH = 7 + m.currentInputHeight()
	}
	m.viewport.Width = max(20, m.width-margin*2)
	m.viewport.Height = max(4, m.height-fixedH)
}

func isNewlineKey(msg tea.KeyMsg) bool {
	return msg.Type == tea.KeyCtrlJ || (msg.Type == tea.KeyEnter && msg.Alt)
}

func (m *Model) currentInputHeight() int {
	if m.phase == PhaseReview {
		return m.reviewInput.Height()
	}
	return m.input.Height()
}

func (m *Model) resizeInput() {
	ta := &m.input
	if m.phase == PhaseReview {
		ta = &m.reviewInput
	}
	lines := strings.Count(ta.Value(), "\n") + 1
	minH := 3
	maxH := m.height * 2 / 5
	if maxH < minH {
		maxH = minH
	}
	h := lines
	if h < minH {
		h = minH
	}
	if h > maxH {
		h = maxH
	}
	ta.SetHeight(h)
	m.updateViewportSize()
}

func openInEditor(content string) tea.Cmd {
	f, err := os.CreateTemp("", "mission-*.md")
	if err != nil {
		return func() tea.Msg {
			return editorFinishedMsg{err: err}
		}
	}
	f.WriteString(content)
	f.Close()

	editor := os.Getenv("EDITOR")
	if editor == "" {
		editor = "vim"
	}
	c := exec.Command(editor, f.Name())
	return tea.ExecProcess(c, func(err error) tea.Msg {
		return editorFinishedMsg{err: err, path: f.Name()}
	})
}

// --- Key handling ---

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch m.phase {
	case PhaseSpecSelect:
		return m.handleSpecSelectKey(msg)
	case PhaseDiscovery:
		return m.handleDiscoveryKey(msg)
	case PhaseRunning:
		return m.handleRunningKey(msg)
	case PhaseReview:
		return m.handleReviewKey(msg)
	case PhaseDashboard:
		return m.handleDashboardKey(msg)
	}
	return m, nil
}

func (m Model) handleSpecSelectKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	maxIdx := len(m.specs) // last index = "Create new"

	switch msg.String() {
	case "up", "k":
		if m.specCursor > 0 {
			m.specCursor--
		}
	case "down", "j":
		if m.specCursor < maxIdx {
			m.specCursor++
		}
	case "enter":
		if m.specCursor == maxIdx {
			m.editingSpec = false
			m.phase = PhaseDiscovery
			m.input.Focus()
			return m, nil
		}
		spec := m.specs[m.specCursor]
		m.activeSpec = &spec
		m.missionDir = ResolveArtifactDir(spec.SpecPath)
		m.mission = spec.Mission
		m.criticPassed = false
		m.criticBypassed = false
		m.criticLoopCount = 0
		m.criticBlocking = nil
		m.criticAdvisory = nil
		m.criticSelected = nil

		if !spec.Mission.Exists {
			return m.generateMissionFromSpec(spec)
		}

		m.phase = PhaseDashboard
		regenerateSpecIfTemplate(spec.SpecPath, m.missionDir)
		m.updateDashboardContent()
		return m, nil
	case "q", "esc":
		return m, tea.Quit
	}
	return m, nil
}

func (m Model) specSelectView() string {
	w := m.width - margin*2

	header := "\n" + m.styles.Title.Render("Quest Control") + "\n"
	sep := m.styles.Separator.Render(strings.Repeat("─", w))

	var sb strings.Builder
	if len(m.specs) == 0 {
		sb.WriteString("\n" + m.styles.Dim.Render("  No specs found. Press Enter to create one.") + "\n\n")
	} else {
		sb.WriteString("\n  " + m.styles.Cyan.Render("Select a spec:") + "\n\n")
		for i, spec := range m.specs {
			cursor := "  "
			if i == m.specCursor {
				cursor = m.styles.Title.Render("> ")
			}

			stats := spec.Mission.Stats
			total := stats.Total
			done := stats.Done
			pct := 0
			if total > 0 {
				pct = (done * 100) / total
			}

			barW := 12
			filled := (pct * barW) / 100
			bar := strings.Repeat("█", filled) + strings.Repeat("░", barW-filled)

			var statusColor lipgloss.Style
			if stats.InProgress > 0 {
				statusColor = m.styles.StatusWIP
			} else if done == total && total > 0 {
				statusColor = m.styles.StatusDone
			} else {
				statusColor = m.styles.StatusPend
			}

			title := spec.Title
			if len(title) > w-40 {
				title = title[:w-40] + "…"
			}

			var line string
			if !spec.Mission.Exists {
				line = fmt.Sprintf("%s%s  %s  %s",
					cursor,
					m.styles.Cyan.Render(spec.Slug),
					m.styles.Dim.Render(title),
					m.styles.Yellow.Render("needs planning"),
				)
			} else {
				line = fmt.Sprintf("%s%s  %s  %s %d%%  %s",
					cursor,
					m.styles.Cyan.Render(spec.Slug),
					m.styles.Dim.Render(title),
					statusColor.Render(bar),
					pct,
					m.styles.Dim.Render(fmt.Sprintf("(%d/%d)", done, total)),
				)
			}
			sb.WriteString("  " + line + "\n")
		}

		sb.WriteString("\n")
	}

	// "Create new" option
	cursor := "  "
	if m.specCursor == len(m.specs) {
		cursor = m.styles.Title.Render("> ")
	}
	sb.WriteString(fmt.Sprintf("  %s%s\n", cursor, m.styles.Green.Render("+ Create new spec")))
	sb.WriteString("\n")

	hint := m.styles.Hint.Render("  ↑↓: navigate · Enter: select · q: quit")

	pad := lipgloss.NewStyle().PaddingLeft(margin).PaddingRight(margin)
	return pad.Render(lipgloss.JoinVertical(lipgloss.Left,
		header, sep, sb.String(), sep, hint,
	))
}

func (m Model) handleDiscoveryKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.claudeRunning {
		switch msg.String() {
		case "v":
			m.verbose = !m.verbose
			return m, nil
		}
		switch msg.Type {
		case tea.KeyEsc:
			StopClaude(m.claudeCmd)
			m.claudeRunning = false
			m.streamLines = nil
			m.input.Focus()
			return m, nil
		}
		var cmd tea.Cmd
		m.viewport, cmd = m.viewport.Update(msg)
		return m, cmd
	}

	if isNewlineKey(msg) {
		m.input.InsertString("\n")
		m.resizeInput()
		return m, nil
	}

	hasMessages := len(m.discoveryMsgs) > 0

	switch msg.Type {
	case tea.KeyEsc:
		if m.input.Value() != "" {
			m.input.Reset()
			m.resizeInput()
		} else if hasMessages {
			m.discoveryMsgs = nil
			m.streamLines = nil
			m.viewport.SetContent(m.renderChatContent())
		} else {
			return m, tea.Quit
		}
		return m, nil
	case tea.KeyCtrlE:
		return m, openInEditor(m.input.Value())
	case tea.KeyEnter:
		text := strings.TrimSpace(m.input.Value())
		if text == "" {
			if hasMessages {
				return m.approveRequirements()
			}
			return m, nil
		}
		if !hasMessages {
			return m.startDiscovery()
		}
		return m.sendDiscoveryFeedback(text)
	}

	prevOffset := m.viewport.YOffset
	var vpCmd tea.Cmd
	m.viewport, vpCmd = m.viewport.Update(msg)
	if m.viewport.YOffset != prevOffset {
		return m, vpCmd
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	m.resizeInput()
	return m, cmd
}

func (m Model) handleRunningKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if !m.claudeRunning {
		switch msg.String() {
		case "r":
			m.claudeRetries = 0
			m.discoveryMsgs = append(m.discoveryMsgs, ChatMessage{Role: "system", Text: "Retrying..."})
			return m.retryClaude()
		case "v":
			m.verbose = !m.verbose
			return m, nil
		}
		switch msg.Type {
		case tea.KeyEsc:
			m.discoveryMsgs = nil
			m.streamLines = nil
			m.editingSpec = false
			m.claudeSessionID = ""
			m.claudeResumeHint = ""
			m.phase = PhaseSpecSelect
			return m, nil
		}
		var cmd tea.Cmd
		m.viewport, cmd = m.viewport.Update(msg)
		return m, cmd
	}

	switch msg.String() {
	case "v":
		m.verbose = !m.verbose
		return m, nil
	}
	switch msg.Type {
	case tea.KeyEsc:
		StopClaude(m.claudeCmd)
		m.claudeRunning = false
		m.genPhase = GenPhaseNone
		m.streamLines = nil
		m.discoveryMsgs = nil
		m.editingSpec = false
		m.phase = PhaseDiscovery
		m.input.Focus()
		return m, nil
	}
	var cmd tea.Cmd
	m.viewport, cmd = m.viewport.Update(msg)
	return m, cmd
}

func (m Model) handleReviewKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.isRefining {
		if msg.Type == tea.KeyEsc {
			StopClaude(m.claudeCmd)
			m.isRefining = false
			m.claudeRunning = false
			m.reviewChat = append(m.reviewChat, ChatMessage{Role: "system", Text: "Cancelled"})
		}
		return m, nil
	}

	if isNewlineKey(msg) {
		m.reviewInput.InsertString("\n")
		m.resizeInput()
		return m, nil
	}

	switch msg.Type {
	case tea.KeyTab:
		idx := 0
		for i, t := range ReviewTabOrder {
			if t == m.reviewTab {
				idx = i
				break
			}
		}
		m.reviewTab = ReviewTabOrder[(idx+1)%len(ReviewTabOrder)]
		m.updateReviewContent()
		return m, nil
	case tea.KeyCtrlE:
		return m, openInEditor(m.reviewInput.Value())
	case tea.KeyEnter:
		text := strings.TrimSpace(m.reviewInput.Value())
		if text == "" {
			return m.approvePlan()
		}
		return m.refinePlan(text)
	case tea.KeyEsc:
		if m.reviewInput.Value() != "" {
			m.reviewInput.Reset()
			m.resizeInput()
		} else {
			return m.rejectPlan()
		}
		return m, nil
	}

	if m.reviewTab == ReviewTabCritic && len(m.criticAdvisory) > 0 {
		m.ensureCriticSelectionState()
		switch msg.String() {
		case "up", "k":
			if m.criticCursor > 0 {
				m.criticCursor--
				m.updateReviewContent()
			}
			return m, nil
		case "down", "j":
			if m.criticCursor < len(m.criticAdvisory)-1 {
				m.criticCursor++
				m.updateReviewContent()
			}
			return m, nil
		case " ":
			if m.criticCursor >= 0 && m.criticCursor < len(m.criticSelected) {
				m.criticSelected[m.criticCursor] = !m.criticSelected[m.criticCursor]
			}
			m.updateReviewContent()
			return m, nil
		}
	}

	prevOffset := m.viewport.YOffset
	var vpCmd tea.Cmd
	m.viewport, vpCmd = m.viewport.Update(msg)
	if m.viewport.YOffset != prevOffset {
		return m, vpCmd
	}

	var cmd tea.Cmd
	m.reviewInput, cmd = m.reviewInput.Update(msg)
	m.resizeInput()
	return m, cmd
}

func (m Model) handleDashboardKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.confirmRegen {
		switch msg.String() {
		case "y", "Y":
			m.confirmRegen = false
			return m.fullRegenMissionPlan()
		default:
			m.confirmRegen = false
			m.updateDashboardContent()
			return m, nil
		}
	}
	if m.confirmDelta {
		switch msg.String() {
		case "y", "Y":
			m.confirmDelta = false
			return m.regenMissionPlan()
		default:
			m.confirmDelta = false
			m.updateDashboardContent()
			return m, nil
		}
	}
	if m.confirmChanges {
		switch msg.String() {
		case "y", "Y":
			m.confirmChanges = false
			return m.changesMissionPlan()
		default:
			m.confirmChanges = false
			m.updateDashboardContent()
			return m, nil
		}
	}
	if m.confirmSkipCritic {
		switch msg.String() {
		case "y", "Y":
			m.confirmSkipCritic = false
			return m.startWorkersSkipCritic()
		default:
			m.confirmSkipCritic = false
			m.updateDashboardContent()
			return m, nil
		}
	}

	if m.confirmFullReset > 0 {
		switch m.confirmFullReset {
		case 1:
			if msg.String() == "y" || msg.String() == "Y" {
				m.confirmFullReset = 2
			} else {
				m.confirmFullReset = 0
			}
			m.updateDashboardContent()
			return m, nil
		case 2:
			if msg.String() == "y" || msg.String() == "Y" {
				m.confirmFullReset = 0
				return m.fullResetAndStart()
			}
			m.confirmFullReset = 0
			m.updateDashboardContent()
			return m, nil
		}
	}

	if m.criticFailReport != nil && !m.autoFixRunning {
		switch msg.String() {
		case "up", "k":
			if m.criticMenuCursor > 0 {
				m.criticMenuCursor--
			}
		case "down", "j":
			if m.criticMenuCursor < 2 {
				m.criticMenuCursor++
			}
		case "enter":
			switch m.criticMenuCursor {
			case 0:
				return m.startAutoFix()
			case 1:
				m.criticFailReport = nil
				return m.startWorkersSkipCritic()
			case 2:
				m.criticFailReport = nil
			}
		case "esc":
			m.criticFailReport = nil
		}
		m.updateDashboardContent()
		return m, nil
	}

	if m.autoFixRunning {
		switch msg.String() {
		case "esc":
			StopClaude(m.claudeCmd)
			m.autoFixRunning = false
			m.criticFailReport = nil
			m.workerLogs = append(m.workerLogs, "[AUTOFIX] ✕ Cancelled by user")
			m.updateDashboardContent()
			return m, nil
		}
		return m, nil
	}

	switch msg.String() {
	case "q":
		if m.executing {
			m.workerPool.Stop()
			m.executing = false
			if m.logger != nil {
				m.logger.Close()
			}
		}
		return m, tea.Quit
	case "esc":
		if m.executing {
			m.workerPool.Stop()
			m.executing = false
			m.mission = ReadMissionState(m.missionDir)
			if m.logger != nil {
				m.logger.Close()
			}
			m.updateDashboardContent()
			return m, nil
		}
		return m, tea.Quit
	case "s":
		if !m.executing {
			return m.startWorkers()
		}
	case "S":
		if !m.executing && m.mission.Stats.Pending > 0 {
			m.confirmRegen = false
			m.confirmDelta = false
			m.confirmChanges = false
			m.confirmSkipCritic = true
			m.updateDashboardContent()
			return m, nil
		}
	case "e":
		if !m.executing && m.activeSpec != nil {
			m.editingSpec = true
			m.phase = PhaseDiscovery
			m.discoveryMsgs = nil
			m.streamLines = nil
			m.input.Focus()
			return m, nil
		}
	case "n":
		if !m.executing {
			m.editingSpec = false
			m.specs = ScanSpecs(m.projectDir)
			m.specCursor = 0
			m.activeSpec = nil
			m.phase = PhaseSpecSelect
			m.discoveryMsgs = nil
			m.workerLogs = nil
			return m, nil
		}
	case "r":
		if !m.executing {
			n := m.resetFeatures(false)
			if n > 0 {
				m.workerLogs = append(m.workerLogs, fmt.Sprintf("[ORCH] Reset %d stuck features to pending", n))
			}
			m.updateDashboardContent()
			return m, nil
		}
	case "R":
		if !m.executing {
			m.confirmSkipCritic = false
			m.confirmFullReset = 1
			m.updateDashboardContent()
			return m, nil
		}
	case "G":
		if !m.executing && m.activeSpec != nil {
			m.confirmDelta = false
			m.confirmChanges = false
			m.confirmSkipCritic = false
			m.confirmRegen = true
			m.updateDashboardContent()
			return m, nil
		}
	case "U":
		if !m.executing && m.activeSpec != nil {
			m.confirmRegen = false
			m.confirmChanges = false
			m.confirmSkipCritic = false
			m.confirmDelta = true
			m.updateDashboardContent()
			return m, nil
		}
	case "C":
		if !m.executing && m.activeSpec != nil {
			m.confirmRegen = false
			m.confirmDelta = false
			m.confirmSkipCritic = false
			m.confirmChanges = true
			m.updateDashboardContent()
			return m, nil
		}
	case "enter":
		if !m.executing && len(m.mission.Features) > 0 && (m.activeTab == TabOverview || m.activeTab == TabKanban) {
			if m.featureCursor >= 0 && m.featureCursor < len(m.mission.Features) {
				return m.retryFeature(m.featureCursor)
			}
		}
	case "up":
		if (m.activeTab == TabOverview || m.activeTab == TabKanban) && len(m.mission.Features) > 0 {
			if m.featureCursor > 0 {
				m.featureCursor--
			}
			m.updateDashboardContent()
			return m, nil
		}
	case "down":
		if (m.activeTab == TabOverview || m.activeTab == TabKanban) && len(m.mission.Features) > 0 {
			if m.featureCursor < len(m.mission.Features)-1 {
				m.featureCursor++
			}
			m.updateDashboardContent()
			return m, nil
		}
	case "v":
		m.verbose = !m.verbose
		m.updateDashboardContent()
		return m, nil
	case "f":
		m.activeTab = TabOverview
		m.updateDashboardContent()
	case "k":
		m.activeTab = TabKanban
		m.updateDashboardContent()
	case "l":
		m.activeTab = TabLog
		m.logFilter = -1
		m.updateDashboardContent()
		m.viewport.GotoTop()
	case "d":
		m.activeTab = TabDiagram
		m.updateDashboardContent()
	case "c":
		m.activeTab = TabCost
		m.updateDashboardContent()
		m.viewport.GotoTop()
	case "left":
		if m.activeTab == TabLog && len(m.mission.Features) > 0 {
			m.logFilter--
			if m.logFilter < -1 {
				m.logFilter = len(m.mission.Features) - 1
			}
			m.updateDashboardContent()
			m.viewport.GotoTop()
			return m, nil
		}
	case "right":
		if m.activeTab == TabLog && len(m.mission.Features) > 0 {
			m.logFilter++
			if m.logFilter >= len(m.mission.Features) {
				m.logFilter = -1
			}
			m.updateDashboardContent()
			m.viewport.GotoTop()
			return m, nil
		}
	case "tab":
		idx := 0
		for i, t := range TabOrder {
			if t == m.activeTab {
				idx = i
				break
			}
		}
		m.activeTab = TabOrder[(idx+1)%len(TabOrder)]
		m.updateDashboardContent()
	}

	var cmd tea.Cmd
	m.viewport, cmd = m.viewport.Update(msg)
	return m, cmd
}

// --- Actions ---

func (m Model) generateMissionFromSpec(spec SpecEntry) (tea.Model, tea.Cmd) {
	m.phase = PhaseRunning
	m.claudeRunning = true
	now := time.Now()
	m.claudeStartTime = now
	m.genStartTime = now
	m.editingSpec = true
	m.genPhase = GenPhaseAnalysis
	m.fullRegenMode = false
	m.regenMode = false
	m.changesMode = false
	m.changesScopeJSON = ""
	m.pendingFeatures = nil
	m.knowledgeResult = nil
	m.criticPassed = false
	m.criticBypassed = false
	m.criticLoopCount = 0

	hasExistingAnalysis := fileExists(filepath.Join(ResolveArtifactDir(spec.SpecPath), "codebase-analysis.md"))

	m.discoveryMsgs = []ChatMessage{
		{Role: "system", Text: fmt.Sprintf("Preparing quest plan for %s", spec.Slug)},
		{Role: "system", Text: "This will analyze your codebase and generate the execution plan."},
		{Role: "system", Text: "Estimated time: 5-10 minutes across 6 phases."},
		{Role: "system", Text: ""},
		{Role: "system", Text: "PHASE_ROADMAP"},
		{Role: "system", Text: ""},
		{Role: "system", Text: "PHASE_START:1"},
	}
	m.streamLines = nil
	m.claudeRetries = 0
	m.claudeSessionID = ""
	m.claudeResumeHint = ""

	m.viewport.SetContent(m.renderChatContent())
	m.viewport.GotoBottom()

	specPath := spec.SpecPath
	projectDir := m.projectDir
	verboseVal := m.verbose

	return m, tea.Batch(
		m.spinner.Tick,
		func() tea.Msg {
			prompt := BuildAnalysisPrompt(specPath, projectDir, hasExistingAnalysis)
			ch := make(chan ClaudeStreamMsg, 64)
			v := verboseVal
			maxTurns := "10"
			if hasExistingAnalysis {
				maxTurns = "5"
			}
			args := []string{"--model", "claude-sonnet-4-6", "--max-turns", maxTurns}
			cmd := StartClaude(prompt, projectDir, &v, ch, args...)
			return contextReadyMsg{ch: ch, cmd: cmd, prompt: prompt, extraArgs: args}
		},
	)
}

func (m Model) genPhaseLabel() string {
	switch m.genPhase {
	case GenPhaseAnalysis:
		if m.changesMode {
			return "sonnet · phase 1/5"
		}
		return "sonnet · phase 1/6"
	case GenPhaseAssertions:
		if m.changesMode {
			return "opus · phase 2/5"
		}
		if m.regenMode {
			return "opus · phase 1/4"
		}
		return "opus · phase 2/6"
	case GenPhaseFeatures:
		if m.changesMode {
			return "opus · phase 3/5"
		}
		if m.regenMode {
			return "opus · phase 2/4"
		}
		return "opus · phase 3/6"
	case GenPhaseKnowledge:
		return "sonnet · phase 4/6"
	case GenPhaseCritic:
		if m.changesMode {
			return "sonnet · phase 4/5"
		}
		if m.regenMode {
			return "sonnet · phase 3/4"
		}
		if m.fullRegenMode {
			return "sonnet · phase 2/3"
		}
		return "sonnet · phase 5/6"
	case GenPhaseFixLoop:
		if m.changesMode {
			return "sonnet · phase 5/5"
		}
		if m.regenMode {
			return "sonnet · phase 4/4"
		}
		if m.fullRegenMode {
			return "sonnet · phase 3/3"
		}
		return "sonnet · phase 6/6"
	default:
		if m.fullRegenMode {
			return "opus · phase 1/3"
		}
		if m.regenMode {
			return "opus · phase 1/4"
		}
		if m.changesMode {
			return "sonnet · phase 1/5"
		}
		return "opus"
	}
}

func (m Model) retryGenPhase(reason string) (tea.Model, tea.Cmd) {
	const maxGenRetries = 5
	m.claudeRetries++
	if m.claudeRetries <= maxGenRetries && (m.lastPrompt != "" || m.claudeSessionID != "") {
		resumeLabel := ""
		if m.claudeSessionID != "" {
			resumeLabel = " (resuming session)"
		}
		m.claudeResumeHint = reason
		m.discoveryMsgs = append(m.discoveryMsgs, ChatMessage{Role: "system", Text: fmt.Sprintf("⚠ %s — retrying (%d/%d)%s...", reason, m.claudeRetries, maxGenRetries, resumeLabel)})
		m.claudeRunning = true
		return m.retryClaudeWithDelay()
	}
	m.genPhase = GenPhaseNone
	m.claudeRunning = false
	m.discoveryMsgs = append(m.discoveryMsgs, ChatMessage{Role: "system", Text: fmt.Sprintf("✕ %s — failed after %d retries", reason, m.claudeRetries)})
	m.viewport.SetContent(m.renderChatContent())
	m.viewport.GotoBottom()
	return m, sendNotification("Quest", fmt.Sprintf("Phase failed after %d retries: %s", m.claudeRetries, reason))
}

func (m Model) nextGenPhase(result string) (tea.Model, tea.Cmd) {
	specPath := ""
	if m.activeSpec != nil {
		specPath = m.activeSpec.SpecPath
	}
	if specPath == "" {
		m.discoveryMsgs = append(m.discoveryMsgs, ChatMessage{Role: "system", Text: "✕ No spec path available"})
		m.viewport.SetContent(m.renderChatContent())
		m.viewport.GotoBottom()
		return m, nil
	}

	mDir := ResolveArtifactDir(specPath)
	os.MkdirAll(mDir, 0o755)
	projectDir := m.projectDir
	verboseVal := m.verbose

	elapsed := time.Since(m.claudeStartTime).Round(time.Second)

	switch m.genPhase {
	case GenPhaseAnalysis:
		if m.changesMode {
			scope := strings.TrimSpace(result)
			if scope == "" {
				return m.retryGenPhase("Changes analysis returned empty result")
			}
			m.changesScopeJSON = scope
			m.discoveryMsgs = append(m.discoveryMsgs, ChatMessage{
				Role: "system",
				Text: fmt.Sprintf("PHASE_DONE_CHANGES:1:%s:semantic change analysis captured", elapsed),
			})

			m.genPhase = GenPhaseAssertions
			m.changesPromptTight = false
			m.changesTimeouts = 0
			m.resetChangesPhaseBudgetTracking()
			m.discoveryMsgs = append(m.discoveryMsgs, ChatMessage{Role: "system", Text: "PHASE_START_CHANGES:2"})
			m.streamLines = nil
			m.claudeRunning = true
			m.claudeStartTime = time.Now()
			m.claudeRetries = 0
			m.coverageRetries = 0
			m.claudeSessionID = ""
			m.claudeResumeHint = ""
			m.viewport.SetContent(m.renderChatContent())
			m.viewport.GotoBottom()

			return m, m.spawnChangesAssertionsCall(specPath, mDir, projectDir, verboseVal, "")
		}
		ap := filepath.Join(mDir, "codebase-analysis.md")
		existing := readFileContent(ap)
		if result != "" {
			result = cleanAnalysisOutput(result)
		}
		if result == "" && existing == "" {
			return m.retryGenPhase("Analysis returned empty result")
		}
		detail := ""
		if result != "" && len(result) >= len(existing) {
			os.WriteFile(ap, []byte(result), 0o644)
			detail = fmt.Sprintf("saved (%d chars)", len(result))
		} else if existing != "" {
			detail = fmt.Sprintf("validated (%d chars)", len(existing))
		}
		m.discoveryMsgs = append(m.discoveryMsgs, ChatMessage{Role: "system", Text: fmt.Sprintf("PHASE_DONE:1:%s:%s", elapsed, detail)})

		m.genPhase = GenPhaseAssertions
		m.discoveryMsgs = append(m.discoveryMsgs, ChatMessage{Role: "system", Text: "PHASE_START:2"})
		m.streamLines = nil
		m.claudeRunning = true
		m.claudeStartTime = time.Now()
		m.claudeRetries = 0
		m.coverageRetries = 0
		m.claudeSessionID = ""
		m.claudeResumeHint = ""
		m.viewport.SetContent(m.renderChatContent())
		m.viewport.GotoBottom()

		return m, m.spawnAssertionsCall(specPath, projectDir, verboseVal, "")

	case GenPhaseAssertions:
		var (
			assertions     []Assertion
			ok             bool
			assertionDelta AssertionDelta
		)
		if m.changesMode {
			assertionDelta, ok = ParseAssertionDeltaJSON(result)
			if !ok {
				return m.retryGenPhase("Could not parse assertion delta output")
			}
			existingAssertions := parseAssertionsFromContract(filepath.Join(mDir, "validation-contract.md"))
			assertions = ApplyAssertionDelta(existingAssertions, assertionDelta)
			if len(assertions) == 0 {
				return m.retryGenPhase("Assertion delta merged into empty contract")
			}
		} else {
			assertions, ok = ParseAssertionsOnlyJSON(result)
			if !ok {
				return m.retryGenPhase("Could not parse assertions output")
			}
		}

		spec := readFileContent(filepath.Join(specPath, "spec.md"))
		issues := validateAssertionsCoverage(spec, assertions)

		const maxCoverageRetries = 2
		if len(issues) > 0 && m.coverageRetries < maxCoverageRetries {
			m.coverageRetries++
			feedback := formatCoverageIssues(issues)
			m.discoveryMsgs = append(m.discoveryMsgs, ChatMessage{
				Role: "system",
				Text: fmt.Sprintf("⚠ Assertions coverage gaps (%d) — retrying with feedback (%d/%d)", len(issues), m.coverageRetries, maxCoverageRetries),
			})
			m.streamLines = nil
			m.claudeRunning = true
			m.claudeStartTime = time.Now()
			m.viewport.SetContent(m.renderChatContent())
			m.viewport.GotoBottom()
			if m.changesMode {
				return m, m.spawnChangesAssertionsCall(specPath, mDir, projectDir, verboseVal, feedback)
			}
			if m.regenMode {
				return m, m.spawnRegenAssertionsCall(specPath, mDir, projectDir, verboseVal, feedback)
			}
			return m, m.spawnAssertionsCall(specPath, projectDir, verboseVal, feedback)
		}

		project := extractSpecTitle(specPath)
		if project == "" {
			project = filepath.Base(specPath)
		}
		totalAssertions := 0
		for _, a := range assertions {
			totalAssertions += len(a.Items)
		}
		WriteValidationContract(mDir, project, assertions)
		m.assertionIDs = CompactAssertionIDs(assertions)

		detailSuffix := ""
		if len(issues) > 0 {
			detailSuffix = fmt.Sprintf(" — %d coverage gap(s) accepted (critic phase will catch)", len(issues))
		}
		if m.changesMode {
			m.discoveryMsgs = append(m.discoveryMsgs, ChatMessage{
				Role: "system",
				Text: fmt.Sprintf("PHASE_DONE_CHANGES:2:%s:%d assertions across %d categories (%d upsert, %d remove)%s", elapsed, totalAssertions, len(assertions), len(assertionDelta.Upsert), len(assertionDelta.Remove), detailSuffix),
			})

			m.genPhase = GenPhaseFeatures
			m.changesPromptTight = false
			m.changesTimeouts = 0
			m.resetChangesPhaseBudgetTracking()
			m.discoveryMsgs = append(m.discoveryMsgs, ChatMessage{Role: "system", Text: "PHASE_START_CHANGES:3"})
			m.streamLines = nil
			m.claudeRunning = true
			m.claudeStartTime = time.Now()
			m.claudeRetries = 0
			m.coverageRetries = 0
			m.claudeSessionID = ""
			m.claudeResumeHint = ""
			m.viewport.SetContent(m.renderChatContent())
			m.viewport.GotoBottom()

			return m, m.spawnChangesFeaturesCall(specPath, mDir, projectDir, m.assertionIDs, verboseVal, "")
		}
		if m.regenMode {
			m.discoveryMsgs = append(m.discoveryMsgs, ChatMessage{
				Role: "system",
				Text: fmt.Sprintf("PHASE_DONE_REGEN:1:%s:%d assertions across %d categories%s", elapsed, totalAssertions, len(assertions), detailSuffix),
			})

			m.genPhase = GenPhaseFeatures
			m.discoveryMsgs = append(m.discoveryMsgs, ChatMessage{Role: "system", Text: "PHASE_START_REGEN:2"})
			m.streamLines = nil
			m.claudeRunning = true
			m.claudeStartTime = time.Now()
			m.claudeRetries = 0
			m.coverageRetries = 0
			m.claudeSessionID = ""
			m.claudeResumeHint = ""
			m.viewport.SetContent(m.renderChatContent())
			m.viewport.GotoBottom()

			return m, m.spawnRegenFeaturesCall(specPath, mDir, projectDir, m.assertionIDs, verboseVal, "")
		}
		m.discoveryMsgs = append(m.discoveryMsgs, ChatMessage{
			Role: "system",
			Text: fmt.Sprintf("PHASE_DONE:2:%s:%d assertions across %d categories%s", elapsed, totalAssertions, len(assertions), detailSuffix),
		})

		m.genPhase = GenPhaseFeatures
		m.discoveryMsgs = append(m.discoveryMsgs, ChatMessage{Role: "system", Text: "PHASE_START:3"})
		m.streamLines = nil
		m.claudeRunning = true
		m.claudeStartTime = time.Now()
		m.claudeRetries = 0
		m.coverageRetries = 0
		m.claudeSessionID = ""
		m.claudeResumeHint = ""
		m.viewport.SetContent(m.renderChatContent())
		m.viewport.GotoBottom()

		return m, m.spawnFeaturesCall(specPath, projectDir, verboseVal, m.assertionIDs, "")

	case GenPhaseFeatures:
		var (
			features     []Feature
			ok           bool
			featureDelta FeatureDelta
		)
		if m.changesMode {
			featureDelta, ok = ParseFeatureDeltaJSON(result)
			if !ok {
				return m.retryGenPhase("Could not parse feature delta output")
			}
			var manifest FeaturesManifest
			if data, err := os.ReadFile(filepath.Join(mDir, "features.json")); err == nil {
				_ = json.Unmarshal(data, &manifest)
			}
			features = ApplyFeatureDelta(manifest.Features, featureDelta, true)
			if len(features) == 0 {
				return m.retryGenPhase("Feature delta merged into empty feature set")
			}
		} else {
			features, ok = ParseFeaturesOnlyJSON(result)
			if !ok {
				return m.retryGenPhase("Could not parse features output")
			}
		}

		issues := validateFeaturesCoverage(features, m.assertionIDs)

		const maxCoverageRetries = 2
		if len(issues) > 0 && m.coverageRetries < maxCoverageRetries {
			m.coverageRetries++
			feedback := formatCoverageIssues(issues)
			m.discoveryMsgs = append(m.discoveryMsgs, ChatMessage{
				Role: "system",
				Text: fmt.Sprintf("⚠ Features coverage gaps (%d) — retrying with feedback (%d/%d)", len(issues), m.coverageRetries, maxCoverageRetries),
			})
			m.streamLines = nil
			m.claudeRunning = true
			m.claudeStartTime = time.Now()
			m.viewport.SetContent(m.renderChatContent())
			m.viewport.GotoBottom()
			if m.changesMode {
				return m, m.spawnChangesFeaturesCall(specPath, mDir, projectDir, m.assertionIDs, verboseVal, feedback)
			}
			if m.regenMode {
				return m, m.spawnRegenFeaturesCall(specPath, mDir, projectDir, m.assertionIDs, verboseVal, feedback)
			}
			return m, m.spawnFeaturesCall(specPath, projectDir, verboseVal, m.assertionIDs, feedback)
		}

		m.pendingFeatures = features

		detailSuffix := ""
		if len(issues) > 0 {
			detailSuffix = fmt.Sprintf(" — %d coverage gap(s) accepted", len(issues))
		}
		if m.changesMode {
			project := extractSpecTitle(specPath)
			if project == "" {
				project = filepath.Base(specPath)
			}
			slug := ""
			if m.activeSpec != nil {
				slug = m.activeSpec.Slug
			}
			if slug == "" {
				slug = filepath.Base(specPath)
			}

			assertions := parseAssertionsFromContract(filepath.Join(mDir, "validation-contract.md"))
			knowledgePath := filepath.Join(mDir, "knowledge-base.md")
			existingKnowledge := readFileContent(knowledgePath)

			plan := PlanData{
				Slug:       slug,
				Spec:       fmt.Sprintf("docs/specs/%s/spec.md", slug),
				Project:    project,
				Owner:      "",
				Features:   features,
				Assertions: assertions,
				Knowledge:  []string{},
			}
			_ = WriteMissionFiles(specPath, m.projectDir, plan)
			if existingKnowledge != "" {
				_ = os.WriteFile(knowledgePath, []byte(existingKnowledge), 0o644)
			}

			m.missionDir = mDir
			m.activeSpec = &SpecEntry{Slug: slug, SpecPath: specPath}
			m.mission = ReadMissionState(mDir)
			m.editingSpec = false

			totalAssertions := 0
			for _, a := range assertions {
				totalAssertions += len(a.Items)
			}
			m.discoveryMsgs = append(m.discoveryMsgs, ChatMessage{
				Role: "system",
				Text: fmt.Sprintf("PHASE_DONE_CHANGES:3:%s:%d features, %d assertions refreshed (%d upsert, %d remove)%s", elapsed, len(features), totalAssertions, len(featureDelta.Upsert), len(featureDelta.Remove), detailSuffix),
			})
			m.changesPromptTight = false
			m.changesTimeouts = 0
			m.resetChangesPhaseBudgetTracking()
			m.streamLines = nil
			m.claudeRetries = 0
			m.claudeSessionID = ""
			m.claudeResumeHint = ""
			m.viewport.SetContent(m.renderChatContent())
			m.viewport.GotoBottom()

			return m.startCriticLoop()
		}
		if m.regenMode {
			project := extractSpecTitle(specPath)
			if project == "" {
				project = filepath.Base(specPath)
			}
			slug := ""
			if m.activeSpec != nil {
				slug = m.activeSpec.Slug
			}
			if slug == "" {
				slug = filepath.Base(specPath)
			}

			assertions := parseAssertionsFromContract(filepath.Join(mDir, "validation-contract.md"))
			knowledgePath := filepath.Join(mDir, "knowledge-base.md")
			existingKnowledge := readFileContent(knowledgePath)

			plan := PlanData{
				Slug:       slug,
				Spec:       fmt.Sprintf("docs/specs/%s/spec.md", slug),
				Project:    project,
				Owner:      "",
				Features:   features,
				Assertions: assertions,
				Knowledge:  []string{},
			}
			_ = WriteMissionFiles(specPath, m.projectDir, plan)
			if existingKnowledge != "" {
				_ = os.WriteFile(knowledgePath, []byte(existingKnowledge), 0o644)
			}

			m.missionDir = mDir
			m.activeSpec = &SpecEntry{Slug: slug, SpecPath: specPath}
			m.mission = ReadMissionState(mDir)
			m.editingSpec = false

			totalAssertions := 0
			for _, a := range assertions {
				totalAssertions += len(a.Items)
			}
			m.discoveryMsgs = append(m.discoveryMsgs, ChatMessage{
				Role: "system",
				Text: fmt.Sprintf("PHASE_DONE_REGEN:2:%s:%d features, %d assertions refreshed%s", elapsed, len(features), totalAssertions, detailSuffix),
			})
			m.streamLines = nil
			m.claudeRetries = 0
			m.claudeSessionID = ""
			m.claudeResumeHint = ""
			m.viewport.SetContent(m.renderChatContent())
			m.viewport.GotoBottom()

			return m.startCriticLoop()
		}
		m.discoveryMsgs = append(m.discoveryMsgs, ChatMessage{
			Role: "system",
			Text: fmt.Sprintf("PHASE_DONE:3:%s:%d features%s", elapsed, len(features), detailSuffix),
		})

		m.genPhase = GenPhaseKnowledge
		m.discoveryMsgs = append(m.discoveryMsgs, ChatMessage{Role: "system", Text: "PHASE_START:4"})
		m.streamLines = nil
		m.claudeRunning = true
		m.claudeStartTime = time.Now()
		m.claudeRetries = 0
		m.coverageRetries = 0
		m.claudeSessionID = ""
		m.claudeResumeHint = ""
		m.viewport.SetContent(m.renderChatContent())
		m.viewport.GotoBottom()

		return m, m.spawnKnowledgeCall(specPath, projectDir, verboseVal, features, "")

	case GenPhaseKnowledge:
		knowledge := ParseKnowledgeJSON(result)
		// Knowledge is best-effort — sonnet generates 8-18 short bullets.
		// Empty array is acceptable (worker pipeline will tolerate).
		if knowledge == nil {
			knowledge = []string{}
		}

		knowledgeJSON, _ := json.Marshal(knowledge)
		kr := string(knowledgeJSON)
		m.knowledgeResult = &kr

		m.discoveryMsgs = append(m.discoveryMsgs, ChatMessage{
			Role: "system",
			Text: fmt.Sprintf("PHASE_DONE:4:%s:%d knowledge entries", elapsed, len(knowledge)),
		})
		return m.finalizeGeneration(specPath, mDir)
	}

	return m, nil
}

// spawnAssertionsCall returns a tea.Cmd that builds the v2 Call 1 prompt and
// starts the Claude subprocess. retryFeedback is empty on the first attempt
// and contains the previous coverage gaps on retries.
//
// Model is pinned to opus because the validation contract is the source of
// truth for the rest of the pipeline — quality here determines downstream
// quality.
func (m *Model) spawnAssertionsCall(specPath, projectDir string, verbose bool, retryFeedback string) tea.Cmd {
	return func() tea.Msg {
		prompt := BuildAssertionsPrompt(specPath, projectDir, retryFeedback)
		ch := make(chan ClaudeStreamMsg, 64)
		v := verbose
		args := []string{"--allowedTools", "Read", "--max-turns", "1", "--model", "opus"}
		cmd := StartClaude(prompt, projectDir, &v, ch, args...)
		return contextReadyMsg{ch: ch, cmd: cmd, prompt: prompt, extraArgs: args}
	}
}

// spawnRegenAssertionsCall starts regen phase 1: regenerate assertions only.
func (m *Model) spawnRegenAssertionsCall(specPath, missionDir, projectDir string, verbose bool, retryFeedback string) tea.Cmd {
	return func() tea.Msg {
		prompt := BuildRegenAssertionsPrompt(specPath, missionDir, projectDir, retryFeedback)
		ch := make(chan ClaudeStreamMsg, 64)
		v := verbose
		args := []string{"--allowedTools", "Read", "--max-turns", "1", "--model", "opus"}
		cmd := StartClaude(prompt, projectDir, &v, ch, args...)
		return contextReadyMsg{ch: ch, cmd: cmd, prompt: prompt, extraArgs: args}
	}
}

// spawnFeaturesCall returns a tea.Cmd that builds the v2 Call 2 prompt and
// starts the Claude subprocess. assertionIDs is the per-category map produced
// by Call 1; retryFeedback is empty on the first attempt.
//
// Model is pinned to opus because feature decomposition quality drives the
// whole worker pipeline — bad scope or missing validation_refs cascade.
func (m *Model) spawnFeaturesCall(specPath, projectDir string, verbose bool, assertionIDs map[string][]string, retryFeedback string) tea.Cmd {
	return func() tea.Msg {
		prompt := BuildFeaturesPrompt(specPath, projectDir, assertionIDs, retryFeedback)
		ch := make(chan ClaudeStreamMsg, 64)
		v := verbose
		args := []string{"--allowedTools", "Read", "--max-turns", "1", "--model", "opus"}
		cmd := StartClaude(prompt, projectDir, &v, ch, args...)
		return contextReadyMsg{ch: ch, cmd: cmd, prompt: prompt, extraArgs: args}
	}
}

// spawnRegenFeaturesCall starts regen phase 2: regenerate features only.
func (m *Model) spawnRegenFeaturesCall(specPath, missionDir, projectDir string, assertionIDs map[string][]string, verbose bool, retryFeedback string) tea.Cmd {
	return func() tea.Msg {
		prompt := BuildRegenFeaturesPrompt(specPath, missionDir, projectDir, assertionIDs, retryFeedback)
		ch := make(chan ClaudeStreamMsg, 64)
		v := verbose
		args := []string{"--allowedTools", "Read", "--max-turns", "1", "--model", "opus"}
		cmd := StartClaude(prompt, projectDir, &v, ch, args...)
		return contextReadyMsg{ch: ch, cmd: cmd, prompt: prompt, extraArgs: args}
	}
}

func (m *Model) spawnChangesAnalysisCall(specPath, missionDir, projectDir string, verbose bool) tea.Cmd {
	return func() tea.Msg {
		prompt := BuildChangesDiffPrompt(specPath, missionDir, projectDir, m.changesPromptTight)
		ch := make(chan ClaudeStreamMsg, 64)
		v := verbose
		args := []string{"--allowedTools", "Read", "--max-turns", "1", "--model", "claude-sonnet-4-6"}
		cmd := StartClaude(prompt, projectDir, &v, ch, args...)
		return contextReadyMsg{ch: ch, cmd: cmd, prompt: prompt, extraArgs: args}
	}
}

func (m *Model) spawnChangesAssertionsCall(specPath, missionDir, projectDir string, verbose bool, retryFeedback string) tea.Cmd {
	return func() tea.Msg {
		prompt := BuildChangesAssertionsPrompt(specPath, missionDir, projectDir, m.changesScopeJSON, retryFeedback, m.changesPromptTight)
		ch := make(chan ClaudeStreamMsg, 64)
		v := verbose
		args := []string{"--allowedTools", "Read", "--max-turns", "1", "--model", "opus"}
		cmd := StartClaude(prompt, projectDir, &v, ch, args...)
		return contextReadyMsg{ch: ch, cmd: cmd, prompt: prompt, extraArgs: args}
	}
}

func (m *Model) spawnChangesFeaturesCall(specPath, missionDir, projectDir string, assertionIDs map[string][]string, verbose bool, retryFeedback string) tea.Cmd {
	return func() tea.Msg {
		prompt := BuildChangesFeaturesPrompt(specPath, missionDir, projectDir, assertionIDs, m.changesScopeJSON, retryFeedback, m.changesPromptTight)
		ch := make(chan ClaudeStreamMsg, 64)
		v := verbose
		args := []string{"--allowedTools", "Read", "--max-turns", "1", "--model", "opus"}
		cmd := StartClaude(prompt, projectDir, &v, ch, args...)
		return contextReadyMsg{ch: ch, cmd: cmd, prompt: prompt, extraArgs: args}
	}
}

// spawnKnowledgeCall is the v3 Phase 4 spawner: sonnet synthesizes knowledge
// from spec + analysis + already-decomposed features. Sonnet is sufficient
// because knowledge is extraction/summary, not decomposition reasoning.
func (m *Model) spawnKnowledgeCall(specPath, projectDir string, verbose bool, features []Feature, retryFeedback string) tea.Cmd {
	return func() tea.Msg {
		prompt := BuildKnowledgePromptV2(specPath, projectDir, features, retryFeedback)
		ch := make(chan ClaudeStreamMsg, 64)
		v := verbose
		args := []string{"--allowedTools", "Read", "--max-turns", "1", "--model", "sonnet"}
		cmd := StartClaude(prompt, projectDir, &v, ch, args...)
		return contextReadyMsg{ch: ch, cmd: cmd, prompt: prompt, extraArgs: args}
	}
}

func (m Model) finalizeGeneration(specPath, mDir string) (tea.Model, tea.Cmd) {
	m.genPhase = GenPhaseNone

	var knowledge []string
	if m.knowledgeResult != nil {
		knowledge = ParseKnowledgeJSON(*m.knowledgeResult)
		m.knowledgeResult = nil
	}

	features := m.pendingFeatures
	m.pendingFeatures = nil

	if len(features) == 0 {
		state := ReadMissionState(mDir)
		if state.Exists && len(state.Features) > 0 {
			m.missionDir = mDir
			m.mission = state
			m.editingSpec = false
			m.phase = PhaseReview
			m.reviewTab = ReviewTabChat
			m.reviewInput.Focus()
			totalTime := time.Since(m.genStartTime).Round(time.Second)
			m.discoveryMsgs = append(m.discoveryMsgs, ChatMessage{Role: "system", Text: fmt.Sprintf("GEN_COMPLETE:%s:%d features (recovered from disk)", totalTime, len(state.Features))})
			m.updateReviewContent()
			m.viewport.SetContent(m.renderChatContent())
			m.viewport.GotoBottom()
			return m, nil
		}
		m.discoveryMsgs = append(m.discoveryMsgs, ChatMessage{Role: "system", Text: "✕ No features available — plan generation failed"})
		m.viewport.SetContent(m.renderChatContent())
		m.viewport.GotoBottom()
		return m, sendNotification("Quest", "Plan generation failed — no features parsed")
	}

	project := extractSpecTitle(specPath)
	if project == "" {
		project = filepath.Base(specPath)
	}
	slug := ""
	if m.activeSpec != nil {
		slug = m.activeSpec.Slug
	}
	if slug == "" {
		slug = filepath.Base(specPath)
	}
	assertions := parseAssertionsFromContract(filepath.Join(mDir, "validation-contract.md"))

	plan := PlanData{
		Slug:       slug,
		Spec:       fmt.Sprintf("docs/specs/%s/spec.md", slug),
		Project:    project,
		Owner:      "",
		Features:   features,
		Assertions: assertions,
		Knowledge:  knowledge,
	}

	_ = WriteMissionFiles(specPath, m.projectDir, plan)
	m.missionDir = mDir
	m.activeSpec = &SpecEntry{Slug: slug, SpecPath: specPath}
	m.mission = ReadMissionState(mDir)
	m.editingSpec = false

	totalAssertions := 0
	for _, a := range assertions {
		totalAssertions += len(a.Items)
	}
	m.discoveryMsgs = append(m.discoveryMsgs, ChatMessage{Role: "system", Text: fmt.Sprintf("  %d features, %d assertions — starting critic validation...", len(features), totalAssertions)})

	return m.startCriticLoop()
}

func (m Model) fullRegenMissionPlan() (tea.Model, tea.Cmd) {
	spec := m.activeSpec
	m.phase = PhaseRunning
	m.claudeRunning = true
	now := time.Now()
	m.claudeStartTime = now
	m.genStartTime = now
	m.editingSpec = true
	m.fullRegenMode = true
	m.regenMode = false
	m.changesMode = false
	m.changesScopeJSON = ""
	m.changesPromptTight = false
	m.changesTimeouts = 0
	m.resetChangesPhaseBudgetTracking()
	m.genPhase = GenPhaseNone
	m.criticLoopCount = 0
	m.criticPassed = false
	m.criticBypassed = false
	m.discoveryMsgs = []ChatMessage{
		{Role: "system", Text: fmt.Sprintf("Fully regenerating quest plan for %s (replanning all non-terminal work)...", spec.Slug)},
		{Role: "system", Text: "This will regenerate the plan as a whole, then run critic validation."},
		{Role: "system", Text: "Estimated time: 2-8 minutes across 3 phases."},
		{Role: "system", Text: ""},
		{Role: "system", Text: "PHASE_ROADMAP_FULL"},
		{Role: "system", Text: ""},
		{Role: "system", Text: "PHASE_START_FULL:1"},
	}
	m.streamLines = nil
	m.claudeRetries = 0
	m.claudeSessionID = ""
	m.claudeResumeHint = ""

	m.viewport.SetContent(m.renderChatContent())
	m.viewport.GotoBottom()

	specPath := spec.SpecPath
	missionDir := m.missionDir
	projectDir := m.projectDir
	verboseVal := m.verbose

	return m, tea.Batch(
		m.spinner.Tick,
		func() tea.Msg {
			prompt := BuildRegenPlanPrompt(specPath, missionDir, projectDir)
			ch := make(chan ClaudeStreamMsg, 64)
			v := verboseVal
			args := []string{"--allowedTools", "Read", "--max-turns", "4", "--model", "opus"}
			cmd := StartClaude(prompt, projectDir, &v, ch, args...)
			return contextReadyMsg{ch: ch, cmd: cmd, prompt: prompt, extraArgs: args}
		},
	)
}

func (m Model) regenMissionPlan() (tea.Model, tea.Cmd) {
	spec := m.activeSpec
	m.phase = PhaseRunning
	m.claudeRunning = true
	now := time.Now()
	m.claudeStartTime = now
	m.genStartTime = now
	m.editingSpec = true
	m.fullRegenMode = false
	m.regenMode = true
	m.changesMode = false
	m.changesScopeJSON = ""
	m.changesPromptTight = false
	m.changesTimeouts = 0
	m.resetChangesPhaseBudgetTracking()
	m.genPhase = GenPhaseAssertions
	m.criticLoopCount = 0
	m.criticPassed = false
	m.criticBypassed = false
	m.discoveryMsgs = []ChatMessage{
		{Role: "system", Text: fmt.Sprintf("Regenerating quest plan for %s (preserving completed work)...", spec.Slug)},
		{Role: "system", Text: "This will regenerate assertions and features, then run critic validation."},
		{Role: "system", Text: "Estimated time: 1-4 minutes across 4 phases."},
		{Role: "system", Text: ""},
		{Role: "system", Text: "PHASE_ROADMAP_REGEN"},
		{Role: "system", Text: ""},
		{Role: "system", Text: "PHASE_START_REGEN:1"},
	}
	m.streamLines = nil
	m.claudeRetries = 0
	m.claudeSessionID = ""
	m.claudeResumeHint = ""

	m.viewport.SetContent(m.renderChatContent())
	m.viewport.GotoBottom()

	specPath := spec.SpecPath
	missionDir := m.missionDir
	projectDir := m.projectDir
	verboseVal := m.verbose
	regenAssertionsCmd := m.spawnRegenAssertionsCall(specPath, missionDir, projectDir, verboseVal, "")

	return m, tea.Batch(
		m.spinner.Tick,
		regenAssertionsCmd,
	)
}

func (m Model) changesMissionPlan() (tea.Model, tea.Cmd) {
	spec := m.activeSpec
	m.phase = PhaseRunning
	m.claudeRunning = true
	now := time.Now()
	m.claudeStartTime = now
	m.genStartTime = now
	m.editingSpec = true
	m.fullRegenMode = false
	m.regenMode = false
	m.changesMode = true
	m.changesScopeJSON = ""
	m.changesPromptTight = false
	m.changesTimeouts = 0
	m.resetChangesPhaseBudgetTracking()
	m.genPhase = GenPhaseAnalysis
	m.criticLoopCount = 0
	m.criticPassed = false
	m.criticBypassed = false
	m.discoveryMsgs = []ChatMessage{
		{Role: "system", Text: fmt.Sprintf("Analyzing spec changes for %s...", spec.Slug)},
		{Role: "system", Text: "This will detect semantic changes first, then regenerate only impacted assertions/features."},
		{Role: "system", Text: "Estimated time: 2-6 minutes across 5 phases."},
		{Role: "system", Text: ""},
		{Role: "system", Text: "PHASE_ROADMAP_CHANGES"},
		{Role: "system", Text: ""},
		{Role: "system", Text: "PHASE_START_CHANGES:1"},
	}
	m.streamLines = nil
	m.claudeRetries = 0
	m.claudeSessionID = ""
	m.claudeResumeHint = ""

	m.viewport.SetContent(m.renderChatContent())
	m.viewport.GotoBottom()

	specPath := spec.SpecPath
	missionDir := m.missionDir
	projectDir := m.projectDir
	verboseVal := m.verbose
	changesAnalysisCmd := m.spawnChangesAnalysisCall(specPath, missionDir, projectDir, verboseVal)

	return m, tea.Batch(
		m.spinner.Tick,
		changesAnalysisCmd,
	)
}

func (m Model) startDiscovery() (tea.Model, tea.Cmd) {
	text := strings.TrimSpace(m.input.Value())
	if text == "" {
		return m, nil
	}

	m.fullRegenMode = false
	m.regenMode = false
	m.changesMode = false
	m.changesScopeJSON = ""
	m.input.Reset()
	m.input.Blur()
	m.discoveryMsgs = []ChatMessage{{Role: "user", Text: text}}
	m.streamLines = nil
	m.phase = PhaseDiscovery
	m.claudeRunning = true
	m.claudeStartTime = time.Now()

	ch := make(chan ClaudeStreamMsg, 64)
	m.claudeCh = ch
	var prompt string
	if m.editingSpec && m.activeSpec != nil {
		prompt = BuildEditDiscoveryPrompt(text, m.activeSpec.SpecPath, m.projectDir)
	} else {
		prompt = BuildDiscoveryPrompt(text, m.projectDir)
	}
	m.lastPrompt = prompt
	m.claudeRetries = 0
	m.claudeSessionID = ""
	m.claudeResumeHint = ""
	m.claudeCmd = StartClaude(prompt, m.projectDir, &m.verbose, ch)

	m.viewport.SetContent(m.renderChatContent())
	m.viewport.GotoBottom()

	return m, tea.Batch(listenClaude(ch), m.spinner.Tick)
}

func (m Model) sendDiscoveryFeedback(feedback string) (tea.Model, tea.Cmd) {
	m.input.Reset()
	m.fullRegenMode = false
	m.regenMode = false
	m.changesMode = false
	m.changesScopeJSON = ""
	m.input.Blur()
	m.discoveryMsgs = append(m.discoveryMsgs, ChatMessage{Role: "user", Text: feedback})
	m.streamLines = nil
	m.claudeRunning = true
	m.claudeStartTime = time.Now()

	ch := make(chan ClaudeStreamMsg, 64)
	m.claudeCh = ch
	m.claudeRetries = 0
	m.claudeResumeHint = ""
	// Keep a full-context prompt as the retry fallback regardless of path, in
	// case the session can't be resumed (e.g. it expired server-side).
	m.lastPrompt = BuildFollowUpPrompt(m.discoveryMsgs, feedback, m.projectDir)
	if m.claudeSessionID != "" {
		// Continuous session: the resumed conversation already holds the skill,
		// project context, and full history — send only the new user message.
		m.claudeCmd = StartClaude(feedback, m.projectDir, &m.verbose, ch, "--resume", m.claudeSessionID)
	} else {
		// No live session yet — replay the transcript as a flat prompt.
		m.claudeCmd = StartClaude(m.lastPrompt, m.projectDir, &m.verbose, ch)
	}

	m.viewport.SetContent(m.renderChatContent())
	m.viewport.GotoBottom()

	return m, tea.Batch(listenClaude(ch), m.spinner.Tick)
}

func (m Model) approveRequirements() (tea.Model, tea.Cmd) {
	m.phase = PhaseRunning
	m.fullRegenMode = false
	m.regenMode = false
	m.changesMode = false
	m.changesScopeJSON = ""
	m.input.Blur()
	m.streamLines = nil
	m.claudeRunning = true
	m.claudeStartTime = time.Now()

	ch := make(chan ClaudeStreamMsg, 64)
	m.claudeCh = ch
	editing := m.editingSpec && m.activeSpec != nil
	// Full-context prompt kept as the retry fallback regardless of path.
	if editing {
		m.lastPrompt = BuildEditPlanPrompt(m.discoveryMsgs, m.activeSpec.SpecPath, m.projectDir)
	} else {
		m.lastPrompt = BuildPlanPrompt(m.discoveryMsgs, m.projectDir)
	}
	m.claudeRetries = 0
	m.claudeResumeHint = ""
	if m.claudeSessionID != "" {
		// Resume the discovery session: it already holds the full requirements
		// conversation, so generate the plan from its own memory rather than
		// replaying a flattened transcript.
		resumePrompt := BuildPlanPromptResume(m.projectDir)
		if editing {
			resumePrompt = BuildEditPlanPromptResume(m.activeSpec.SpecPath, m.projectDir)
		}
		m.claudeCmd = StartClaude(resumePrompt, m.projectDir, &m.verbose, ch, "--resume", m.claudeSessionID)
	} else {
		m.claudeCmd = StartClaude(m.lastPrompt, m.projectDir, &m.verbose, ch)
	}

	m.discoveryMsgs = append(m.discoveryMsgs, ChatMessage{Role: "system", Text: "Requirements approved. Generating quest plan..."})
	m.viewport.SetContent(m.renderChatContent())
	m.viewport.GotoBottom()

	return m, tea.Batch(listenClaude(ch), m.spinner.Tick)
}

func (m Model) approvePlan() (tea.Model, tea.Cmd) {
	m.reviewInput.Reset()
	m.reviewChat = nil

	var selectedFindings []CriticFinding
	for i, sel := range m.criticSelected {
		if sel && i < len(m.criticAdvisory) {
			selectedFindings = append(selectedFindings, m.criticAdvisory[i])
		}
	}

	if len(selectedFindings) > 0 {
		return m.startAdvisoryFix(selectedFindings)
	}

	if m.criticPassed {
		return m.startWorkersAfterCritic()
	}

	// If the user approves with unresolved critic blocking findings, honor
	// the "approve anyway" contract shown in the review UI and bypass the
	// critic gate for this start.
	if len(m.criticBlocking) > 0 {
		return m.startWorkersSkipCritic()
	}

	m.phase = PhaseDashboard

	var pending []Feature
	for _, f := range m.mission.Features {
		if f.Status == "pending" {
			pending = append(pending, f)
		}
	}

	if len(pending) == 0 {
		m.updateDashboardContent()
		return m, nil
	}

	logger, _ := NewMissionLogger(m.missionDir)
	m.logger = logger
	m.executing = true
	m.workerLogs = nil
	m.logFilter = -1

	m.bindCostTracker(m.missionDir)
	pool := NewWorkerPool(m.projectDir, m.missionDir, pending, logger, &m.verbose, m.costTracker)
	pool.autonomousMode = true
	m.workerPool = pool

	m.updateDashboardContent()
	return m, tea.Batch(pool.Start(), m.spinner.Tick)
}

func (m Model) startWorkers() (tea.Model, tea.Cmd) {
	m.mission = ReadMissionState(m.missionDir)
	m.resetCriticGenerationState()

	// If the critic already passed during plan generation OR the user
	// explicitly approved running without the critic gate, skip the gate
	// and go straight to spawning workers.
	if m.criticPassed || m.criticBypassed {
		return m.startWorkersAfterCritic()
	}

	var pending []Feature
	for _, f := range m.mission.Features {
		if f.Status == "pending" {
			pending = append(pending, f)
		}
	}
	if len(pending) == 0 {
		return m, nil
	}

	logger, _ := NewMissionLogger(m.missionDir)
	m.logger = logger
	m.executing = true
	m.workerLogs = nil
	m.logFilter = -1

	m.bindCostTracker(m.missionDir)
	pool := NewWorkerPool(m.projectDir, m.missionDir, pending, logger, &m.verbose, m.costTracker)
	pool.autonomousMode = true
	m.workerPool = pool

	m.updateDashboardContent()
	return m, tea.Batch(pool.Start(), m.spinner.Tick)
}

func (m Model) startAutoFix() (tea.Model, tea.Cmd) {
	report := m.criticFailReport
	if report == nil {
		return m, nil
	}

	specDir := filepath.Dir(m.missionDir)
	projectDir := m.projectDir

	m.autoFixRunning = true
	m.activeTab = TabLog
	m.workerLogs = append(m.workerLogs, "[AUTOFIX] ▶ Starting auto-fix agent (Sonnet)...")

	afCh := make(chan autoFixEventMsg, 64)
	m.autoFixCh = afCh

	prompt := BuildAutoFixPrompt(report, specDir, projectDir)
	claudeCh := make(chan ClaudeStreamMsg, 64)
	v := true
	cmd := StartClaude(prompt, projectDir, &v, claudeCh,
		"--allowedTools", "Read,Edit,Write,Bash,Glob,Grep",
		"--max-turns", "15",
		"--model", "sonnet",
	)
	m.claudeCmd = cmd

	costTracker := m.costTracker
	go func() {
		for msg := range claudeCh {
			if msg.Done {
				if costTracker != nil {
					costTracker.Record(CostRecord{
						Model:               msg.Model,
						Role:                "autofix",
						Phase:               -1,
						InputTokens:         msg.InputTokens,
						OutputTokens:        msg.OutputTokens,
						CacheCreationTokens: msg.CacheCreationTokens,
						CacheReadTokens:     msg.CacheReadTokens,
						CostUSD:             msg.CostUSD,
					})
				}
				afCh <- autoFixEventMsg{done: true, err: msg.Err}
				close(afCh)
				return
			}
			if msg.Line != "" {
				afCh <- autoFixEventMsg{line: msg.Line}
			}
		}
		afCh <- autoFixEventMsg{done: true}
		close(afCh)
	}()

	m.updateDashboardContent()
	return m, tea.Batch(listenAutoFix(afCh), m.spinner.Tick)
}

func (m Model) startWorkersSkipCritic() (tea.Model, tea.Cmd) {
	m.mission = ReadMissionState(m.missionDir)
	m.resetCriticGenerationState()
	m.criticBypassed = true

	var pending []Feature
	for _, f := range m.mission.Features {
		if f.Status == "pending" {
			pending = append(pending, f)
		}
	}
	if len(pending) == 0 {
		return m, nil
	}

	logger, _ := NewMissionLogger(m.missionDir)
	m.logger = logger
	m.executing = true
	m.workerLogs = append(m.workerLogs, "[ORCH] ⚠ Critic bypassed — starting workers directly")
	m.logFilter = -1

	m.bindCostTracker(m.missionDir)
	pool := NewWorkerPool(m.projectDir, m.missionDir, pending, logger, &m.verbose, m.costTracker)
	pool.skipCritic = true
	pool.autonomousMode = true
	m.workerPool = pool

	m.updateDashboardContent()
	return m, tea.Batch(pool.Start(), m.spinner.Tick)
}

func (m Model) startCriticLoop() (tea.Model, tea.Cmd) {
	m.genPhase = GenPhaseCritic
	m.criticLoopCount++
	m.claudeRunning = true
	m.claudeStartTime = time.Now()
	m.criticPassed = false
	m.criticBypassed = false

	// The mission directory now exists; bind the cost tracker (flushing any
	// discovery/plan-generation costs buffered before it existed) so the
	// critic and all later calls record live.
	m.bindCostTracker(m.missionDir)

	if m.changesMode {
		m.discoveryMsgs = append(m.discoveryMsgs, ChatMessage{Role: "system", Text: "PHASE_START_CHANGES:4"})
	} else if m.regenMode {
		m.discoveryMsgs = append(m.discoveryMsgs, ChatMessage{Role: "system", Text: "PHASE_START_REGEN:3"})
	} else if m.fullRegenMode {
		m.discoveryMsgs = append(m.discoveryMsgs, ChatMessage{Role: "system", Text: "PHASE_START_FULL:2"})
	} else {
		m.discoveryMsgs = append(m.discoveryMsgs, ChatMessage{Role: "system", Text: "PHASE_START:5"})
	}
	m.streamLines = nil
	m.viewport.SetContent(m.renderChatContent())
	m.viewport.GotoBottom()

	doneCh := make(chan criticLoopMsg, 1)
	streamCh := make(chan criticStreamMsg, 64)
	m.criticLoopCh = doneCh
	m.criticStreamCh = streamCh

	specDir := filepath.Dir(m.missionDir)
	projectDir := m.projectDir
	verbose := m.verbose
	attempt := m.criticLoopCount
	costTracker := m.costTracker

	go func() {
		eventCh := make(chan WorkerEvent, 64)

		go RunCriticGate(projectDir, ResolveArtifactDir(specDir), &verbose, eventCh, costTracker)

		var report *CriticReport
		var verdict string
		for ev := range eventCh {
			if ev.Line != "" {
				streamCh <- criticStreamMsg{line: ev.Line}
			}
			if ev.Done {
				verdict = ev.Verdict
				report = ev.CriticReport
				break
			}
		}
		close(streamCh)

		if verdict == "PASS" || report == nil {
			doneCh <- criticLoopMsg{
				report:  report,
				passed:  verdict == "PASS",
				attempt: attempt,
			}
			return
		}

		advisory := report.AdvisoryFindings()
		blocking := report.BlockingFailures()
		passed := !report.HasBlockingFailures()

		doneCh <- criticLoopMsg{
			report:   report,
			passed:   passed,
			advisory: advisory,
			blocking: blocking,
			attempt:  attempt,
		}
	}()

	return m, tea.Batch(listenCriticStream(streamCh), listenCriticLoop(doneCh), m.spinner.Tick)
}

func (m Model) handleCriticLoopMsg(msg criticLoopMsg) (tea.Model, tea.Cmd) {
	if !m.inCriticGenerationFlow() {
		return m, nil
	}

	m.claudeRunning = false
	m.criticStreamCh = nil
	elapsed := time.Since(m.claudeStartTime).Round(time.Second)

	if msg.err != nil {
		if m.changesMode {
			m.discoveryMsgs = append(m.discoveryMsgs, ChatMessage{Role: "system", Text: fmt.Sprintf("PHASE_DONE_CHANGES:4:%s:error — %v", elapsed, msg.err)})
		} else if m.regenMode {
			m.discoveryMsgs = append(m.discoveryMsgs, ChatMessage{Role: "system", Text: fmt.Sprintf("PHASE_DONE_REGEN:3:%s:error — %v", elapsed, msg.err)})
		} else if m.fullRegenMode {
			m.discoveryMsgs = append(m.discoveryMsgs, ChatMessage{Role: "system", Text: fmt.Sprintf("PHASE_DONE_FULL:2:%s:error — %v", elapsed, msg.err)})
		} else {
			m.discoveryMsgs = append(m.discoveryMsgs, ChatMessage{Role: "system", Text: fmt.Sprintf("PHASE_DONE:5:%s:error — %v", elapsed, msg.err)})
		}
		m.viewport.SetContent(m.renderChatContent())
		m.viewport.GotoBottom()
		return m.transitionToReview()
	}

	if msg.passed {
		detail := "all spec-quality checks passed"
		if len(msg.advisory) > 0 {
			detail += fmt.Sprintf(", %d advisory", len(msg.advisory))
		}
		if m.changesMode {
			m.discoveryMsgs = append(m.discoveryMsgs, ChatMessage{Role: "system", Text: fmt.Sprintf("PHASE_DONE_CHANGES:4:%s:%s", elapsed, detail)})
		} else if m.regenMode {
			m.discoveryMsgs = append(m.discoveryMsgs, ChatMessage{Role: "system", Text: fmt.Sprintf("PHASE_DONE_REGEN:3:%s:%s", elapsed, detail)})
		} else if m.fullRegenMode {
			m.discoveryMsgs = append(m.discoveryMsgs, ChatMessage{Role: "system", Text: fmt.Sprintf("PHASE_DONE_FULL:2:%s:%s", elapsed, detail)})
		} else {
			m.discoveryMsgs = append(m.discoveryMsgs, ChatMessage{Role: "system", Text: fmt.Sprintf("PHASE_DONE:5:%s:%s", elapsed, detail)})
		}
		m.criticAdvisory = msg.advisory
		m.criticBlocking = nil
		m.criticSelected = make([]bool, len(msg.advisory))
		m.criticPassed = true

		if len(m.streamLines) > 0 {
			m.discoveryMsgs = append(m.discoveryMsgs, ChatMessage{Role: "assistant", Text: strings.Join(m.streamLines, "\n")})
			m.streamLines = nil
		}

		m.viewport.SetContent(m.renderChatContent())
		m.viewport.GotoBottom()
		return m.transitionToReview()
	}

	blockingCount := len(msg.blocking)
	if m.changesMode {
		m.discoveryMsgs = append(m.discoveryMsgs, ChatMessage{Role: "system", Text: fmt.Sprintf("PHASE_DONE_CHANGES:4:%s:%d blocking findings", elapsed, blockingCount)})
	} else if m.regenMode {
		m.discoveryMsgs = append(m.discoveryMsgs, ChatMessage{Role: "system", Text: fmt.Sprintf("PHASE_DONE_REGEN:3:%s:%d blocking findings", elapsed, blockingCount)})
	} else if m.fullRegenMode {
		m.discoveryMsgs = append(m.discoveryMsgs, ChatMessage{Role: "system", Text: fmt.Sprintf("PHASE_DONE_FULL:2:%s:%d blocking findings", elapsed, blockingCount)})
	} else {
		m.discoveryMsgs = append(m.discoveryMsgs, ChatMessage{Role: "system", Text: fmt.Sprintf("PHASE_DONE:5:%s:%d blocking findings", elapsed, blockingCount)})
	}

	if len(m.streamLines) > 0 {
		m.discoveryMsgs = append(m.discoveryMsgs, ChatMessage{Role: "assistant", Text: strings.Join(m.streamLines, "\n")})
		m.streamLines = nil
	}

	const maxCriticLoops = 3
	if m.criticLoopCount >= maxCriticLoops {
		m.discoveryMsgs = append(m.discoveryMsgs, ChatMessage{Role: "system", Text: fmt.Sprintf("⚠ %d blocking findings remain after %d auto-fix attempts", blockingCount, maxCriticLoops)})
		m.criticAdvisory = msg.advisory
		m.criticBlocking = msg.blocking
		m.criticSelected = make([]bool, len(msg.advisory))
		m.viewport.SetContent(m.renderChatContent())
		m.viewport.GotoBottom()
		return m.transitionToReview()
	}

	m.criticAdvisory = msg.advisory

	m.discoveryMsgs = append(m.discoveryMsgs, ChatMessage{Role: "system", Text: fmt.Sprintf("Auto-fixing %d blocking findings (attempt %d/%d)...", blockingCount, m.criticLoopCount, maxCriticLoops)})
	m.viewport.SetContent(m.renderChatContent())
	m.viewport.GotoBottom()

	return m.startCriticFix(msg.report)
}

func (m Model) startCriticFix(report *CriticReport) (tea.Model, tea.Cmd) {
	m.genPhase = GenPhaseFixLoop
	m.claudeRunning = true
	m.claudeStartTime = time.Now()
	m.streamLines = nil

	if m.changesMode {
		m.discoveryMsgs = append(m.discoveryMsgs, ChatMessage{Role: "system", Text: "PHASE_START_CHANGES:5"})
	} else if m.regenMode {
		m.discoveryMsgs = append(m.discoveryMsgs, ChatMessage{Role: "system", Text: "PHASE_START_REGEN:4"})
	} else if m.fullRegenMode {
		m.discoveryMsgs = append(m.discoveryMsgs, ChatMessage{Role: "system", Text: "PHASE_START_FULL:3"})
	} else {
		m.discoveryMsgs = append(m.discoveryMsgs, ChatMessage{Role: "system", Text: "PHASE_START:6"})
	}
	m.viewport.SetContent(m.renderChatContent())
	m.viewport.GotoBottom()

	specDir := filepath.Dir(m.missionDir)
	projectDir := m.projectDir

	streamCh := make(chan criticStreamMsg, 64)
	m.criticStreamCh = streamCh

	doneCh := make(chan criticFixDoneMsg, 1)

	go func() {
		v := true
		err := RunCriticBlockingAutoFix(report, specDir, projectDir, &v, func(line string) {
			if line != "" {
				streamCh <- criticStreamMsg{line: line}
			}
		})
		close(streamCh)
		doneCh <- criticFixDoneMsg{err: err}
	}()

	return m, tea.Batch(listenCriticStream(streamCh), listenCriticFixDone(doneCh), m.spinner.Tick)
}

func (m Model) handleCriticFixDone(msg criticFixDoneMsg) (tea.Model, tea.Cmd) {
	if !m.inCriticGenerationFlow() {
		return m, nil
	}

	m.claudeRunning = false
	m.criticStreamCh = nil
	elapsed := time.Since(m.claudeStartTime).Round(time.Second)

	if msg.err != nil {
		if m.changesMode {
			m.discoveryMsgs = append(m.discoveryMsgs, ChatMessage{Role: "system", Text: fmt.Sprintf("PHASE_DONE_CHANGES:5:%s:error — %v", elapsed, msg.err)})
		} else if m.regenMode {
			m.discoveryMsgs = append(m.discoveryMsgs, ChatMessage{Role: "system", Text: fmt.Sprintf("PHASE_DONE_REGEN:4:%s:error — %v", elapsed, msg.err)})
		} else if m.fullRegenMode {
			m.discoveryMsgs = append(m.discoveryMsgs, ChatMessage{Role: "system", Text: fmt.Sprintf("PHASE_DONE_FULL:3:%s:error — %v", elapsed, msg.err)})
		} else {
			m.discoveryMsgs = append(m.discoveryMsgs, ChatMessage{Role: "system", Text: fmt.Sprintf("PHASE_DONE:6:%s:error — %v", elapsed, msg.err)})
		}
		m.viewport.SetContent(m.renderChatContent())
		m.viewport.GotoBottom()
		return m.transitionToReview()
	}

	detail := fmt.Sprintf("fixes applied in %s", elapsed)
	if m.changesMode {
		m.discoveryMsgs = append(m.discoveryMsgs, ChatMessage{Role: "system", Text: fmt.Sprintf("PHASE_DONE_CHANGES:5:%s:%s", elapsed, detail)})
	} else if m.regenMode {
		m.discoveryMsgs = append(m.discoveryMsgs, ChatMessage{Role: "system", Text: fmt.Sprintf("PHASE_DONE_REGEN:4:%s:%s", elapsed, detail)})
	} else if m.fullRegenMode {
		m.discoveryMsgs = append(m.discoveryMsgs, ChatMessage{Role: "system", Text: fmt.Sprintf("PHASE_DONE_FULL:3:%s:%s", elapsed, detail)})
	} else {
		m.discoveryMsgs = append(m.discoveryMsgs, ChatMessage{Role: "system", Text: fmt.Sprintf("PHASE_DONE:6:%s:%s", elapsed, detail)})
	}

	if len(m.streamLines) > 0 {
		m.discoveryMsgs = append(m.discoveryMsgs, ChatMessage{Role: "assistant", Text: strings.Join(m.streamLines, "\n")})
		m.streamLines = nil
	}

	m.mission = ReadMissionState(m.missionDir)
	m.viewport.SetContent(m.renderChatContent())
	m.viewport.GotoBottom()

	return m.startCriticLoop()
}

func (m Model) transitionToReview() (tea.Model, tea.Cmd) {
	m.genPhase = GenPhaseNone
	m.fullRegenMode = false
	m.regenMode = false
	m.changesMode = false
	m.changesScopeJSON = ""
	m.phase = PhaseReview
	if len(m.criticAdvisory) > 0 || len(m.criticBlocking) > 0 {
		m.reviewTab = ReviewTabCritic
	} else {
		m.reviewTab = ReviewTabChat
	}
	m.ensureCriticSelectionState()
	m.reviewInput.Focus()
	m.mission = ReadMissionState(m.missionDir)
	m.updateReviewContent()

	totalTime := time.Since(m.genStartTime).Round(time.Second)
	status := "✓ Critic passed"
	if len(m.criticBlocking) > 0 {
		status = fmt.Sprintf("⚠ %d blocking findings unresolved", len(m.criticBlocking))
	}
	m.discoveryMsgs = append(m.discoveryMsgs, ChatMessage{Role: "system", Text: fmt.Sprintf("GEN_COMPLETE:%s:%d features — %s", totalTime, len(m.mission.Features), status)})
	m.viewport.SetContent(m.renderChatContent())
	m.viewport.GotoBottom()

	return m, sendNotification("Quest", fmt.Sprintf("Plan ready — %d features", len(m.mission.Features)))
}

func (m Model) startWorkersAfterCritic() (tea.Model, tea.Cmd) {
	m.mission = ReadMissionState(m.missionDir)
	m.resetCriticGenerationState()

	var pending []Feature
	for _, f := range m.mission.Features {
		if f.Status == "pending" {
			pending = append(pending, f)
		}
	}
	if len(pending) == 0 {
		m.phase = PhaseDashboard
		m.updateDashboardContent()
		return m, nil
	}

	m.phase = PhaseDashboard
	logger, _ := NewMissionLogger(m.missionDir)
	m.logger = logger
	m.executing = true
	m.workerLogs = nil
	m.logFilter = -1

	m.bindCostTracker(m.missionDir)
	pool := NewWorkerPool(m.projectDir, m.missionDir, pending, logger, &m.verbose, m.costTracker)
	pool.skipCritic = true
	pool.autonomousMode = true
	m.workerPool = pool

	m.updateDashboardContent()
	return m, tea.Batch(pool.Start(), m.spinner.Tick)
}

func (m Model) inCriticGenerationFlow() bool {
	if m.phase != PhaseRunning {
		return false
	}
	return m.genPhase == GenPhaseCritic || m.genPhase == GenPhaseFixLoop
}

func (m *Model) resetCriticGenerationState() {
	m.phase = PhaseDashboard
	m.genPhase = GenPhaseNone
	m.criticLoopCh = nil
	m.criticStreamCh = nil
}

func (m Model) startAdvisoryFix(findings []CriticFinding) (tea.Model, tea.Cmd) {
	m.autoFixRunning = true
	m.phase = PhaseDashboard
	m.activeTab = TabLog
	m.workerLogs = append(m.workerLogs, fmt.Sprintf("[AUTOFIX] ▶ Fixing %d advisory findings...", len(findings)))

	specDir := filepath.Dir(m.missionDir)
	projectDir := m.projectDir
	costTracker := m.costTracker

	return m, func() tea.Msg {
		prompt := BuildAdvisoryAutoFixPrompt(findings, specDir, projectDir)
		if prompt == "" {
			return advisoryFixDoneMsg{err: fmt.Errorf("no findings to fix")}
		}
		ch := make(chan ClaudeStreamMsg, 64)
		v := false
		_ = StartClaude(prompt, projectDir, &v, ch,
			"--allowedTools", "Read,Edit,Write",
			"--max-turns", "10",
			"--model", "sonnet",
		)
		var lastErr error
		for msg := range ch {
			if msg.Done {
				lastErr = msg.Err
				if costTracker != nil {
					costTracker.Record(CostRecord{
						Model:               msg.Model,
						Role:                "autofix",
						Phase:               -1,
						InputTokens:         msg.InputTokens,
						OutputTokens:        msg.OutputTokens,
						CacheCreationTokens: msg.CacheCreationTokens,
						CacheReadTokens:     msg.CacheReadTokens,
						CostUSD:             msg.CostUSD,
					})
				}
				break
			}
		}
		return advisoryFixDoneMsg{err: lastErr}
	}
}

func (m *Model) resetFeatures(inclueDone bool) int {
	path := filepath.Join(m.missionDir, "features.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}

	var manifest FeaturesManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return 0
	}

	all := make([]Feature, 0, len(manifest.Features)+len(manifest.FixFeatures))
	all = append(all, manifest.Features...)
	all = append(all, manifest.FixFeatures...)
	tainted := loadTaintedFeatureIDs(m.missionDir, all)
	outcomes := buildFeatureOutcomes(all, tainted)

	count := 0
	reset := func(features []Feature) {
		for i := range features {
			s := features[i].Status
			if s == "pending" {
				continue
			}
			if !inclueDone && (s == "done" || s == "validated") {
				continue
			}
			if !inclueDone {
				if out, ok := outcomes[features[i].ID]; ok && out.EffectiveDone {
					continue
				}
			}
			features[i].Status = "pending"
			features[i].Resolution = ""
			features[i].ResolvedBy = ""
			features[i].ResolvedAt = ""
			features[i].Tainted = false
			count++
		}
	}
	reset(manifest.Features)
	reset(manifest.FixFeatures)

	if count == 0 {
		return 0
	}

	out, _ := json.MarshalIndent(manifest, "", "  ")
	_ = os.WriteFile(path, out, 0o644)
	m.mission = ReadMissionState(m.missionDir)
	return count
}

func (m *Model) fullResetMainFeatures() (int, int, error) {
	path := filepath.Join(m.missionDir, "features.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, err
	}

	var manifest FeaturesManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return 0, 0, err
	}

	rootCount := len(manifest.Features)
	clearedFixes := len(manifest.FixFeatures)

	for i := range manifest.Features {
		manifest.Features[i].Status = "pending"
		manifest.Features[i].Resolution = ""
		manifest.Features[i].ResolvedBy = ""
		manifest.Features[i].ResolvedAt = ""
		manifest.Features[i].Tainted = false
	}
	manifest.FixFeatures = nil

	out, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return 0, 0, err
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		return 0, 0, err
	}

	m.mission = ReadMissionState(m.missionDir)
	return rootCount, clearedFixes, nil
}

func (m Model) fullResetAndStart() (tea.Model, tea.Cmd) {
	rootCount, clearedFixes, err := m.fullResetMainFeatures()
	if err != nil {
		m.workerLogs = append(m.workerLogs, fmt.Sprintf("[ORCH] ✕ Full reset failed: %v", err))
		m.updateDashboardContent()
		return m, nil
	}

	nextModel, cmd := m.startWorkers()
	logLine := fmt.Sprintf(
		"[ORCH] Full reset requested by user — cleared %d fix features and reset %d root features to pending",
		clearedFixes, rootCount,
	)
	if next, ok := nextModel.(Model); ok {
		next.workerLogs = append(next.workerLogs, logLine)
		next.updateDashboardContent()
		return next, cmd
	}
	return nextModel, cmd
}

func (m Model) retryFeature(idx int) (tea.Model, tea.Cmd) {
	f := m.mission.Features[idx]

	statusMap := make(map[string]string)
	for _, feat := range m.mission.Features {
		statusMap[feat.ID] = feat.Status
	}
	tainted := loadTaintedFeatureIDs(m.missionDir, m.mission.Features)
	outcomes := buildFeatureOutcomes(m.mission.Features, tainted)

	var blockedBy []string
	for _, depID := range f.DependsOn {
		depStatus := statusMap[depID]
		if depStatus == "done" || depStatus == "validated" || depStatus == "awaiting_validation" {
			continue
		}

		out := outcomes[depID]
		if depStatus == "blocked" && out.EffectiveDone && out.Resolution != ResolutionResolvedTainted {
			continue
		}

		if out.Resolution != "" {
			blockedBy = append(blockedBy, fmt.Sprintf("%s (%s/%s)", depID, depStatus, out.Resolution))
		} else {
			blockedBy = append(blockedBy, fmt.Sprintf("%s (%s)", depID, depStatus))
		}
	}

	if len(blockedBy) > 0 {
		m.workerLogs = append(m.workerLogs, fmt.Sprintf("[ORCH] Cannot retry %s — deps not done: %s", f.ID, strings.Join(blockedBy, ", ")))
		m.updateDashboardContent()
		return m, nil
	}

	m.resetSingleFeature(f.ID)
	return m.startSingleWorker(f.ID)
}

func (m *Model) resetSingleFeature(featureID string) {
	path := filepath.Join(m.missionDir, "features.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}

	var manifest FeaturesManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return
	}

	set := func(features []Feature) {
		for i := range features {
			if features[i].ID == featureID {
				features[i].Status = "pending"
				features[i].Resolution = ""
				features[i].ResolvedBy = ""
				features[i].ResolvedAt = ""
				features[i].Tainted = false
			}
		}
	}
	set(manifest.Features)
	set(manifest.FixFeatures)

	out, _ := json.MarshalIndent(manifest, "", "  ")
	_ = os.WriteFile(path, out, 0o644)
	m.mission = ReadMissionState(m.missionDir)
}

func (m Model) startSingleWorker(featureID string) (tea.Model, tea.Cmd) {
	m.mission = ReadMissionState(m.missionDir)

	var target []Feature
	for _, f := range m.mission.Features {
		if f.ID == featureID && f.Status == "pending" {
			target = append(target, f)
		}
	}
	if len(target) == 0 {
		return m, nil
	}

	logger, _ := NewMissionLogger(m.missionDir)
	m.logger = logger
	m.executing = true

	m.bindCostTracker(m.missionDir)
	pool := NewWorkerPool(m.projectDir, m.missionDir, target, logger, &m.verbose, m.costTracker)
	m.workerPool = pool
	m.workerLogs = append(m.workerLogs, fmt.Sprintf("[ORCH] Retrying %s — %s", featureID, target[0].Title))
	m.updateDashboardContent()
	return m, tea.Batch(pool.Start(), m.spinner.Tick)
}

func (m Model) rejectPlan() (tea.Model, tea.Cmd) {
	StopClaude(m.claudeCmd)
	m.claudeRunning = false
	m.phase = PhaseDiscovery
	m.discoveryMsgs = nil
	m.reviewInput.Reset()
	m.reviewChat = nil
	m.input.Focus()
	return m, nil
}

func (m Model) refinePlan(feedback string) (tea.Model, tea.Cmd) {
	m.isRefining = true
	m.reviewInput.Reset()
	m.claudeRunning = true
	m.claudeStartTime = time.Now()
	m.reviewChat = append(m.reviewChat, ChatMessage{Role: "user", Text: feedback})

	ch := make(chan ClaudeStreamMsg, 64)
	m.claudeCh = ch
	specDir := filepath.Dir(m.missionDir)
	m.lastPrompt = BuildRefinePlanPrompt(feedback, specDir, m.projectDir)
	m.claudeRetries = 0
	m.claudeSessionID = ""
	m.claudeResumeHint = ""
	m.claudeCmd = StartClaude(m.lastPrompt, m.projectDir, &m.verbose, ch)

	return m, tea.Batch(listenClaude(ch), m.spinner.Tick)
}

// --- Claude stream handler ---

func (m Model) retryClaude() (tea.Model, tea.Cmd) {
	m.claudeRunning = true
	m.claudeStartTime = time.Now()

	hadToolCalls := false
	for _, l := range m.streamLines {
		if strings.HasPrefix(l, "▸") {
			hadToolCalls = true
			break
		}
	}
	m.streamLines = nil

	ch := make(chan ClaudeStreamMsg, 64)
	m.claudeCh = ch

	genPhaseJSON := m.genPhase == GenPhaseAssertions || m.genPhase == GenPhaseFeatures || m.genPhase == GenPhaseKnowledge
	if m.claudeSessionID != "" && hadToolCalls && !genPhaseJSON {
		hint := m.claudeResumeHint
		if hint == "" {
			hint = "Continue from where you left off and complete the task."
		}
		m.claudeCmd = StartClaude(hint, m.projectDir, &m.verbose, ch,
			"--resume", m.claudeSessionID,
		)
	} else {
		m.claudeSessionID = ""
		m.claudeCmd = StartClaude(m.lastPrompt, m.projectDir, &m.verbose, ch, m.claudeExtraArgs...)
	}

	m.viewport.SetContent(m.renderChatContent())
	m.viewport.GotoBottom()
	return m, tea.Batch(listenClaude(ch), m.spinner.Tick)
}

func (m Model) retryClaudeWithDelay() (tea.Model, tea.Cmd) {
	delay := time.Duration(m.claudeRetries) * 2 * time.Second
	m.viewport.SetContent(m.renderChatContent())
	m.viewport.GotoBottom()
	return m, tea.Tick(delay, func(t time.Time) tea.Msg {
		return retryDelayMsg{}
	})
}

// bindCostTracker attaches a cost tracker for missionDir (loading any persisted
// records) and flushes records buffered before the directory existed. Idempotent:
// it only reloads when the target path changes.
func (m *Model) bindCostTracker(missionDir string) {
	if missionDir == "" {
		return
	}
	want := filepath.Join(missionDir, costFileName)
	if m.costTracker == nil || m.costTracker.path != want {
		m.costTracker = LoadCostTracker(missionDir)
	}
	if len(m.pendingCostRecords) > 0 {
		m.costTracker.RecordMany(m.pendingCostRecords)
		m.pendingCostRecords = nil
	}
}

// currentClaudeRole maps the current TUI phase to a cost role for Claude calls
// that flow through handleClaudeStream (discovery and plan generation).
func (m Model) currentClaudeRole() string {
	if m.phase == PhaseDiscovery {
		return "discovery"
	}
	switch m.genPhase {
	case GenPhaseAnalysis:
		return "analysis"
	case GenPhaseAssertions:
		return "assertions"
	case GenPhaseFeatures:
		return "features"
	case GenPhaseKnowledge:
		return "knowledge"
	case GenPhaseCritic, GenPhaseFixLoop:
		return "critic"
	default:
		return "plan"
	}
}

// recordClaudeCost records a finished Claude call's usage. With no tracker bound
// yet (first-time discovery / plan generation before missionDir exists) it
// buffers the record for a later flush by bindCostTracker.
func (m *Model) recordClaudeCost(msg ClaudeStreamMsg, role string) {
	rec := CostRecord{
		Model:               msg.Model,
		Role:                role,
		Phase:               -1,
		InputTokens:         msg.InputTokens,
		OutputTokens:        msg.OutputTokens,
		CacheCreationTokens: msg.CacheCreationTokens,
		CacheReadTokens:     msg.CacheReadTokens,
		CostUSD:             msg.CostUSD,
	}
	if rec.CostUSD == 0 && rec.InputTokens == 0 && rec.OutputTokens == 0 &&
		rec.CacheCreationTokens == 0 && rec.CacheReadTokens == 0 {
		return
	}
	if m.costTracker != nil {
		m.costTracker.Record(rec)
		return
	}
	rec.Ts = time.Now().UTC().Format(time.RFC3339)
	m.pendingCostRecords = append(m.pendingCostRecords, rec)
}

func (m Model) handleClaudeStream(msg ClaudeStreamMsg) (tea.Model, tea.Cmd) {
	if msg.SessionID != "" {
		m.claudeSessionID = msg.SessionID
	}
	if msg.Done {
		m.recordClaudeCost(msg, m.currentClaudeRole())
	}

	if msg.Err != nil {
		if m.changesMode && m.phase == PhaseRunning && m.genPhase != GenPhaseNone && m.changesTimeoutSent {
			m.changesTimeoutSent = false
			phaseIdx := m.changesPhaseIndex()
			if m.changesTimeouts < changesPhaseTimeoutMaxRetries {
				m.changesTimeouts++
				m.changesPromptTight = true
				m.claudeRetries = 0
				m.coverageRetries = 0
				m.claudeSessionID = ""
				m.claudeResumeHint = ""
				m.claudeRunning = true
				m.claudeStartTime = time.Now()
				m.streamLines = nil
				m.resetChangesPhaseBudgetTracking()
				m.discoveryMsgs = append(m.discoveryMsgs, ChatMessage{
					Role: "system",
					Text: fmt.Sprintf("PHASE_FALLBACK_CHANGES:%d:restarting with tighter prompt budget", phaseIdx),
				})
				m.viewport.SetContent(m.renderChatContent())
				m.viewport.GotoBottom()

				specPath := ""
				if m.activeSpec != nil {
					specPath = m.activeSpec.SpecPath
				}
				if specPath == "" {
					m.claudeRunning = false
					m.genPhase = GenPhaseNone
					m.discoveryMsgs = append(m.discoveryMsgs, ChatMessage{Role: "system", Text: "✕ Changes timeout fallback failed: missing spec path"})
					m.viewport.SetContent(m.renderChatContent())
					m.viewport.GotoBottom()
					return m, sendNotification("Quest", "Changes mode aborted: missing spec path")
				}
				mDir := ResolveArtifactDir(specPath)
				switch m.genPhase {
				case GenPhaseAnalysis:
					return m, m.spawnChangesAnalysisCall(specPath, mDir, m.projectDir, m.verbose)
				case GenPhaseAssertions:
					return m, m.spawnChangesAssertionsCall(specPath, mDir, m.projectDir, m.verbose, "Previous attempt exceeded the 5-minute budget. Emit the minimum safe delta.")
				case GenPhaseFeatures:
					return m, m.spawnChangesFeaturesCall(specPath, mDir, m.projectDir, m.assertionIDs, m.verbose, "Previous attempt exceeded the 5-minute budget. Emit the minimum safe delta.")
				}
			}

			m.claudeRunning = false
			m.genPhase = GenPhaseNone
			m.discoveryMsgs = append(m.discoveryMsgs, ChatMessage{
				Role: "system",
				Text: fmt.Sprintf("✕ Changes phase %d exceeded the 5m budget after fallback — aborting", phaseIdx),
			})
			m.viewport.SetContent(m.renderChatContent())
			m.viewport.GotoBottom()
			return m, sendNotification("Quest", fmt.Sprintf("Changes phase %d exceeded 5m budget", phaseIdx))
		}

		maxRetries := 3
		if m.phase == PhaseRunning && m.genPhase != GenPhaseNone {
			maxRetries = 5
		}
		m.claudeRetries++
		m.claudeResumeHint = "An error interrupted your work. Continue from where you left off and complete the task."
		if m.claudeRetries <= maxRetries && (m.lastPrompt != "" || m.claudeSessionID != "") {
			resumeLabel := ""
			if m.claudeSessionID != "" {
				resumeLabel = " (resuming session)"
			}
			retryMsg := fmt.Sprintf("⚠ %s — retrying (%d/%d)%s...", msg.Err, m.claudeRetries, maxRetries, resumeLabel)
			switch m.phase {
			case PhaseDiscovery:
				m.discoveryMsgs = append(m.discoveryMsgs, ChatMessage{Role: "system", Text: retryMsg})
			case PhaseRunning:
				m.discoveryMsgs = append(m.discoveryMsgs, ChatMessage{Role: "system", Text: retryMsg})
			case PhaseReview:
				m.reviewChat = append(m.reviewChat, ChatMessage{Role: "system", Text: retryMsg})
			}
			return m.retryClaudeWithDelay()
		}

		m.claudeRunning = false
		m.genPhase = GenPhaseNone
		errMsg := fmt.Sprintf("✕ Failed after %d retries: %s", m.claudeRetries, msg.Err)
		var notifyCmd tea.Cmd
		switch m.phase {
		case PhaseDiscovery:
			m.discoveryMsgs = append(m.discoveryMsgs, ChatMessage{Role: "system", Text: errMsg})
			m.input.Focus()
		case PhaseRunning:
			m.discoveryMsgs = append(m.discoveryMsgs, ChatMessage{Role: "system", Text: errMsg})
			notifyCmd = sendNotification("Quest", fmt.Sprintf("Generation failed: %s", msg.Err))
		case PhaseReview:
			m.reviewChat = append(m.reviewChat, ChatMessage{Role: "system", Text: errMsg})
			m.isRefining = false
			m.reviewInput.Focus()
		}
		m.viewport.SetContent(m.renderChatContent())
		m.viewport.GotoBottom()
		return m, notifyCmd
	}

	if msg.Done {
		m.claudeRunning = false

		if len(m.streamLines) > 0 {
			m.discoveryMsgs = append(m.discoveryMsgs, ChatMessage{Role: "assistant", Text: strings.Join(m.streamLines, "\n")})
		}
		m.streamLines = nil

		switch m.phase {
		case PhaseDiscovery:
			if msg.Result != "" {
				m.discoveryMsgs = append(m.discoveryMsgs, ChatMessage{Role: "assistant", Text: msg.Result})
			}
			m.input.Focus()
			m.viewport.SetContent(m.renderChatContent())
			m.viewport.GotoBottom()

		case PhaseRunning:
			if m.genPhase != GenPhaseNone {
				return m.nextGenPhase(msg.Result)
			}

			plan := ParsePlanFromText(msg.Result)

			if plan == nil || len(plan.Features) == 0 {
				if m.editingSpec && m.activeSpec != nil {
					mDir := ResolveArtifactDir(m.activeSpec.SpecPath)
					state := ReadMissionState(mDir)
					if state.Exists && len(state.Features) > 0 {
						m.missionDir = mDir
						m.mission = state
						m.editingSpec = false
						m.phase = PhaseReview
						m.reviewTab = ReviewTabChat
						m.reviewInput.Focus()
						m.discoveryMsgs = append(m.discoveryMsgs, ChatMessage{Role: "system", Text: fmt.Sprintf("✓ Quest recovered from disk — %d features", len(state.Features))})
						m.updateReviewContent()
						m.viewport.SetContent(m.renderChatContent())
						m.viewport.GotoBottom()
						return m, nil
					}
				}
			}

			if plan != nil && len(plan.Features) > 0 {
				var specDir, missionDir string
				if m.editingSpec && m.activeSpec != nil {
					specDir = m.activeSpec.SpecPath
					missionDir = ResolveArtifactDir(specDir)
				} else {
					specDir = filepath.Join(m.projectDir, "docs", "specs", plan.Slug)
					missionDir = ResolveArtifactDir(specDir)
				}
				knowledgePath := filepath.Join(missionDir, "knowledge-base.md")
				existingKnowledge := ""
				if m.regenMode {
					existingKnowledge = readFileContent(knowledgePath)
				}
				_ = WriteMissionFiles(specDir, m.projectDir, *plan)
				if m.regenMode && existingKnowledge != "" && len(plan.Knowledge) == 0 {
					_ = os.WriteFile(knowledgePath, []byte(existingKnowledge), 0o644)
				}
				m.missionDir = missionDir
				m.activeSpec = &SpecEntry{Slug: plan.Slug, SpecPath: specDir}
				m.mission = ReadMissionState(missionDir)
				if m.fullRegenMode {
					elapsed := time.Since(m.claudeStartTime).Round(time.Second)
					totalAssertions := 0
					for _, a := range plan.Assertions {
						totalAssertions += len(a.Items)
					}
					m.discoveryMsgs = append(m.discoveryMsgs, ChatMessage{
						Role: "system",
						Text: fmt.Sprintf("PHASE_DONE_FULL:1:%s:%d features, %d assertions refreshed", elapsed, len(plan.Features), totalAssertions),
					})
					m.editingSpec = false
					m.streamLines = nil
					m.claudeRetries = 0
					m.claudeSessionID = ""
					m.claudeResumeHint = ""
					m.viewport.SetContent(m.renderChatContent())
					m.viewport.GotoBottom()
					return m.startCriticLoop()
				}
				if m.regenMode {
					elapsed := time.Since(m.claudeStartTime).Round(time.Second)
					totalAssertions := 0
					for _, a := range plan.Assertions {
						totalAssertions += len(a.Items)
					}
					m.discoveryMsgs = append(m.discoveryMsgs, ChatMessage{
						Role: "system",
						Text: fmt.Sprintf("PHASE_DONE_REGEN:1:%s:%d features, %d assertions refreshed", elapsed, len(plan.Features), totalAssertions),
					})
					m.editingSpec = false
					m.streamLines = nil
					m.claudeRetries = 0
					m.claudeSessionID = ""
					m.claudeResumeHint = ""
					m.viewport.SetContent(m.renderChatContent())
					m.viewport.GotoBottom()
					return m.startCriticLoop()
				}
				m.editingSpec = false
				m.phase = PhaseReview
				m.reviewTab = ReviewTabChat
				m.reviewInput.Focus()
				m.updateReviewContent()
			} else {
				const maxRetries = 3
				m.claudeRetries++
				m.claudeResumeHint = "Your previous output could not be parsed as valid JSON. Output ONLY the complete JSON plan object — no markdown, no code fences, no explanation."
				if m.claudeRetries <= maxRetries && (m.lastPrompt != "" || m.claudeSessionID != "") {
					retryMsg := fmt.Sprintf("⚠ Plan output unparseable — retrying (%d/%d)...", m.claudeRetries, maxRetries)
					if m.claudeSessionID != "" {
						retryMsg += " (resuming session)"
					}
					m.discoveryMsgs = append(m.discoveryMsgs, ChatMessage{Role: "system", Text: retryMsg})
					m.viewport.SetContent(m.renderChatContent())
					m.viewport.GotoBottom()
					return m.retryClaudeWithDelay()
				}
				m.discoveryMsgs = append(m.discoveryMsgs, ChatMessage{Role: "system", Text: fmt.Sprintf("✕ Could not parse plan after %d attempts.", m.claudeRetries)})
				m.viewport.SetContent(m.renderChatContent())
				m.viewport.GotoBottom()
				return m, sendNotification("Quest", fmt.Sprintf("Failed to parse plan after %d attempts", m.claudeRetries))
			}
			m.viewport.SetContent(m.renderChatContent())
			m.viewport.GotoBottom()

		case PhaseReview:
			if m.isRefining && msg.Result != "" {
				plan := ParsePlanFromText(msg.Result)
				if plan != nil && len(plan.Features) > 0 {
					specDir := filepath.Dir(m.missionDir)
					_ = WriteMissionFiles(specDir, m.projectDir, *plan)
					m.mission = ReadMissionState(m.missionDir)
					m.reviewChat = append(m.reviewChat, ChatMessage{Role: "system", Text: fmt.Sprintf("Updated — %d features", len(plan.Features))})
				} else {
					const maxRetries = 3
					m.claudeRetries++
					if m.claudeRetries <= maxRetries && m.lastPrompt != "" {
						retryMsg := fmt.Sprintf("⚠ Updated plan unparseable — retrying (%d/%d)...", m.claudeRetries, maxRetries)
						m.reviewChat = append(m.reviewChat, ChatMessage{Role: "system", Text: retryMsg})
						m.viewport.SetContent(m.renderChatContent())
						m.viewport.GotoBottom()
						return m.retryClaudeWithDelay()
					}
					m.reviewChat = append(m.reviewChat, ChatMessage{Role: "system", Text: fmt.Sprintf("Could not parse updated plan after %d attempts.", m.claudeRetries)})
				}
				m.isRefining = false
				m.reviewInput.Focus()
				m.updateReviewContent()
			}
		}

		return m, nil
	}

	if msg.Line != "" {
		m.streamLines = append(m.streamLines, msg.Line)
		limit := 200
		if m.verbose {
			limit = 5000
		}
		if len(m.streamLines) > limit {
			m.streamLines = m.streamLines[len(m.streamLines)-limit:]
		}
		m.viewport.SetContent(m.renderChatContent())
		m.viewport.GotoBottom()
	}

	return m, listenClaude(m.claudeCh)
}

// --- Worker event handler ---

func (m Model) handleWorkerEvent(ev WorkerEvent) (tea.Model, tea.Cmd) {
	if ev.Line != "" {
		prefix := "[ORCH]"
		switch {
		case ev.Role == "critic":
			prefix = "[CRITIC]"
		case ev.Role == "critic-fix":
			prefix = "[AUTOFIX]"
		case ev.Role == "validator" && ev.FeatureID != "":
			prefix = fmt.Sprintf("[VALIDATOR:%s]", ev.FeatureID)
		case ev.Role == "refinement" && ev.FeatureID != "":
			prefix = fmt.Sprintf("[REFINE:%s]", ev.FeatureID)
		case ev.FeatureID != "":
			prefix = fmt.Sprintf("[%s]", ev.FeatureID)
		}
		m.workerLogs = append(m.workerLogs, fmt.Sprintf("%s %s", prefix, ev.Line))
		if len(m.workerLogs) > 10000 {
			m.workerLogs = m.workerLogs[len(m.workerLogs)-10000:]
		}
	}

	if ev.Done && ev.Role == "critic" && ev.Verdict == "FAIL" && ev.CriticReport != nil {
		m.criticFailReport = ev.CriticReport
		m.criticMenuCursor = 0
	}

	m.mission = ReadMissionState(m.missionDir)
	m.syncMissionWithPool()

	if ev.AllDone {
		m.executing = false
		if m.logger != nil {
			m.logger.Close()
		}
		if m.criticFailReport != nil {
			m.activeTab = TabOverview
			m.updateDashboardContent()
			return m, nil
		}
		m.updateDashboardContent()
		stats := m.mission.Stats
		return m, sendNotification(
			"Mission",
			fmt.Sprintf(
				"Execution complete — %d/%d done (%d via fix, %d waived), %d blocked unresolved, %d tainted",
				stats.Done, stats.Total, stats.DoneViaFix, stats.DoneWaived, stats.BlockedUnresolved, stats.BlockedTainted,
			),
		)
	}

	// Newest log lines render at the top; keep the view pinned there while the
	// user is already at the top, but don't yank them back if they scrolled
	// down to read older output.
	wasAtTop := m.viewport.AtTop()
	m.updateDashboardContent()
	if m.activeTab == TabLog && wasAtTop {
		m.viewport.GotoTop()
	}
	return m, listenWorker(m.workerPool.eventCh)
}

func (m *Model) syncMissionWithPool() {
	if m.workerPool == nil || !m.executing {
		return
	}
	workers := m.workerPool.GetWorkerStatuses()
	statusMap := make(map[string]string)
	for _, w := range workers {
		switch w.Status {
		case WorkerRunning:
			statusMap[w.Feature.ID] = "in_progress"
		case WorkerAwaitingValidation:
			statusMap[w.Feature.ID] = "awaiting_validation"
		case WorkerValidating:
			statusMap[w.Feature.ID] = "validating"
		case WorkerRefining:
			statusMap[w.Feature.ID] = "refining"
		case WorkerDone:
			statusMap[w.Feature.ID] = "done"
		case WorkerFailed:
			statusMap[w.Feature.ID] = "blocked"
		}
	}
	var stats MissionStats
	stats.Total = len(m.mission.Features)
	for i := range m.mission.Features {
		if s, ok := statusMap[m.mission.Features[i].ID]; ok {
			m.mission.Features[i].Status = s
		}
	}

	tainted := m.workerPool.GetTaintedStatuses()
	outcomes := buildFeatureOutcomes(m.mission.Features, tainted)

	for i := range m.mission.Features {
		if out, ok := outcomes[m.mission.Features[i].ID]; ok {
			m.mission.Features[i].Resolution = out.Resolution
			if out.ResolvedBy != "" {
				m.mission.Features[i].ResolvedBy = out.ResolvedBy
			}
			m.mission.Features[i].Tainted = out.Tainted
		}
		switch m.mission.Features[i].Status {
		case "done", "validated":
			stats.DoneDirect++
		case "in_progress":
			stats.InProgress++
		case "blocked":
			stats.Blocked++
			out := outcomes[m.mission.Features[i].ID]
			switch out.Resolution {
			case ResolutionResolvedViaFix:
				stats.DoneViaFix++
				stats.BlockedResolved++
			case ResolutionResolvedTainted:
				stats.DoneViaFix++
				stats.BlockedResolved++
				stats.BlockedTainted++
			case ResolutionWaivedContract:
				stats.DoneWaived++
				stats.BlockedResolved++
				stats.BlockedWaived++
			default:
				stats.BlockedUnresolved++
			}
		case "pending", "":
			stats.Pending++
		case "awaiting_validation":
			stats.AwaitingValidation++
		case "validating":
			stats.Validating++
		case "refining":
			stats.Refining++
		}
	}
	stats.Done = stats.DoneDirect + stats.DoneViaFix + stats.DoneWaived
	m.mission.Stats = stats
}

// --- View rendering ---

func (m Model) chatView() string {
	w := m.width - margin*2

	// Header
	header := "\n" + m.styles.Title.Render("Quest Control")
	if m.claudeRunning {
		header += " " + m.spinner.View()
	}
	header += "\n"

	sep := m.styles.Separator.Render(strings.Repeat("─", w))

	chat := m.viewport.View()

	var input string
	showInput := m.phase == PhaseDiscovery && !m.claudeRunning
	if showInput {
		input = m.input.View()
	} else if m.claudeRunning {
		input = m.styles.Dim.Render("  Analyzing...")
	} else {
		input = m.styles.Dim.Render("  Waiting...")
	}

	vLabel := "V: verbose"
	if m.verbose {
		vLabel = "V: summary"
	}

	hasMessages := len(m.discoveryMsgs) > 0

	var hint string
	switch m.phase {
	case PhaseDiscovery:
		if m.claudeRunning {
			hint = fmt.Sprintf("  %s · ↑↓/scroll · Esc: cancel", vLabel)
		} else if hasMessages {
			hint = "  Enter: approve · Shift+Enter: newline · Ctrl+E: editor · Esc: clear"
		} else {
			hint = "  Enter: send · Shift+Enter: newline · Ctrl+E: editor · Esc: quit"
		}
	case PhaseRunning:
		if m.claudeRunning {
			hint = fmt.Sprintf("  %s · ↑↓/scroll · Esc: cancel", vLabel)
		} else {
			hint = "  R: retry · Esc: go back"
		}
	}
	hint = m.styles.Hint.Render(hint)

	pad := lipgloss.NewStyle().PaddingLeft(margin).PaddingRight(margin)
	return pad.Render(lipgloss.JoinVertical(lipgloss.Left,
		header, sep, chat, sep, input, hint,
	))
}

func (m Model) renderChatContent() string {
	var sb strings.Builder
	contentW := m.width - margin*2 - 6

	for i, msg := range m.discoveryMsgs {
		if i > 0 {
			sb.WriteString("\n")
		}

		switch msg.Role {
		case "user":
			sb.WriteString(m.styles.UserLabel.Render("  you"))
			sb.WriteString("\n")
			wrapped := lipgloss.NewStyle().Width(contentW).Render(msg.Text)
			for _, line := range strings.Split(wrapped, "\n") {
				sb.WriteString("    " + line + "\n")
			}

		case "assistant":
			sb.WriteString(m.styles.ClaudeLabel.Render("  claude"))
			sb.WriteString("\n")
			wrapW := contentW - 4
			lastBlank := false
			for _, line := range strings.Split(msg.Text, "\n") {
				if strings.TrimSpace(line) == "" {
					if !lastBlank {
						sb.WriteString("\n")
						lastBlank = true
					}
					continue
				}
				lastBlank = false
				isHeader := strings.HasPrefix(line, "#")
				wrapped := lipgloss.NewStyle().Width(wrapW).Render(line)
				for _, wl := range strings.Split(wrapped, "\n") {
					if isHeader {
						sb.WriteString("    " + m.styles.Cyan.Render(wl) + "\n")
					} else {
						sb.WriteString("    " + wl + "\n")
					}
				}
			}

		case "system":
			sb.WriteString(m.renderSystemMsg(msg.Text))
		}
	}

	if len(m.streamLines) > 0 {
		lastIsAssistant := len(m.discoveryMsgs) > 0 && m.discoveryMsgs[len(m.discoveryMsgs)-1].Role == "assistant"
		if !lastIsAssistant {
			sb.WriteString("\n")
			modelName := m.genPhaseLabel()
			label := "  " + modelName
			if m.claudeRunning && !m.claudeStartTime.IsZero() {
				elapsed := time.Since(m.claudeStartTime).Round(time.Second)
				toolCalls := 0
				for _, l := range m.streamLines {
					if strings.HasPrefix(l, "▸") {
						toolCalls++
					}
				}
				label += fmt.Sprintf("  %s · %d tools", elapsed, toolCalls)
			}
			sb.WriteString(m.styles.ClaudeLabel.Render(label))
			sb.WriteString("\n")
		}
		for _, line := range m.streamLines {
			styled := line
			if strings.HasPrefix(line, "▸") {
				styled = m.styles.Cyan.Render(line)
			} else if strings.HasPrefix(line, "←") || strings.HasPrefix(line, "  ←") {
				styled = m.styles.Dim.Render(line)
			} else if strings.HasPrefix(line, "✓") {
				styled = m.styles.Green.Render(line)
			} else if strings.HasPrefix(line, "◆") || strings.HasPrefix(line, "Session") {
				styled = m.styles.Dim.Render(line)
			}
			sb.WriteString("    " + styled + "\n")
		}
	} else if m.claudeRunning && (len(m.discoveryMsgs) == 0 || m.discoveryMsgs[len(m.discoveryMsgs)-1].Role != "assistant") {
		sb.WriteString("\n")
		sb.WriteString(m.styles.ClaudeLabel.Render("  " + m.genPhaseLabel()))
		sb.WriteString("\n")
		sb.WriteString("    " + m.styles.Dim.Render("Starting session...") + "\n")
	}

	return sb.String()
}

var phaseInfo = [6]struct{ model, desc string }{
	{"sonnet", "Codebase analysis"},
	{"opus", "Spec → assertions"},
	{"opus", "Spec → features"},
	{"sonnet", "Spec → knowledge"},
	{"sonnet", "Critic validation"},
	{"sonnet", "Auto-fix"},
}

var regenPhaseInfo = [4]struct{ model, desc string }{
	{"opus", "Spec → assertions (regen)"},
	{"opus", "Spec → features (preserve completed)"},
	{"sonnet", "Critic validation"},
	{"sonnet", "Auto-fix"},
}

var fullRegenPhaseInfo = [3]struct{ model, desc string }{
	{"opus", "Full regenerate plan"},
	{"sonnet", "Critic validation"},
	{"sonnet", "Auto-fix"},
}

var changesPhaseInfo = [5]struct{ model, desc string }{
	{"sonnet", "Analyze spec changes"},
	{"opus", "Assertions delta"},
	{"opus", "Features delta"},
	{"sonnet", "Critic validation"},
	{"sonnet", "Auto-fix"},
}

func (m Model) renderSystemMsg(text string) string {
	if text == "" {
		return "\n"
	}

	if text == "PHASE_ROADMAP_REGEN" {
		var sb strings.Builder
		sb.WriteString("  " + m.styles.Dim.Render("─── Regeneration Pipeline ───────────────────") + "\n")
		for i, p := range regenPhaseInfo {
			num := fmt.Sprintf("  %d ", i+1)
			model := fmt.Sprintf("%-6s", p.model)
			sb.WriteString(m.styles.Blue.Render(num) + m.styles.Magenta.Render(model) + " " + m.styles.Dim.Render(p.desc) + "\n")
		}
		sb.WriteString("  " + m.styles.Dim.Render("─────────────────────────────────────────────") + "\n")
		return sb.String()
	}

	if text == "PHASE_ROADMAP_FULL" {
		var sb strings.Builder
		sb.WriteString("  " + m.styles.Dim.Render("─── Full Regeneration Pipeline ──────────────") + "\n")
		for i, p := range fullRegenPhaseInfo {
			num := fmt.Sprintf("  %d ", i+1)
			model := fmt.Sprintf("%-6s", p.model)
			sb.WriteString(m.styles.Blue.Render(num) + m.styles.Magenta.Render(model) + " " + m.styles.Dim.Render(p.desc) + "\n")
		}
		sb.WriteString("  " + m.styles.Dim.Render("─────────────────────────────────────────────") + "\n")
		return sb.String()
	}

	if text == "PHASE_ROADMAP_CHANGES" {
		var sb strings.Builder
		sb.WriteString("  " + m.styles.Dim.Render("─── Changes Pipeline ────────────────────────") + "\n")
		for i, p := range changesPhaseInfo {
			num := fmt.Sprintf("  %d ", i+1)
			model := fmt.Sprintf("%-6s", p.model)
			sb.WriteString(m.styles.Blue.Render(num) + m.styles.Magenta.Render(model) + " " + m.styles.Dim.Render(p.desc) + "\n")
		}
		sb.WriteString("  " + m.styles.Dim.Render("─────────────────────────────────────────────") + "\n")
		return sb.String()
	}

	if strings.HasPrefix(text, "PHASE_START_REGEN:") {
		idx := 0
		fmt.Sscanf(text, "PHASE_START_REGEN:%d", &idx)
		if idx >= 1 && idx <= len(regenPhaseInfo) {
			p := regenPhaseInfo[idx-1]
			return "\n" +
				m.styles.Blue.Render(fmt.Sprintf("  ▶ Phase %d/%d", idx, len(regenPhaseInfo))) + " " +
				m.styles.Magenta.Render(p.model) + " " +
				m.styles.Dim.Render("— "+p.desc+"...") + "\n"
		}
	}

	if strings.HasPrefix(text, "PHASE_DONE_REGEN:") {
		parts := strings.SplitN(text, ":", 4)
		if len(parts) == 4 {
			idx := 0
			fmt.Sscanf(parts[1], "%d", &idx)
			elapsed := parts[2]
			detail := parts[3]
			if idx >= 1 && idx <= len(regenPhaseInfo) {
				p := regenPhaseInfo[idx-1]
				return m.styles.Green.Render(fmt.Sprintf("  ✓ Phase %d/%d", idx, len(regenPhaseInfo))) + " " +
					m.styles.Magenta.Render(p.model) + " " +
					m.styles.Dim.Render("— "+p.desc) + " " +
					m.styles.Yellow.Render(elapsed) + " " +
					m.styles.Dim.Render("("+detail+")") + "\n"
			}
		}
	}

	if strings.HasPrefix(text, "PHASE_START_FULL:") {
		idx := 0
		fmt.Sscanf(text, "PHASE_START_FULL:%d", &idx)
		if idx >= 1 && idx <= len(fullRegenPhaseInfo) {
			p := fullRegenPhaseInfo[idx-1]
			return "\n" +
				m.styles.Blue.Render(fmt.Sprintf("  ▶ Phase %d/%d", idx, len(fullRegenPhaseInfo))) + " " +
				m.styles.Magenta.Render(p.model) + " " +
				m.styles.Dim.Render("— "+p.desc+"...") + "\n"
		}
	}

	if strings.HasPrefix(text, "PHASE_DONE_FULL:") {
		parts := strings.SplitN(text, ":", 4)
		if len(parts) == 4 {
			idx := 0
			fmt.Sscanf(parts[1], "%d", &idx)
			elapsed := parts[2]
			detail := parts[3]
			if idx >= 1 && idx <= len(fullRegenPhaseInfo) {
				p := fullRegenPhaseInfo[idx-1]
				return m.styles.Green.Render(fmt.Sprintf("  ✓ Phase %d/%d", idx, len(fullRegenPhaseInfo))) + " " +
					m.styles.Magenta.Render(p.model) + " " +
					m.styles.Dim.Render("— "+p.desc) + " " +
					m.styles.Yellow.Render(elapsed) + " " +
					m.styles.Dim.Render("("+detail+")") + "\n"
			}
		}
	}

	if strings.HasPrefix(text, "PHASE_START_CHANGES:") {
		idx := 0
		fmt.Sscanf(text, "PHASE_START_CHANGES:%d", &idx)
		if idx >= 1 && idx <= len(changesPhaseInfo) {
			p := changesPhaseInfo[idx-1]
			return "\n" +
				m.styles.Blue.Render(fmt.Sprintf("  ▶ Phase %d/%d", idx, len(changesPhaseInfo))) + " " +
				m.styles.Magenta.Render(p.model) + " " +
				m.styles.Dim.Render("— "+p.desc+"...") + "\n"
		}
	}

	if strings.HasPrefix(text, "PHASE_WARN_CHANGES:") {
		parts := strings.SplitN(text, ":", 4)
		if len(parts) == 4 {
			idx := 0
			fmt.Sscanf(parts[1], "%d", &idx)
			elapsed := parts[2]
			detail := parts[3]
			if idx >= 1 && idx <= len(changesPhaseInfo) {
				p := changesPhaseInfo[idx-1]
				return m.styles.Yellow.Render(fmt.Sprintf("  ⚠ Phase %d/%d", idx, len(changesPhaseInfo))) + " " +
					m.styles.Magenta.Render(p.model) + " " +
					m.styles.Dim.Render("— "+detail) + " " +
					m.styles.Yellow.Render(elapsed) + "\n"
			}
		}
	}

	if strings.HasPrefix(text, "PHASE_TIMEOUT_CHANGES:") {
		parts := strings.SplitN(text, ":", 4)
		if len(parts) == 4 {
			idx := 0
			fmt.Sscanf(parts[1], "%d", &idx)
			elapsed := parts[2]
			detail := parts[3]
			if idx >= 1 && idx <= len(changesPhaseInfo) {
				p := changesPhaseInfo[idx-1]
				return m.styles.Red.Render(fmt.Sprintf("  ✕ Phase %d/%d", idx, len(changesPhaseInfo))) + " " +
					m.styles.Magenta.Render(p.model) + " " +
					m.styles.Dim.Render("— "+detail) + " " +
					m.styles.Yellow.Render(elapsed) + "\n"
			}
		}
	}

	if strings.HasPrefix(text, "PHASE_FALLBACK_CHANGES:") {
		parts := strings.SplitN(text, ":", 3)
		if len(parts) == 3 {
			idx := 0
			fmt.Sscanf(parts[1], "%d", &idx)
			detail := parts[2]
			if idx >= 1 && idx <= len(changesPhaseInfo) {
				p := changesPhaseInfo[idx-1]
				return m.styles.Cyan.Render(fmt.Sprintf("  ↺ Phase %d/%d", idx, len(changesPhaseInfo))) + " " +
					m.styles.Magenta.Render(p.model) + " " +
					m.styles.Dim.Render("— "+detail) + "\n"
			}
		}
	}

	if strings.HasPrefix(text, "PHASE_DONE_CHANGES:") {
		parts := strings.SplitN(text, ":", 4)
		if len(parts) == 4 {
			idx := 0
			fmt.Sscanf(parts[1], "%d", &idx)
			elapsed := parts[2]
			detail := parts[3]
			if idx >= 1 && idx <= len(changesPhaseInfo) {
				p := changesPhaseInfo[idx-1]
				return m.styles.Green.Render(fmt.Sprintf("  ✓ Phase %d/%d", idx, len(changesPhaseInfo))) + " " +
					m.styles.Magenta.Render(p.model) + " " +
					m.styles.Dim.Render("— "+p.desc) + " " +
					m.styles.Yellow.Render(elapsed) + " " +
					m.styles.Dim.Render("("+detail+")") + "\n"
			}
		}
	}

	if text == "PHASE_ROADMAP" {
		var sb strings.Builder
		sb.WriteString("  " + m.styles.Dim.Render("─── Pipeline ───────────────────────────────") + "\n")
		for i, p := range phaseInfo {
			num := fmt.Sprintf("  %d ", i+1)
			model := fmt.Sprintf("%-6s", p.model)
			sb.WriteString(m.styles.Blue.Render(num) + m.styles.Magenta.Render(model) + " " + m.styles.Dim.Render(p.desc) + "\n")
		}
		sb.WriteString("  " + m.styles.Dim.Render("─────────────────────────────────────────────") + "\n")
		return sb.String()
	}

	if strings.HasPrefix(text, "PHASE_START:") {
		idx := 0
		fmt.Sscanf(text, "PHASE_START:%d", &idx)
		if idx >= 1 && idx <= len(phaseInfo) {
			p := phaseInfo[idx-1]
			return "\n" +
				m.styles.Blue.Render(fmt.Sprintf("  ▶ Phase %d/%d", idx, len(phaseInfo))) + " " +
				m.styles.Magenta.Render(p.model) + " " +
				m.styles.Dim.Render("— "+p.desc+"...") + "\n"
		}
	}

	if strings.HasPrefix(text, "PHASE_DONE:") {
		parts := strings.SplitN(text, ":", 4)
		if len(parts) == 4 {
			idx := 0
			fmt.Sscanf(parts[1], "%d", &idx)
			elapsed := parts[2]
			detail := parts[3]
			if idx >= 1 && idx <= len(phaseInfo) {
				p := phaseInfo[idx-1]
				return m.styles.Green.Render(fmt.Sprintf("  ✓ Phase %d/%d", idx, len(phaseInfo))) + " " +
					m.styles.Magenta.Render(p.model) + " " +
					m.styles.Dim.Render("— "+p.desc) + " " +
					m.styles.Yellow.Render(elapsed) + " " +
					m.styles.Dim.Render("("+detail+")") + "\n"
			}
		}
	}

	if strings.HasPrefix(text, "GEN_COMPLETE:") {
		parts := strings.SplitN(text, ":", 3)
		if len(parts) == 3 {
			elapsed := parts[1]
			detail := parts[2]
			return "\n" +
				m.styles.Green.Render("  ✓ Quest plan ready") + " " +
				m.styles.Yellow.Render(elapsed) + " " +
				m.styles.Dim.Render("— "+detail) + "\n"
		}
	}

	return m.styles.SystemText.Render("  "+text) + "\n"
}

func (m Model) reviewView() string {
	w := m.width - margin*2

	header := "\n" + m.styles.Title.Render("Review Plan")
	if m.isRefining {
		header += " " + m.spinner.View()
	}
	header += "\n"

	tabs := []struct {
		name string
		tab  ReviewTab
	}{
		{"Chat", ReviewTabChat},
		{"Plan", ReviewTabPlan},
		{"Spec", ReviewTabSpec},
		{"Contract", ReviewTabContract},
		{"Critic", ReviewTabCritic},
	}
	var tabParts []string
	for _, t := range tabs {
		if t.tab == m.reviewTab {
			tabParts = append(tabParts, m.styles.TabActive.Render(t.name))
		} else {
			tabParts = append(tabParts, m.styles.TabInactive.Render(t.name))
		}
	}
	tabBar := "  " + strings.Join(tabParts, "  ")

	sep := m.styles.Separator.Render(strings.Repeat("─", w))

	content := m.viewport.View()

	var input string
	if m.isRefining {
		input = m.styles.Dim.Render("  Refining plan...")
	} else {
		input = m.reviewInput.View()
	}

	hintText := "  Enter: approve · Tab: switch · Shift+Enter: newline · Ctrl+E: editor · Esc: reject"
	if m.reviewTab == ReviewTabCritic && len(m.criticAdvisory) > 0 {
		hintText = "  ↑↓: navigate · Space: toggle · Enter: approve · Tab: switch · Esc: reject"
	}
	hint := m.styles.Hint.Render(hintText)

	pad := lipgloss.NewStyle().PaddingLeft(margin).PaddingRight(margin)
	return pad.Render(lipgloss.JoinVertical(lipgloss.Left,
		header, tabBar, sep, content, sep, input, hint,
	))
}

func (m *Model) updateReviewContent() {
	switch m.reviewTab {
	case ReviewTabChat:
		m.viewport.SetContent(m.renderChatContent())
	case ReviewTabPlan:
		m.viewport.SetContent(m.renderReviewPlan())
	case ReviewTabSpec:
		m.viewport.SetContent(m.renderReviewSpec())
	case ReviewTabContract:
		m.viewport.SetContent(m.renderReviewContract())
	case ReviewTabCritic:
		m.viewport.SetContent(m.renderReviewCritic())
	}
}

func (m Model) renderReviewPlan() string {
	var sb strings.Builder
	contentW := m.width - margin*2 - 10
	wrapStyle := lipgloss.NewStyle().Width(contentW)

	sb.WriteString(m.styles.Cyan.Render(fmt.Sprintf("  Project: %s | Owner: %s", m.mission.Project, m.mission.Owner)))
	sb.WriteString("\n")
	sb.WriteString(fmt.Sprintf("  %d features across %d phases\n\n",
		len(m.mission.Features), countPhases(m.mission.Features)))

	phaseNames := []string{"Foundation", "Core", "Polish", "Extras"}
	for phase := 0; phase < 4; phase++ {
		features := featuresInPhase(m.mission.Features, phase)
		if len(features) == 0 {
			continue
		}
		phaseName := "Unknown"
		if phase < len(phaseNames) {
			phaseName = phaseNames[phase]
		}
		sb.WriteString(m.styles.Cyan.Render(fmt.Sprintf("  Phase %d: %s", phase, phaseName)))
		sb.WriteString("\n")
		for _, f := range features {
			icon, style := statusDisplay(f.Status, m.styles)
			deps := ""
			if len(f.DependsOn) > 0 {
				deps = m.styles.Dim.Render(fmt.Sprintf(" → %s", strings.Join(f.DependsOn, ", ")))
			}
			sb.WriteString(fmt.Sprintf("    %s %s %s%s\n", style.Render(icon), f.ID, f.Title, deps))
			wrapped := wrapStyle.Render(f.Scope)
			for _, wl := range strings.Split(wrapped, "\n") {
				sb.WriteString(m.styles.Dim.Render(fmt.Sprintf("      %s", wl)) + "\n")
			}
			if strings.TrimSpace(f.Description) != "" {
				descWrapped := wrapStyle.Render("desc: " + f.Description)
				for _, wl := range strings.Split(descWrapped, "\n") {
					sb.WriteString(m.styles.Dim.Render(fmt.Sprintf("      %s", wl)) + "\n")
				}
			}
			if len(f.ValidationRefs) > 0 {
				sb.WriteString(m.styles.Dim.Render(fmt.Sprintf("      refs: %s\n", strings.Join(f.ValidationRefs, ", "))))
			}
		}
		sb.WriteString("\n")
	}

	for _, msg := range m.reviewChat {
		switch msg.Role {
		case "user":
			sb.WriteString("\n" + m.styles.UserLabel.Render("  you") + "\n")
			sb.WriteString("    " + msg.Text + "\n")
		case "system":
			sb.WriteString(m.styles.SystemText.Render("  "+msg.Text) + "\n")
		}
	}

	return sb.String()
}

func (m Model) renderReviewSpec() string {
	specPath := filepath.Join(filepath.Dir(m.missionDir), "spec.md")
	content := readFileContent(specPath)
	if content == "" {
		return m.styles.Dim.Render("  No spec.md generated yet.")
	}

	var sb strings.Builder
	contentW := m.width - margin*2 - 4
	wrapStyle := lipgloss.NewStyle().Width(contentW)

	for _, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(line, "#") {
			sb.WriteString("  " + m.styles.Cyan.Render(line) + "\n")
		} else if strings.TrimSpace(line) == "" {
			sb.WriteString("\n")
		} else {
			wrapped := wrapStyle.Render(line)
			for _, wl := range strings.Split(wrapped, "\n") {
				sb.WriteString("  " + wl + "\n")
			}
		}
	}

	return sb.String()
}

func (m Model) renderReviewContract() string {
	contractPath := filepath.Join(m.missionDir, "validation-contract.md")
	content := readFileContent(contractPath)
	if content == "" {
		return m.styles.Dim.Render("  No validation contract generated yet.")
	}

	var sb strings.Builder
	contentW := m.width - margin*2 - 4
	wrapStyle := lipgloss.NewStyle().Width(contentW)

	for _, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(line, "#") {
			sb.WriteString("  " + m.styles.Cyan.Render(line) + "\n")
		} else if strings.HasPrefix(strings.TrimSpace(line), "- **") {
			sb.WriteString("  " + m.styles.Green.Render(line) + "\n")
		} else if strings.TrimSpace(line) == "" {
			sb.WriteString("\n")
		} else {
			wrapped := wrapStyle.Render(line)
			for _, wl := range strings.Split(wrapped, "\n") {
				sb.WriteString("  " + wl + "\n")
			}
		}
	}

	return sb.String()
}

func (m Model) renderReviewCritic() string {
	var sb strings.Builder
	contentW := m.width - margin*2 - 4
	wrapStyle := lipgloss.NewStyle().Width(contentW)

	if len(m.criticBlocking) > 0 {
		sb.WriteString("\n")
		sb.WriteString(m.styles.Yellow.Render(fmt.Sprintf("  ⚠ %d blocking finding(s) could not be auto-fixed after %d attempts", len(m.criticBlocking), m.criticLoopCount)))
		sb.WriteString("\n\n")
		for _, f := range m.criticBlocking {
			sb.WriteString(m.styles.Red.Render(fmt.Sprintf("  ✕ [%s] %s", f.Criterion, f.Status)))
			sb.WriteString("\n")
			if f.Note != "" {
				wrapped := wrapStyle.Render(f.Note)
				for _, wl := range strings.Split(wrapped, "\n") {
					sb.WriteString(m.styles.Dim.Render("    "+wl) + "\n")
				}
			}
			if f.Suggestion != "" {
				wrapped := wrapStyle.Render("→ " + f.Suggestion)
				for _, wl := range strings.Split(wrapped, "\n") {
					sb.WriteString(m.styles.Dim.Render("    "+wl) + "\n")
				}
			}
			sb.WriteString("\n")
		}
		sb.WriteString(m.styles.Dim.Render("  You can approve anyway — workers will run without critic gate."))
		sb.WriteString("\n\n")
		sb.WriteString(m.styles.Separator.Render("  " + strings.Repeat("─", contentW-4)))
		sb.WriteString("\n\n")
	}

	if len(m.criticAdvisory) == 0 && len(m.criticBlocking) == 0 {
		sb.WriteString("\n")
		sb.WriteString(m.styles.Green.Render("  ✓ All critic checks passed"))
		sb.WriteString("\n\n")
		sb.WriteString(m.styles.Dim.Render("  No findings to review."))
		sb.WriteString("\n")
		return sb.String()
	}

	if len(m.criticAdvisory) > 0 {
		sb.WriteString("\n")
		sb.WriteString(m.styles.Cyan.Render(fmt.Sprintf("  Critic Opinions — %d advisory finding(s)", len(m.criticAdvisory))))
		sb.WriteString("\n\n")
		sb.WriteString(m.styles.Dim.Render("  Architecture-level suggestions. These don't block execution."))
		sb.WriteString("\n")
		sb.WriteString(m.styles.Dim.Render("  Select items you want fixed before starting workers."))
		sb.WriteString("\n\n")

		for i, f := range m.criticAdvisory {
			checkbox := "☐"
			if i < len(m.criticSelected) && m.criticSelected[i] {
				checkbox = "☑"
			}

			isCursor := i == m.criticCursor
			prefix := "  "
			if isCursor {
				prefix = "▸ "
			}

			label := fmt.Sprintf("%s%s [%s]", prefix, checkbox, f.Criterion)
			if isCursor {
				sb.WriteString(m.styles.Cyan.Render(label))
			} else {
				sb.WriteString(m.styles.Dim.Render(label))
			}
			sb.WriteString("\n")

			if f.Note != "" {
				wrapped := wrapStyle.Render(f.Note)
				for _, wl := range strings.Split(wrapped, "\n") {
					if isCursor {
						sb.WriteString("    " + wl + "\n")
					} else {
						sb.WriteString(m.styles.Dim.Render("    "+wl) + "\n")
					}
				}
			}
			if f.Suggestion != "" {
				wrapped := wrapStyle.Render("→ " + f.Suggestion)
				for _, wl := range strings.Split(wrapped, "\n") {
					sb.WriteString(m.styles.Dim.Render("    "+wl) + "\n")
				}
			}
			sb.WriteString("\n")
		}
	}

	return sb.String()
}

func (m *Model) updateDashboardContent() {
	m.bindCostTracker(m.missionDir)
	switch m.activeTab {
	case TabOverview:
		m.viewport.SetContent(m.renderOverviewTab())
	case TabKanban:
		m.viewport.SetContent(m.renderKanbanTab())
	case TabLog:
		m.viewport.SetContent(m.renderLogTab())
	case TabDiagram:
		m.viewport.SetContent(m.renderDiagramTab())
	case TabCost:
		m.viewport.SetContent(m.renderCostTab())
	}
}

func (m Model) renderOverviewTab() string {
	totalW := m.width - margin*2
	panelBorder := lipgloss.RoundedBorder()
	panelColor := lipgloss.Color("240")
	phaseNames := []string{"Foundation", "Core", "Polish", "Extras"}

	leftW := totalW*3/5 - 1
	rightW := totalW - leftW - 1
	leftInner := leftW - 4
	rightInner := rightW - 4

	if m.criticFailReport != nil {
		return m.renderCriticFailView(totalW)
	}

	if m.executing && m.workerPool != nil {
		return m.renderExecutingOverview(totalW, leftW, rightW, leftInner, rightInner, panelBorder, panelColor, phaseNames)
	}

	return m.renderStaticOverview(totalW, leftW, rightW, leftInner, rightInner, panelBorder, panelColor, phaseNames)
}

func (m Model) renderCriticFailView(totalW int) string {
	var sb strings.Builder
	report := m.criticFailReport

	nBlocking := len(report.BlockingFindings)
	header := fmt.Sprintf("  ✕ CRITIC GATE FAILED — %d blocking findings", nBlocking)
	sep := strings.Repeat("━", min(totalW-4, 60))

	sb.WriteString("\n")
	sb.WriteString(m.styles.Red.Bold(true).Render(sep) + "\n")
	sb.WriteString(m.styles.Red.Bold(true).Render(header) + "\n")
	sb.WriteString(m.styles.Red.Bold(true).Render(sep) + "\n")
	sb.WriteString("\n")

	wrapW := totalW - 14
	if wrapW < 40 {
		wrapW = 40
	}
	for _, f := range report.Findings {
		if f.Status != "needs-work" {
			continue
		}
		target := f.Target
		if target == "" {
			target = f.Note
		}
		criterion := m.styles.Yellow.Render(fmt.Sprintf("[%s]", f.Criterion))
		sb.WriteString(fmt.Sprintf("  %s %s\n", criterion, target))
		if f.Suggestion != "" {
			wrapped := lipgloss.NewStyle().Width(wrapW).Render(f.Suggestion)
			for _, line := range strings.Split(wrapped, "\n") {
				sb.WriteString(fmt.Sprintf("         %s\n", m.styles.Cyan.Render(line)))
			}
		}
		sb.WriteString("\n")
	}

	sb.WriteString("\n")

	if m.autoFixRunning {
		sb.WriteString(m.styles.Cyan.Render("  ⏳ Auto-fix agent running...") + "\n\n")
		start := 0
		if len(m.workerLogs) > 8 {
			start = len(m.workerLogs) - 8
		}
		for _, line := range m.workerLogs[start:] {
			sb.WriteString("  " + m.styles.Dim.Render(line) + "\n")
		}
	} else {
		menuOptions := []string{
			"Auto-fix suggestions",
			"Skip critic & start workers",
			"Cancel",
		}

		boxW := 36
		sb.WriteString("  " + m.styles.Dim.Render("┌"+strings.Repeat("─", boxW)+"┐") + "\n")
		for i, opt := range menuOptions {
			var line string
			if i == m.criticMenuCursor {
				line = fmt.Sprintf("  ▸ %s", opt)
				padded := line + strings.Repeat(" ", max(0, boxW-len([]rune(line))))
				sb.WriteString("  " + m.styles.Dim.Render("│") + m.styles.Cyan.Bold(true).Render(padded) + m.styles.Dim.Render("│") + "\n")
			} else {
				line = fmt.Sprintf("    %s", opt)
				padded := line + strings.Repeat(" ", max(0, boxW-len([]rune(line))))
				sb.WriteString("  " + m.styles.Dim.Render("│"+padded+"│") + "\n")
			}
		}
		sb.WriteString("  " + m.styles.Dim.Render("└"+strings.Repeat("─", boxW)+"┘") + "\n")
	}

	return sb.String()
}

func (m Model) renderExecutingOverview(totalW, leftW, rightW, leftInner, rightInner int, border lipgloss.Border, borderColor lipgloss.Color, phaseNames []string) string {
	var sb strings.Builder
	workers := m.workerPool.GetWorkerStatuses()

	var running, done, failed, pending, validating, refining int
	currentPhase, maxPhase := -1, 0
	for _, w := range workers {
		switch w.Status {
		case WorkerRunning:
			running++
			if w.Feature.Phase > currentPhase {
				currentPhase = w.Feature.Phase
			}
		case WorkerDone:
			done++
		case WorkerFailed:
			failed++
		case WorkerPending:
			pending++
		case WorkerAwaitingValidation, WorkerValidating:
			validating++
		case WorkerRefining:
			refining++
		}
		if w.Feature.Phase > maxPhase {
			maxPhase = w.Feature.Phase
		}
	}
	if currentPhase == -1 {
		currentPhase = 0
	}
	total := len(workers)
	pct := 0
	if total > 0 {
		pct = (done * 100) / total
	}

	barW := totalW - 20
	if barW < 10 {
		barW = 10
	}
	filled := (pct * barW) / 100
	bar := strings.Repeat("█", filled) + strings.Repeat("░", barW-filled)

	sb.WriteString(fmt.Sprintf("%s  %s %d%%   %s/%d\n",
		m.styles.StatusWIP.Render("● Running"),
		m.styles.Green.Render(bar), pct,
		m.styles.Dim.Render(fmt.Sprintf("%d", done)),
		total,
	))
	stats := fmt.Sprintf("%s %d done  %s %d running  %s %d failed  %s %d pending",
		m.styles.StatusDone.Render("✓"), done,
		m.styles.StatusWIP.Render("●"), running,
		m.styles.StatusBlock.Render("✕"), failed,
		m.styles.StatusPend.Render("○"), pending,
	)
	if validating > 0 {
		stats += fmt.Sprintf("  %s %d validating", m.styles.StatusValidating.Render("◎"), validating)
	}
	if refining > 0 {
		stats += fmt.Sprintf("  %s %d refining", m.styles.StatusRefining.Render("⟳"), refining)
	}
	sb.WriteString(stats + "\n")
	final := m.mission.Stats
	finalLine := fmt.Sprintf("%s %d direct  %s %d via-fix  %s %d waived  %s %d unresolved  %s %d tainted",
		m.styles.StatusDone.Render("•"), final.DoneDirect,
		m.styles.Green.Render("•"), final.DoneViaFix,
		m.styles.Cyan.Render("•"), final.DoneWaived,
		m.styles.StatusBlock.Render("•"), final.BlockedUnresolved,
		m.styles.Yellow.Render("•"), final.BlockedTainted,
	)
	sb.WriteString(finalLine + "\n")

	// Left panel: Phases
	var left strings.Builder
	for phase := 0; phase <= maxPhase; phase++ {
		var pw []FeatureWorker
		for _, w := range workers {
			if w.Feature.Phase == phase {
				pw = append(pw, w)
			}
		}
		if len(pw) == 0 {
			continue
		}
		pName := "Unknown"
		if phase < len(phaseNames) {
			pName = phaseNames[phase]
		}
		left.WriteString(m.styles.Cyan.Render(fmt.Sprintf("Phase %d: %s", phase, pName)))
		left.WriteString("\n")
		for _, w := range pw {
			icon, style := workerStatusDisplay(w.Status, m.styles)
			elapsed := ""
			switch w.Status {
			case WorkerRunning, WorkerAwaitingValidation, WorkerValidating, WorkerRefining:
				if !w.StartTime.IsZero() {
					elapsed = m.styles.Dim.Render(fmt.Sprintf(" (%s)", time.Since(w.StartTime).Round(time.Second)))
				}
			case WorkerDone:
				if !w.EndTime.IsZero() {
					elapsed = m.styles.Dim.Render(fmt.Sprintf(" (%s)", w.EndTime.Sub(w.StartTime).Round(time.Second)))
				}
			}
			left.WriteString(fmt.Sprintf("  %s %s %s%s\n", style.Render(icon), w.Feature.ID, w.Feature.Title, elapsed))
			if (w.Status == WorkerRunning || w.Status == WorkerAwaitingValidation || w.Status == WorkerValidating || w.Status == WorkerRefining) && w.LastLine != "" {
				detailWrap := lipgloss.NewStyle().Width(leftInner - 4)
				wrapped := detailWrap.Render(w.LastLine)
				for _, wl := range strings.Split(wrapped, "\n") {
					left.WriteString(m.styles.Dim.Render(fmt.Sprintf("    %s", wl)) + "\n")
				}
			}
		}
		left.WriteString("\n")
	}

	leftPanel := lipgloss.NewStyle().
		Width(leftW).
		Border(border).
		BorderForeground(borderColor).
		Padding(0, 1).
		Render(m.styles.Cyan.Render("Phases") + "\n" + left.String())

	// Right: Features list + Progress Log
	var featList strings.Builder
	for _, w := range workers {
		icon, style := workerStatusDisplay(w.Status, m.styles)
		title := w.Feature.Title
		if len(title) > rightInner-8 {
			title = title[:rightInner-8] + "…"
		}
		featList.WriteString(fmt.Sprintf("%s %s %s\n", style.Render(icon), w.Feature.ID, title))
	}

	featPanel := lipgloss.NewStyle().
		Width(rightW).
		Border(border).
		BorderForeground(borderColor).
		Padding(0, 1).
		Render(fmt.Sprintf("%s %s\n%s",
			m.styles.Cyan.Render("Features"),
			m.styles.Dim.Render(fmt.Sprintf("%d/%d", done, total)),
			featList.String(),
		))

	var logContent strings.Builder
	start := 0
	if len(m.workerLogs) > 8 {
		start = len(m.workerLogs) - 8
	}
	logWrap := lipgloss.NewStyle().Width(rightInner)
	for _, line := range m.workerLogs[start:] {
		var renderStyle lipgloss.Style
		if strings.HasPrefix(line, "[AUTOFIX]") {
			renderStyle = m.styles.Magenta
		} else if strings.Contains(line, "✓") {
			renderStyle = m.styles.Green
		} else if strings.Contains(line, "✕") {
			renderStyle = m.styles.Red
		} else if strings.Contains(line, "●") || strings.Contains(line, "▶") {
			renderStyle = m.styles.Cyan
		} else {
			renderStyle = m.styles.Dim
		}
		wrapped := logWrap.Render(line)
		for _, wl := range strings.Split(wrapped, "\n") {
			logContent.WriteString(renderStyle.Render(wl) + "\n")
		}
	}

	logPanel := lipgloss.NewStyle().
		Width(rightW).
		Border(border).
		BorderForeground(borderColor).
		Padding(0, 1).
		Render(m.styles.Cyan.Render("Progress Log") + "\n" + logContent.String())

	rightCol := lipgloss.JoinVertical(lipgloss.Left, featPanel, logPanel)

	sb.WriteString(lipgloss.JoinHorizontal(lipgloss.Top, leftPanel, rightCol))
	return sb.String()
}

func (m Model) renderStaticOverview(totalW, leftW, rightW, leftInner, rightInner int, border lipgloss.Border, borderColor lipgloss.Color, phaseNames []string) string {
	var sb strings.Builder
	stats := m.mission.Stats

	statusLine := fmt.Sprintf("%s %d done  %s %d wip  %s %d blocked  %s %d pending",
		m.styles.StatusDone.Render("✓"), stats.Done,
		m.styles.StatusWIP.Render("●"), stats.InProgress,
		m.styles.StatusBlock.Render("✕"), stats.Blocked,
		m.styles.StatusPend.Render("○"), stats.Pending,
	)
	if stats.Validating > 0 {
		statusLine += fmt.Sprintf("  %s %d validating", m.styles.StatusValidating.Render("◎"), stats.Validating)
	}
	if stats.Refining > 0 {
		statusLine += fmt.Sprintf("  %s %d refining", m.styles.StatusRefining.Render("⟳"), stats.Refining)
	}
	sb.WriteString(statusLine + "\n")
	finalLine := fmt.Sprintf("%s %d direct  %s %d via-fix  %s %d waived  %s %d unresolved  %s %d tainted",
		m.styles.StatusDone.Render("•"), stats.DoneDirect,
		m.styles.Green.Render("•"), stats.DoneViaFix,
		m.styles.Cyan.Render("•"), stats.DoneWaived,
		m.styles.StatusBlock.Render("•"), stats.BlockedUnresolved,
		m.styles.Yellow.Render("•"), stats.BlockedTainted,
	)
	sb.WriteString(finalLine + "\n")

	// Left panel: Phases with scope
	var left strings.Builder
	scopeWrap := lipgloss.NewStyle().Width(leftInner - 4)
	for phase := 0; phase < 4; phase++ {
		features := featuresInPhase(m.mission.Features, phase)
		if len(features) == 0 {
			continue
		}
		pName := "Unknown"
		if phase < len(phaseNames) {
			pName = phaseNames[phase]
		}
		left.WriteString(m.styles.Cyan.Render(fmt.Sprintf("Phase %d: %s", phase, pName)))
		left.WriteString("\n")
		for _, f := range features {
			icon, style := statusDisplay(f.Status, m.styles)
			left.WriteString(fmt.Sprintf("  %s %s %s%s\n", style.Render(icon), f.ID, f.Title, featureOutcomeSuffix(f, m.styles)))
			if f.Scope != "" {
				wrapped := scopeWrap.Render(f.Scope)
				for _, wl := range strings.Split(wrapped, "\n") {
					left.WriteString(m.styles.Dim.Render(fmt.Sprintf("    %s", wl)) + "\n")
				}
			}
			if strings.TrimSpace(f.Description) != "" {
				left.WriteString(m.styles.Dim.Render(fmt.Sprintf("    desc: %s\n", truncatePreview(f.Description, 140))))
			}
		}
		left.WriteString("\n")
	}

	leftPanel := lipgloss.NewStyle().
		Width(leftW).
		Border(border).
		BorderForeground(borderColor).
		Padding(0, 1).
		Render(m.styles.Cyan.Render("Phases") + "\n" + left.String())

	// Right panel: Features compact list with cursor
	var right strings.Builder
	for i, f := range m.mission.Features {
		icon, style := statusDisplay(f.Status, m.styles)
		title := f.Title
		cursor := "  "
		maxTitle := rightInner - 10
		if i == m.featureCursor {
			cursor = m.styles.Title.Render("> ")
			maxTitle -= 2
		}
		if maxTitle > 0 && len(title) > maxTitle {
			title = title[:maxTitle] + "…"
		}
		if i == m.featureCursor {
			right.WriteString(fmt.Sprintf("%s%s %s %s%s\n", cursor, style.Render(icon), style.Render(f.ID), style.Render(title), featureOutcomeSuffix(f, m.styles)))
		} else {
			right.WriteString(fmt.Sprintf("%s%s %s %s%s\n", cursor, style.Render(icon), f.ID, title, featureOutcomeSuffix(f, m.styles)))
		}
	}

	rightPanel := lipgloss.NewStyle().
		Width(rightW).
		Border(border).
		BorderForeground(borderColor).
		Padding(0, 1).
		Render(fmt.Sprintf("%s %s\n%s",
			m.styles.Cyan.Render("Features"),
			m.styles.Dim.Render(fmt.Sprintf("%d/%d", stats.Done, stats.Total)),
			right.String(),
		))

	sb.WriteString(lipgloss.JoinHorizontal(lipgloss.Top, leftPanel, rightPanel))
	return sb.String()
}

func (m Model) renderKanbanTab() string {
	totalW := m.width - margin*2 - 4
	colW := totalW / 6
	if colW < 14 {
		colW = 14
	}

	type kanbanCol struct {
		name     string
		statuses []string
		style    lipgloss.Style
		features []Feature
	}

	cols := []kanbanCol{
		{"PENDING", []string{"pending", ""}, m.styles.StatusPend, nil},
		{"RUNNING", []string{"in_progress"}, m.styles.StatusWIP, nil},
		{"VALIDATING", []string{"awaiting_validation", "validating"}, m.styles.StatusValidating, nil},
		{"REFINING", []string{"refining"}, m.styles.StatusRefining, nil},
		{"DONE", []string{"done", "validated"}, m.styles.StatusDone, nil},
		{"BLOCKED", []string{"blocked"}, m.styles.StatusBlock, nil},
	}

	for i := range cols {
		for _, f := range m.mission.Features {
			for _, s := range cols[i].statuses {
				if f.Status == s {
					cols[i].features = append(cols[i].features, f)
					break
				}
			}
		}
	}

	var rendered []string
	for _, col := range cols {
		var content strings.Builder
		content.WriteString(col.style.Render(fmt.Sprintf("%s (%d)", col.name, len(col.features))))
		content.WriteString("\n")
		content.WriteString(col.style.Render(strings.Repeat("─", colW-4)))
		content.WriteString("\n")
		if len(col.features) == 0 {
			content.WriteString(m.styles.Dim.Render("(empty)"))
			content.WriteString("\n")
		}
		for _, f := range col.features {
			icon, st := statusDisplay(f.Status, m.styles)
			title := f.Title
			selected := m.featureCursor >= 0 && m.featureCursor < len(m.mission.Features) && m.mission.Features[m.featureCursor].ID == f.ID
			prefix := "  "
			maxTitle := colW - len(f.ID) - 8
			if selected {
				prefix = m.styles.Title.Render("> ")
			}
			if maxTitle > 0 && len(title) > maxTitle {
				title = title[:maxTitle] + "…"
			}
			if selected {
				content.WriteString(fmt.Sprintf("%s%s %s %s%s\n", prefix, st.Render(icon), st.Render(f.ID), st.Render(title), featureOutcomeSuffix(f, m.styles)))
			} else {
				content.WriteString(fmt.Sprintf("%s%s %s %s%s\n", prefix, st.Render(icon), f.ID, title, featureOutcomeSuffix(f, m.styles)))
			}
		}

		box := lipgloss.NewStyle().
			Width(colW).
			Border(lipgloss.RoundedBorder()).
			BorderForeground(col.style.GetForeground()).
			Padding(0, 1).
			Render(content.String())

		rendered = append(rendered, box)
	}

	stats := m.mission.Stats
	summary := fmt.Sprintf(
		"  %s %d direct  %s %d via-fix  %s %d waived  %s %d unresolved  %s %d tainted",
		m.styles.StatusDone.Render("•"), stats.DoneDirect,
		m.styles.Green.Render("•"), stats.DoneViaFix,
		m.styles.Cyan.Render("•"), stats.DoneWaived,
		m.styles.StatusBlock.Render("•"), stats.BlockedUnresolved,
		m.styles.Yellow.Render("•"), stats.BlockedTainted,
	)

	return summary + "\n" + "  " + lipgloss.JoinHorizontal(lipgloss.Top, rendered...)
}

func (m Model) renderLogTab() string {
	var sb strings.Builder

	// Filter header
	filterLabel := "All Workers"
	if m.logFilter >= 0 && m.logFilter < len(m.mission.Features) {
		f := m.mission.Features[m.logFilter]
		filterLabel = fmt.Sprintf("%s: %s", f.ID, f.Title)
	}
	sb.WriteString(fmt.Sprintf("  %s  %s\n",
		m.styles.Cyan.Render("Filter:"),
		m.styles.Title.Render(filterLabel),
	))
	sb.WriteString(m.styles.Dim.Render("  ←/→ switch worker · L reset to all"))
	sb.WriteString("\n\n")

	if len(m.workerLogs) == 0 {
		sb.WriteString(m.styles.Dim.Render("  No logs yet"))
		sb.WriteString("\n")
		return sb.String()
	}

	filterPrefix := ""
	if m.logFilter >= 0 && m.logFilter < len(m.mission.Features) {
		filterPrefix = fmt.Sprintf("[%s]", m.mission.Features[m.logFilter].ID)
	}

	// Render newest-first: the latest line sits at the top so new output is
	// visible without scrolling to the bottom.
	for i := len(m.workerLogs) - 1; i >= 0; i-- {
		line := m.workerLogs[i]
		if filterPrefix != "" && !strings.HasPrefix(line, filterPrefix) {
			continue
		}

		styled := line
		if strings.HasPrefix(line, "[AUTOFIX]") {
			styled = m.styles.Magenta.Render(line)
		} else if strings.HasPrefix(line, "[CRITIC]") {
			styled = m.styles.Yellow.Render(line)
		} else if strings.HasPrefix(line, "[VALIDATOR:") {
			styled = m.styles.StatusValidating.Render(line)
		} else if strings.HasPrefix(line, "[REFINE:") {
			styled = m.styles.StatusRefining.Render(line)
		} else if strings.Contains(line, "✓") {
			styled = m.styles.Green.Render(line)
		} else if strings.Contains(line, "✕") || strings.Contains(line, "FAILED") {
			styled = m.styles.Red.Render(line)
		} else if strings.Contains(line, "▶") || strings.Contains(line, "●") {
			styled = m.styles.Cyan.Render(line)
		} else if strings.HasPrefix(line, "[ORCH]") {
			styled = m.styles.Dim.Render(line)
		} else if strings.Contains(line, "▸") {
			idx := strings.Index(line, "]")
			if idx > 0 {
				prefix := m.styles.Dim.Render(line[:idx+1])
				rest := m.styles.Cyan.Render(line[idx+1:])
				styled = prefix + rest
			}
		}
		sb.WriteString("  " + styled + "\n")
	}

	return sb.String()
}

func (m Model) renderCostTab() string {
	var sb strings.Builder
	sb.WriteString("  " + m.styles.Cyan.Render("Cost & Tokens"))
	sb.WriteString("\n")
	sb.WriteString(m.styles.Dim.Render("  accumulated across runs · persisted to quest/cost.json"))
	sb.WriteString("\n\n")

	var records []CostRecord
	if m.costTracker != nil {
		records = m.costTracker.Records()
	}
	if len(records) == 0 {
		sb.WriteString(m.styles.Dim.Render("  No cost data yet — run the mission to populate."))
		sb.WriteString("\n")
		return sb.String()
	}

	total := CostTotals(records)
	sb.WriteString(fmt.Sprintf("  %s $%.4f   ·   %d calls\n",
		m.styles.Title.Render("Total:"), total.Cost, total.Calls))
	sb.WriteString(fmt.Sprintf("  %s in %s · out %s · cache %s\n\n",
		m.styles.Dim.Render("tokens:"),
		fmtTokens(total.Input), fmtTokens(total.Output), fmtTokens(total.Cache)))

	writeCostSection(&sb, m.styles, "By model", "Model",
		AggregateCostBy(records, func(r CostRecord) string {
			if r.Model == "" {
				return "(default)"
			}
			return r.Model
		}))

	writeCostSection(&sb, m.styles, "By role", "Role",
		AggregateCostBy(records, func(r CostRecord) string { return r.Role }))

	phaseNames := []string{"Foundation", "Core", "Polish", "Extras"}
	writeCostSection(&sb, m.styles, "By phase", "Phase",
		AggregateCostBy(records, func(r CostRecord) string {
			if r.Phase < 0 {
				return "Planning"
			}
			if r.Phase < len(phaseNames) {
				return phaseNames[r.Phase]
			}
			return fmt.Sprintf("Phase %d", r.Phase)
		}))

	titles := map[string]string{}
	for _, f := range m.mission.Features {
		titles[f.ID] = f.Title
	}
	byFeature := AggregateCostBy(records, func(r CostRecord) string { return r.FeatureID })
	for i := range byFeature {
		if t := titles[byFeature[i].Key]; t != "" {
			byFeature[i].Key = byFeature[i].Key + " — " + t
		}
	}
	writeCostSection(&sb, m.styles, "By feature", "Feature", byFeature)

	return sb.String()
}

// fmtTokens renders a token count compactly (1.2k, 3.4M).
func fmtTokens(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	default:
		return fmt.Sprintf("%d", n)
	}
}

func writeCostSection(sb *strings.Builder, styles Styles, title, keyHeader string, rows []CostRollup) {
	if len(rows) == 0 {
		return
	}
	sb.WriteString("  " + styles.Cyan.Render(title) + "\n")
	sb.WriteString(styles.Dim.Render(fmt.Sprintf("    %-30s %6s %8s %8s %8s %11s",
		keyHeader, "calls", "in", "out", "cache", "cost")))
	sb.WriteString("\n")
	for _, r := range rows {
		key := r.Key
		if len(key) > 30 {
			key = key[:29] + "…"
		}
		sb.WriteString(fmt.Sprintf("    %-30s %6d %8s %8s %8s %11s\n",
			key, r.Calls, fmtTokens(r.Input), fmtTokens(r.Output), fmtTokens(r.Cache),
			fmt.Sprintf("$%.4f", r.Cost)))
	}
	sb.WriteString("\n")
}

func (m Model) renderDiagramTab() string {
	var sb strings.Builder
	sb.WriteString(m.styles.Cyan.Render("  Dependency Graph"))
	sb.WriteString("\n\n")

	phases := make(map[int][]Feature)
	for _, f := range m.mission.Features {
		phases[f.Phase] = append(phases[f.Phase], f)
	}

	phaseNames := []string{"Foundation", "Core", "Integration", "Polish"}
	maxPhase := 0
	for p := range phases {
		if p > maxPhase {
			maxPhase = p
		}
	}

	depMap := make(map[string][]string)
	for _, f := range m.mission.Features {
		for _, dep := range f.DependsOn {
			depMap[dep] = append(depMap[dep], f.ID)
		}
	}

	for phase := 0; phase <= maxPhase; phase++ {
		feats, ok := phases[phase]
		if !ok {
			continue
		}

		name := fmt.Sprintf("Phase %d", phase)
		if phase < len(phaseNames) {
			name = fmt.Sprintf("Phase %d — %s", phase, phaseNames[phase])
		}
		sb.WriteString(m.styles.Yellow.Render(fmt.Sprintf("  ┌─ %s ", name)))
		sb.WriteString(m.styles.Dim.Render(strings.Repeat("─", 40)))
		sb.WriteString("\n")

		for i, f := range feats {
			icon, style := statusDisplay(f.Status, m.styles)
			prefix := "  │  "
			if i == len(feats)-1 {
				prefix = "  │  "
			}

			title := f.Title
			maxT := m.width - margin*2 - 20
			if maxT > 0 && len(title) > maxT {
				title = title[:maxT] + "…"
			}

			sb.WriteString(fmt.Sprintf("%s%s %s %s\n",
				m.styles.Dim.Render(prefix),
				style.Render(icon),
				style.Render(f.ID),
				title,
			))

			if len(f.DependsOn) > 0 {
				arrows := make([]string, len(f.DependsOn))
				for j, dep := range f.DependsOn {
					arrows[j] = dep
				}
				sb.WriteString(fmt.Sprintf("%s%s\n",
					m.styles.Dim.Render("  │     "),
					m.styles.Dim.Render("↑ "+strings.Join(arrows, ", ")),
				))
			}

			if targets, ok := depMap[f.ID]; ok && len(targets) > 0 {
				sb.WriteString(fmt.Sprintf("%s%s\n",
					m.styles.Dim.Render("  │     "),
					m.styles.Cyan.Render("↓ "+strings.Join(targets, ", ")),
				))
			}
		}

		if phase < maxPhase {
			sb.WriteString(m.styles.Dim.Render("  │"))
			sb.WriteString("\n")
			sb.WriteString(m.styles.Yellow.Render("  ▼"))
			sb.WriteString("\n")
		} else {
			sb.WriteString(m.styles.Dim.Render("  └"))
			sb.WriteString(m.styles.Dim.Render(strings.Repeat("─", 50)))
			sb.WriteString("\n")
		}
	}

	return sb.String()
}

func (m Model) dashboardView() string {
	w := m.width - margin*2

	header := "\n" + m.styles.Title.Render("Quest Control")
	specLabel := m.mission.Project
	if m.activeSpec != nil {
		specLabel = m.activeSpec.Slug
	}
	header += m.styles.Dim.Render(fmt.Sprintf("  %s", specLabel))
	if m.executing {
		header += " " + m.spinner.View()
	}
	header += "\n"

	// Tabs
	tabs := []struct {
		key  string
		name string
		tab  Tab
	}{
		{"F", "Overview", TabOverview},
		{"K", "Kanban", TabKanban},
		{"L", "Log", TabLog},
		{"D", "Diagram", TabDiagram},
		{"C", "Cost", TabCost},
	}
	var tabParts []string
	for _, t := range tabs {
		label := fmt.Sprintf("[%s] %s", t.key, t.name)
		if t.tab == m.activeTab {
			tabParts = append(tabParts, m.styles.TabActive.Render(label))
		} else {
			tabParts = append(tabParts, m.styles.TabInactive.Render(label))
		}
	}
	tabBar := "  " + strings.Join(tabParts, "  ")

	sep := m.styles.Separator.Render(strings.Repeat("─", w))

	content := m.viewport.View()

	verboseLabel := "V: verbose"
	if m.verbose {
		verboseLabel = "V: summary"
	}

	hasStuck := m.mission.Stats.InProgress > 0 || m.mission.Stats.BlockedUnresolved > 0 || m.mission.Stats.AwaitingValidation > 0 || m.mission.Stats.Validating > 0 || m.mission.Stats.Refining > 0

	var hintText string
	if m.executing {
		hintText = fmt.Sprintf("  Esc: stop · %s · Tab: switch · ↑↓/scroll · q: quit", verboseLabel)
	} else {
		var parts []string

		if m.featureCursor >= 0 && m.featureCursor < len(m.mission.Features) && (m.activeTab == TabOverview || m.activeTab == TabKanban) {
			sel := m.mission.Features[m.featureCursor]
			parts = append(parts, fmt.Sprintf("↑↓: select · Enter: retry %s", sel.ID))
		}
		if m.mission.Stats.Pending > 0 {
			parts = append(parts, "S: start all", "Shift+S: skip critic")
		}
		if hasStuck {
			parts = append(parts, "r: retry stuck")
		}
		if len(m.mission.Features) > 0 {
			parts = append(parts, "R: full reset (clear fixes)")
		}
		parts = append(parts, "G: full regen", "U: update plan", "C: changes", "E: edit spec", verboseLabel, "N: new", "Tab: switch", "q: quit")
		hintText = "  " + strings.Join(parts, " · ")
	}
	if m.confirmRegen {
		hintText = "  ⚠ Full regenerate plan? Replans all non-terminal work. (Y: confirm · any key: cancel)"
	}
	if m.confirmDelta {
		hintText = "  ⚠ Run Update Plan (delta)? Rebuild assertions/features incrementally. (Y: confirm · any key: cancel)"
	}
	if m.confirmChanges {
		hintText = "  ⚠ Run Changes mode? Sonnet will analyze spec changes before planning. (Y: confirm · any key: cancel)"
	}
	if m.confirmSkipCritic {
		hintText = "  ⚠ Start all WITHOUT critic gate? (Y: confirm · any key: cancel)"
	}
	if m.confirmFullReset == 1 {
		hintText = "  ⚠ Full reset will clear fix_features and reset all root features to pending. (Y: continue · any key: cancel)"
	}
	if m.confirmFullReset == 2 {
		hintText = "  ⚠ Final confirmation: full reset will auto-start execution from root features. (Y: confirm · any key: cancel)"
	}
	if m.criticFailReport != nil && !m.autoFixRunning {
		hintText = "  ↑↓: select · Enter: confirm · Esc: cancel"
	}
	if m.autoFixRunning {
		hintText = "  ⏳ Auto-fix in progress... Esc: cancel"
	}
	hint := m.styles.Hint.Render(hintText)

	pad := lipgloss.NewStyle().PaddingLeft(margin).PaddingRight(margin)
	return pad.Render(lipgloss.JoinVertical(lipgloss.Left,
		header, tabBar, sep, content, sep, hint,
	))
}

// --- Helpers ---

func countPhases(features []Feature) int {
	seen := make(map[int]bool)
	for _, f := range features {
		seen[f.Phase] = true
	}
	return len(seen)
}

func featuresInPhase(features []Feature, phase int) []Feature {
	var result []Feature
	for _, f := range features {
		if f.Phase == phase {
			result = append(result, f)
		}
	}
	return result
}

func statusDisplay(status string, s Styles) (string, lipgloss.Style) {
	switch status {
	case "done", "validated":
		return "✓", s.StatusDone
	case "in_progress":
		return "●", s.StatusWIP
	case "blocked":
		return "✕", s.StatusBlock
	case "awaiting_validation":
		return "◐", s.StatusWIP
	case "validating":
		return "◎", s.StatusValidating
	case "refining":
		return "⟳", s.StatusRefining
	default:
		return "○", s.StatusPend
	}
}

func featureOutcomeSuffix(f Feature, s Styles) string {
	switch f.Resolution {
	case ResolutionResolvedViaFix:
		return " " + s.Green.Render("[via-fix]")
	case ResolutionResolvedTainted:
		return " " + s.Yellow.Render("[tainted]")
	case ResolutionWaivedContract:
		return " " + s.Cyan.Render("[waived-contract]")
	case ResolutionUnresolved:
		if f.Status == "blocked" {
			return " " + s.Red.Render("[unresolved]")
		}
	}
	if f.Status == "done" || f.Status == "validated" {
		if f.Tainted {
			return " " + s.Yellow.Render("[tainted]")
		}
	}
	return ""
}

func workerStatusDisplay(status WorkerStatus, s Styles) (string, lipgloss.Style) {
	switch status {
	case WorkerDone:
		return "✓", s.StatusDone
	case WorkerRunning:
		return "●", s.StatusWIP
	case WorkerFailed:
		return "✕", s.StatusBlock
	case WorkerAwaitingValidation:
		return "◇", s.StatusValidating
	case WorkerValidating:
		return "◎", s.StatusValidating
	case WorkerRefining:
		return "⟳", s.StatusRefining
	default:
		return "○", s.StatusPend
	}
}
