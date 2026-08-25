package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	catppuccin "github.com/catppuccin/go"

	"github.com/unbot/work9flow/internal/protocol"
)

// viewKind is the active screen of the TUI. The model is a small
// state machine over this enum plus the per-view payload.
type viewKind int

const (
	viewDashboard viewKind = iota
	viewDetail
	viewEvents
	viewAttentions
	viewInteract
	viewHelp
)

func (v viewKind) String() string {
	switch v {
	case viewDashboard:
		return "runs"
	case viewDetail:
		return "detail"
	case viewEvents:
		return "events"
	case viewAttentions:
		return "attentions"
	case viewInteract:
		return "interact"
	case viewHelp:
		return "help"
	}
	return "?"
}

// pollInterval is the cadence at which the dashboard / detail / events
// views re-fetch from the runtime.
const pollInterval = 2 * time.Second

// ----- bubbletea messages -----

type tickMsg time.Time
type healthMsg struct {
	resp protocol.HealthResponse
	err  error
}
type runsMsg struct {
	runs []protocol.RunSummary
	err  error
}
type detailMsg struct {
	detail protocol.RunDetail
	err    error
}
type eventsMsg struct {
	events []protocol.EventDTO
	latest int64
	err    error
}
type attentionsMsg struct {
	atts []protocol.AttentionDTO
	err  error
}
type actionMsg struct {
	op   string
	text string
	err  error
}

// ----- model -----

type model struct {
	base   string
	client *Client

	view viewKind
	// dashboard state
	runs     []protocol.RunSummary
	selected int
	// detail / events / attentions
	current   *protocol.RunDetail
	events    []protocol.EventDTO
	eventSeq  int64
	attentions []protocol.AttentionDTO
	// interact prompt buffer
	input  string
	cursor int
	// common
	health  protocol.HealthResponse
	err     error
	width   int
	height  int
	loaded  bool
	quitting bool
	statusMsg string
	statusAt  time.Time
}

func newModel(base string) *model {
	return &model{
		base:   base,
		client: NewClient(base),
		view:   viewDashboard,
	}
}

func (m *model) Init() tea.Cmd {
	return tea.Batch(m.pollHealth(), tick())
}

func tick() tea.Cmd {
	return tea.Tick(pollInterval, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m *model) pollHealth() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		h, err := m.client.Health(ctx)
		return healthMsg{resp: h, err: err}
	}
}

func (m *model) fetchRuns() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		runs, err := m.client.ListRuns(ctx)
		return runsMsg{runs: runs, err: err}
	}
}

func (m *model) fetchDetail(id string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		d, err := m.client.GetRun(ctx, id)
		return detailMsg{detail: d, err: err}
	}
}

func (m *model) fetchEvents(runID string, after int64) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		evs, latest, err := m.client.EventsAfter(ctx, runID, after)
		return eventsMsg{events: evs, latest: latest, err: err}
	}
}

func (m *model) fetchAttentions(runID string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		atts, err := m.client.ListAttentions(ctx, runID)
		return attentionsMsg{atts: atts, err: err}
	}
}

func (m *model) doCancel(id string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		err := m.client.CancelRun(ctx, id)
		return actionMsg{op: "cancel", text: id, err: err}
	}
}

func (m *model) doAnswer(attentionID string, answer json.RawMessage) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		a, err := m.client.AnswerAttention(ctx, attentionID, answer)
		if err != nil {
			return actionMsg{op: "answer", text: attentionID, err: err}
		}
		return actionMsg{op: "answer", text: a.ID, err: nil}
	}
}

func (m *model) doSteer(runID, agentID, message string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		err := m.client.Steer(ctx, runID, protocol.SteerRequest{AgentID: agentID, Message: message})
		return actionMsg{op: "steer", text: runID, err: err}
	}
}

func (m *model) doFollowup(runID, agentID, message string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		err := m.client.Followup(ctx, runID, protocol.FollowupRequest{AgentID: agentID, Message: message})
		return actionMsg{op: "followup", text: runID, err: err}
	}
}

// Update dispatches on message type. The polling loop lives here.
func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
	case tea.KeyMsg:
		return m.handleKey(msg)
	case healthMsg:
		m.health = msg.resp
		m.err = msg.err
		m.loaded = true
		if m.view == viewDashboard && m.current == nil {
			// First-time load: also fetch runs.
			return m, m.fetchRuns()
		}
		return m, nil
	case runsMsg:
		if msg.err == nil {
			m.runs = msg.runs
			if m.selected >= len(m.runs) {
				m.selected = len(m.runs) - 1
			}
			if m.selected < 0 {
				m.selected = 0
			}
		}
		m.err = msg.err
		return m, nil
	case detailMsg:
		if msg.err == nil {
			m.current = &msg.detail
		}
		m.err = msg.err
		return m, nil
	case eventsMsg:
		if msg.err == nil {
			m.events = append(m.events, msg.events...)
			m.eventSeq = msg.latest
			// Keep the event log bounded for display.
			if len(m.events) > 500 {
				m.events = m.events[len(m.events)-500:]
			}
		}
		m.err = msg.err
		return m, nil
	case attentionsMsg:
		m.attentions = msg.atts
		return m, nil
	case actionMsg:
		if msg.err != nil {
			m.setStatus(fmt.Sprintf("%s failed: %v", msg.op, msg.err))
		} else {
			m.setStatus(fmt.Sprintf("%s ok", msg.op))
		}
		// Re-fetch current screen payload.
		return m, m.refreshCurrent()
	case tickMsg:
		if m.quitting {
			return m, nil
		}
		return m, tea.Batch(m.pollHealth(), m.refreshCurrent(), tick())
	}
	return m, nil
}

func (m *model) setStatus(s string) {
	m.statusMsg = s
	m.statusAt = time.Now()
}

// refreshCurrent triggers the right fetches for the active view.
func (m *model) refreshCurrent() tea.Cmd {
	switch m.view {
	case viewDashboard:
		return m.fetchRuns()
	case viewDetail:
		if m.current != nil {
			return m.fetchDetail(m.current.ID)
		}
	case viewEvents:
		if m.current != nil {
			return m.fetchEvents(m.current.ID, m.eventSeq)
		}
	case viewAttentions:
		if m.current != nil {
			return m.fetchAttentions(m.current.ID)
		}
	}
	return nil
}

func (m *model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Global keys: q + ctrl+c quit; ? toggles help; 1..6 jump views.
	switch msg.String() {
	case "ctrl+c":
		m.quitting = true
		return m, tea.Quit
	case "q":
		if m.view == viewHelp {
			m.view = m.previousView()
			return m, nil
		}
		m.quitting = true
		return m, tea.Quit
	case "?":
		if m.view != viewHelp {
			m.previousView() // best effort
			m.view = viewHelp
		}
		return m, nil
	case "1":
		m.view = viewDashboard
		return m, m.fetchRuns()
	case "2":
		if m.current != nil {
			m.view = viewDetail
			return m, m.fetchDetail(m.current.ID)
		}
	case "3":
		if m.current != nil {
			m.view = viewEvents
			m.events = nil
			m.eventSeq = 0
			return m, m.fetchEvents(m.current.ID, 0)
		}
	case "4":
		if m.current != nil {
			m.view = viewAttentions
			return m, m.fetchAttentions(m.current.ID)
		}
	case "5":
		if m.current != nil {
			m.view = viewInteract
			m.input = ""
			m.cursor = 0
		}
	case "esc":
		if m.view == viewHelp {
			m.view = viewDashboard
			return m, m.fetchRuns()
		}
		if m.view != viewDashboard {
			m.view = viewDashboard
			return m, m.fetchRuns()
		}
	}

	// View-specific keys.
	switch m.view {
	case viewDashboard:
		return m.dashboardKey(msg)
	case viewInteract:
		return m.interactKey(msg)
	}
	return m, nil
}

// previousView returns the view to fall back to from help.
func (m *model) previousView() viewKind {
	if m.current != nil {
		return viewDetail
	}
	return viewDashboard
}

func (m *model) dashboardKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "up", "k":
		if m.selected > 0 {
			m.selected--
		}
	case "down", "j":
		if m.selected < len(m.runs)-1 {
			m.selected++
		}
	case "enter":
		if m.selected < len(m.runs) {
			id := m.runs[m.selected].ID
			m.current = nil
			m.view = viewDetail
			return m, m.fetchDetail(id)
		}
	case "n":
		m.view = viewInteract
		m.input = "new:"
		m.cursor = len(m.input)
	case "r":
		return m, m.fetchRuns()
	case "d":
		if m.selected < len(m.runs) {
			return m, m.doCancel(m.runs[m.selected].ID)
		}
	}
	return m, nil
}

func (m *model) interactKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEnter:
		// Interpret the buffer:
		//   "new:workflow_id|repo_path|original_task"
		//   "answer:attention_id|raw_answer"
		//   "steer:run_id|agent_id|message"
		//   "followup:run_id|agent_id|message"
		line := strings.TrimSpace(m.input)
		m.input = ""
		m.cursor = 0
		return m, m.runAction(line)
	case tea.KeyBackspace:
		if m.cursor > 0 {
			m.input = m.input[:m.cursor-1] + m.input[m.cursor:]
			m.cursor--
		}
	case tea.KeyLeft:
		if m.cursor > 0 {
			m.cursor--
		}
	case tea.KeyRight:
		if m.cursor < len(m.input) {
			m.cursor++
		}
	default:
		s := msg.String()
		if len([]rune(s)) == 1 && msg.Type == tea.KeyRunes {
			m.input = m.input[:m.cursor] + s + m.input[m.cursor:]
			m.cursor += len([]rune(s))
		}
	}
	return m, nil
}

// runAction interprets a typed command line.
func (m *model) runAction(line string) tea.Cmd {
	parts := strings.SplitN(line, ":", 2)
	if len(parts) != 2 {
		m.setStatus("bad action; expected verb:args")
		return nil
	}
	verb, args := parts[0], parts[1]
	fields := strings.SplitN(args, "|", 3)
	switch verb {
	case "new":
		if len(fields) < 3 {
			m.setStatus("new needs workflow_id|repo_path|task")
			return nil
		}
		return func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			r, err := m.client.CreateRun(ctx, protocol.RunCreateRequest{
				WorkflowID: fields[0], RepoPath: fields[1], OriginalTask: fields[2],
			})
			if err != nil {
				return actionMsg{op: "create", text: "", err: err}
			}
			return actionMsg{op: "create", text: r.ID}
		}
	case "answer":
		if len(fields) < 2 {
			m.setStatus("answer needs attention_id|raw_answer")
			return nil
		}
		return m.doAnswer(fields[0], json.RawMessage(fields[1]))
	case "steer":
		if len(fields) < 3 || m.current == nil {
			m.setStatus("steer needs agent_id|message (and a selected run)")
			return nil
		}
		return m.doSteer(m.current.ID, fields[0], fields[1]+"|"+fields[2])
	case "followup":
		if len(fields) < 3 || m.current == nil {
			m.setStatus("followup needs agent_id|message (and a selected run)")
			return nil
		}
		return m.doFollowup(m.current.ID, fields[0], fields[1]+"|"+fields[2])
	case "cancel":
		if len(fields) < 1 {
			m.setStatus("cancel needs run_id")
			return nil
		}
		return m.doCancel(fields[0])
	default:
		m.setStatus("unknown verb: " + verb)
	}
	return nil
}

// ----- view rendering -----

var palette = catppuccin.Mocha

func (m *model) View() string {
	if !m.loaded {
		return lipgloss.NewStyle().
			Foreground(lipgloss.Color(palette.Yellow().Hex)).
			Render("connecting to runtime…")
	}
	if m.err != nil && m.view == viewDashboard && m.current == nil {
		return lipgloss.NewStyle().
			Foreground(lipgloss.Color(palette.Red().Hex)).
			Render(fmt.Sprintf("runtime unreachable: %v", m.err))
	}
	var body string
	switch m.view {
	case viewDashboard:
		body = m.viewDashboard()
	case viewDetail:
		body = m.viewDetail()
	case viewEvents:
		body = m.viewEvents()
	case viewAttentions:
		body = m.viewAttentions()
	case viewInteract:
		body = m.viewInteract()
	case viewHelp:
		body = m.viewHelp()
	}
	return lipgloss.JoinVertical(lipgloss.Left, m.header(), body, "", m.footer())
}

func (m *model) header() string {
	title := appName + " — TUI client"
	header := lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color(palette.Peach().Hex)).
		Padding(0, 1).
		Render(title)
	status := lipgloss.NewStyle().
		Foreground(lipgloss.Color(palette.Subtext1().Hex)).
		Render(fmt.Sprintf("runtime: %s   view: %s", m.base, m.view))
	return lipgloss.JoinVertical(lipgloss.Left, header, status)
}

func (m *model) footer() string {
	if m.statusMsg != "" && time.Since(m.statusAt) < 5*time.Second {
		return lipgloss.NewStyle().
			Foreground(lipgloss.Color(palette.Yellow().Hex)).
			Render("status: " + m.statusMsg)
	}
	help := "? help   1..5 views   q quit"
	if m.view == viewInteract {
		help = "enter to submit   esc back"
	}
	return lipgloss.NewStyle().
		Foreground(lipgloss.Color(palette.Overlay1().Hex)).
		Render(help)
}

func (m *model) viewDashboard() string {
	if len(m.runs) == 0 {
		return lipgloss.NewStyle().
			Foreground(lipgloss.Color(palette.Subtext1().Hex)).
			Render("(no runs — press n to start one)")
	}
	var rows []string
	for i, r := range m.runs {
		prefix := "  "
		style := lipgloss.NewStyle().Foreground(lipgloss.Color(palette.Text().Hex))
		if i == m.selected {
			prefix = "▶ "
			style = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(palette.Peach().Hex))
		}
		stateColor := stateColor(r.State)
		state := lipgloss.NewStyle().Foreground(lipgloss.Color(stateColor)).Render(r.State)
		row := fmt.Sprintf("%s%s  %s  %s  pending=%d  %s",
			prefix, r.ID, state, r.WorkflowID, r.Pending, time.Unix(r.CreatedAt, 0).Format("15:04:05"))
		rows = append(rows, style.Render(row))
	}
	return strings.Join(rows, "\n")
}

func stateColor(state string) string {
	switch state {
	case "DONE":
		return palette.Green().Hex
	case "FAILED", "CANCELED":
		return palette.Red().Hex
	case "WAITING_FOR_USER":
		return palette.Yellow().Hex
	case "NEW", "DISCOVERY", "PLANNING", "PLAN_REVIEW":
		return palette.Blue().Hex
	}
	return palette.Text().Hex
}

func (m *model) viewDetail() string {
	if m.current == nil {
		return "(no run selected — press esc to go back to the dashboard)"
	}
	d := m.current
	lines := []string{
		fmt.Sprintf("id:           %s", d.ID),
		fmt.Sprintf("workflow:     %s", d.WorkflowID),
		fmt.Sprintf("state:        %s", lipgloss.NewStyle().Foreground(lipgloss.Color(stateColor(d.State))).Render(d.State)),
		fmt.Sprintf("stage:        %s", d.Stage),
		fmt.Sprintf("repo:         %s", d.RepoPath),
		fmt.Sprintf("task:         %s", d.OriginalTask),
		fmt.Sprintf("pending_attn: %d   events: %d   artifacts: %d", d.PendingAttention, d.EventCount, d.ArtifactCount),
	}
	if d.TerminalReason != "" {
		lines = append(lines, "terminal:     "+d.TerminalReason)
	}
	if len(d.IterationCounters) > 0 {
		lines = append(lines, "iterations:")
		for k, v := range d.IterationCounters {
			lines = append(lines, fmt.Sprintf("  %s = %d", k, v))
		}
	}
	if len(d.ActiveArtifactVersions) > 0 {
		lines = append(lines, "active artifacts:")
		for k, v := range d.ActiveArtifactVersions {
			lines = append(lines, fmt.Sprintf("  %s = %d", k, v))
		}
	}
	return lipgloss.NewStyle().Foreground(lipgloss.Color(palette.Text().Hex)).Render(strings.Join(lines, "\n"))
}

func (m *model) viewEvents() string {
	if m.current == nil {
		return "(no run selected)"
	}
	if len(m.events) == 0 {
		return "(no events yet — they will appear as the run progresses)"
	}
	limit := len(m.events)
	if limit > 200 {
		limit = 200
	}
	var rows []string
	for _, e := range m.events[len(m.events)-limit:] {
		ts := time.Unix(e.At, 0).Format("15:04:05")
		rows = append(rows, fmt.Sprintf("seq=%d  %s  %s", e.Seq, ts, e.Kind))
	}
	return strings.Join(rows, "\n")
}

func (m *model) viewAttentions() string {
	if m.current == nil {
		return "(no run selected)"
	}
	if len(m.attentions) == 0 {
		return "(no attentions — workflow is unblocked)"
	}
	var rows []string
	for _, a := range m.attentions {
		statusColor := palette.Yellow().Hex
		if a.Status == "ANSWERED" {
			statusColor = palette.Green().Hex
		}
		row := fmt.Sprintf("[%s] %s   %s   %s",
			lipgloss.NewStyle().Foreground(lipgloss.Color(statusColor)).Render(a.Status),
			a.ID, a.Title, a.Kind)
		rows = append(rows, row)
		if len(a.Options) > 0 {
			rows = append(rows, "  options: "+strings.Join(a.Options, ", "))
		}
	}
	return strings.Join(rows, "\n") + "\n\n" +
		lipgloss.NewStyle().Foreground(lipgloss.Color(palette.Subtext1().Hex)).Render(
			"answer from the interact view: answer:<attention_id>|<raw_answer>")
}

func (m *model) viewInteract() string {
	prompt := "action> "
	lines := []string{prompt + m.input}
	lines = append(lines, "")
	lines = append(lines, lipgloss.NewStyle().Foreground(lipgloss.Color(palette.Subtext1().Hex)).Render(
		"verbs: new:wf|repo|task   answer:id|raw   steer:id|msg   followup:id|msg   cancel:id"))
	return strings.Join(lines, "\n")
}

func (m *model) viewHelp() string {
	lines := []string{
		"work9flow TUI — keys",
		"",
		"  1   runs dashboard",
		"  2   selected run detail",
		"  3   event stream for selected run",
		"  4   attention inbox for selected run",
		"  5   interact (typed commands)",
		"  ↑/↓ select run",
		"  enter open selected",
		"  n    start typing a new run",
		"  d    cancel selected run",
		"  r    refresh",
		"  ?    this help",
		"  q    quit (does not stop the runtime)",
	}
	return strings.Join(lines, "\n")
}
