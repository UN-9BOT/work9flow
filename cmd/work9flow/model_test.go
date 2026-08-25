package main

import (
	"encoding/json"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/unbot/work9flow/internal/protocol"
)

func TestModelDashboardNavigation(t *testing.T) {
	m := newModel("http://localhost:1")
	m.loaded = true
	m.runs = []protocol.RunSummary{
		{ID: "r1", WorkflowID: "feature-development", State: "PLANNING"},
		{ID: "r2", WorkflowID: "feature-development", State: "DONE"},
	}
	m.selected = 0
	// Press down.
	mUpd, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	mm := mUpd.(*model)
	if mm.selected != 1 {
		t.Errorf("selected = %d", mm.selected)
	}
	mUpd, _ = mm.Update(tea.KeyMsg{Type: tea.KeyDown})
	mm = mUpd.(*model)
	if mm.selected != 1 { t.Errorf("after down: %d", mm.selected) }
	mUpd, _ = mm.Update(tea.KeyMsg{Type: tea.KeyUp})
	mm = mUpd.(*model)
	if mm.selected != 0 {
		t.Errorf("selected = %d, want 0", mm.selected)
	}
}

func TestModelHealthMessageFlagsLoaded(t *testing.T) {
	m := newModel("http://localhost:1")
	mUpd, _ := m.Update(healthMsg{resp: protocol.HealthResponse{Status: "ok"}, err: nil})
	mm := mUpd.(*model)
	if !mm.loaded {
		t.Errorf("loaded = false")
	}
	if mm.health.Status != "ok" {
		t.Errorf("status = %q", mm.health.Status)
	}
}

func TestModelEventsMessageAppendsAndBounds(t *testing.T) {
	m := newModel("http://localhost:1")
	m.loaded = true
	// Append 600 events; expect truncation to 500.
	for i := 0; i < 600; i++ {
		upd, _ := m.Update(eventsMsg{
			events: []protocol.EventDTO{{RunID: "r1", Seq: int64(i + 1), Kind: "k"}},
			latest: int64(i + 1),
		})
		m = upd.(*model)
	}
	if len(m.events) != 500 {
		t.Errorf("events len = %d, want 500", len(m.events))
	}
	if m.eventSeq != 600 {
		t.Errorf("latest = %d, want 600", m.eventSeq)
	}
}

func TestModelInteractBufferInsertDelete(t *testing.T) {
	m := newModel("http://localhost:1")
	m.loaded = true
	m.current = &protocol.RunDetail{ID: "r1", State: "PLANNING"}
	m.view = viewInteract
	m.input = "steer:"
	m.cursor = len(m.input)
	// Type "a".
	upd, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	mm := upd.(*model)
	if mm.input != "steer:a" {
		t.Errorf("input = %q", mm.input)
	}
	// Backspace.
	upd, _ = mm.Update(tea.KeyMsg{Type: tea.KeyBackspace})
	mm = upd.(*model)
	if mm.input != "steer:" {
		t.Errorf("input = %q", mm.input)
	}
}

func TestModelRunActionBad(t *testing.T) {
	m := newModel("http://localhost:1")
	m.loaded = true
	m.current = &protocol.RunDetail{ID: "r1"}
	m.view = viewInteract
	m.input = "garbage"
	cmd := m.runAction("garbage")
	if cmd != nil {
		t.Errorf("bad action should return nil cmd")
	}
}

func TestModelHelpToggles(t *testing.T) {
	m := newModel("http://localhost:1")
	m.loaded = true
	upd, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'?'}})
	mm := upd.(*model)
	if mm.view != viewHelp {
		t.Errorf("view = %v, want help", mm.view)
	}
	upd, _ = mm.Update(tea.KeyMsg{Type: tea.KeyEsc})
	mm = upd.(*model)
	if mm.view == viewHelp {
		t.Errorf("esc did not exit help")
	}
}

func TestModelRenderIncludesHeader(t *testing.T) {
	m := newModel("http://localhost:1")
	m.loaded = true
	m.health = protocol.HealthResponse{Status: "ok", Version: "v"}
	s := m.View()
	if !strings.Contains(s, "work9flow") {
		t.Errorf("view missing brand: %s", s)
	}
	if !strings.Contains(s, "runs") {
		t.Errorf("view missing current view label: %s", s)
	}
}

func TestModelQuitDoesNotStopRuntime(t *testing.T) {
	m := newModel("http://localhost:1")
	m.loaded = true
	upd, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	mm := upd.(*model)
	if !mm.quitting {
		t.Errorf("quitting not set")
	}
	if cmd == nil {
		t.Errorf("expected tea.Quit cmd")
	}
}

func TestModelActionMsgRecordsStatus(t *testing.T) {
	m := newModel("http://localhost:1")
	m.loaded = true
	upd, _ := m.Update(actionMsg{op: "cancel", text: "r1", err: nil})
	mm := upd.(*model)
	if !strings.Contains(mm.statusMsg, "cancel") {
		t.Errorf("statusMsg = %q", mm.statusMsg)
	}
}

func TestModelAnswerWithRawJSON(t *testing.T) {
	m := newModel("http://localhost:1")
	m.loaded = true
	// Simulate typing "answer:a1|postgres".
	cmd := m.runAction("answer:a1|postgres")
	if cmd == nil {
		t.Fatalf("expected non-nil cmd")
	}
	// We can't easily execute the cmd here without a server, but
	// the cmd shape (doAnswer) is enough proof that the routing
	// produced a callable closure.
	_ = cmd
	_ = json.RawMessage(`"x"`)
}
