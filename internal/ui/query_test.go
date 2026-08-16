package ui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/CathalByrneGit/pintail/internal/quack"
)

// The scratchpad has to hold onto the cancel func while a query is in flight,
// and clear it when the result lands.
func TestScratchpadTracksCancellation(t *testing.T) {
	cfg := quack.ServerConfig{Name: "local", Type: quack.ConnLocal, Path: "/tmp/whatever.duckdb"}
	c := quack.NewQuackClient(cfg, nil, nil,
		quack.WithState(quack.ConnState{Online: true}))

	sp := NewScratchpad([]quack.ServerInfo{cfg.ToServerInfo()}, []*quack.QuackClient{c})
	sp.Resize(100, 40)
	sp.editor.SetValue("SELECT 1;")

	sp, cmd := sp.runQuery()
	if !sp.Running() {
		t.Fatal("Running() should be true with a query in flight")
	}
	if sp.cancelQuery == nil {
		t.Fatal("no cancel func was kept, so nothing could interrupt the query")
	}
	if cmd == nil {
		t.Fatal("runQuery returned no command")
	}

	// The status line tells the user how to interrupt.
	if status := sp.ViewResultsStatus(); !strings.Contains(status, "interrupt") {
		t.Errorf("running status should mention interrupting, got %q", status)
	}

	// ctrl+c while running cancels rather than falling through to other keys.
	sp, _ = sp.Update(key("ctrl+c"))
	if sp.cancelQuery != nil {
		t.Error("cancel func should be cleared once used")
	}

	// The result arriving clears the running state.
	sp, _ = sp.Update(queryResultMsg{result: &quack.QueryResult{Query: "SELECT 1;", Err: "cancelled", Method: "cli"}})
	if sp.Running() {
		t.Error("Running() should be false once a result arrives")
	}
}

// ctrl+c quits the app normally, but interrupts the query when one is running —
// otherwise the only way out of a slow query is killing the process.
func TestRootModelRoutesCtrlCWhileQuerying(t *testing.T) {
	cfg := quack.ServerConfig{Name: "local", Type: quack.ConnLocal, Path: "/tmp/whatever.duckdb"}
	c := quack.NewQuackClient(cfg, nil, nil,
		quack.WithState(quack.ConnState{Online: true}))

	m := Model{
		configs:     []quack.ServerConfig{cfg},
		clients:     []*quack.QuackClient{c},
		data:        make([]connData, 1),
		currentView: viewScratchpad,
		width:       100,
		height:      40,
	}
	m.connTable = buildConnectionTable(nil)
	m.scratchpad = NewScratchpad([]quack.ServerInfo{cfg.ToServerInfo()}, []*quack.QuackClient{c})
	m.scratchpad.Resize(100, 40)

	// Not running: ctrl+c quits.
	_, cmd := m.Update(key("ctrl+c"))
	if cmd == nil {
		t.Fatal("ctrl+c with no query running should quit")
	}
	if msg := cmd(); msg == nil || fmt.Sprintf("%T", msg) != "tea.QuitMsg" {
		t.Errorf("expected a quit message, got %T", msg)
	}

	// Running: ctrl+c is handed to the scratchpad and the app stays up.
	m.scratchpad.editor.SetValue("SELECT 1;")
	m.scratchpad, _ = m.scratchpad.runQuery()
	if !m.scratchpad.Running() {
		t.Fatal("expected a query in flight")
	}

	next, cmd := m.Update(key("ctrl+c"))
	if cmd != nil {
		if msg := cmd(); msg != nil && fmt.Sprintf("%T", msg) == "tea.QuitMsg" {
			t.Error("ctrl+c during a query should not quit the app")
		}
	}
	if next.(Model).scratchpad.cancelQuery != nil {
		t.Error("the query should have been cancelled")
	}

	// esc while running cancels too, and stays on the screen.
	m.scratchpad, _ = m.scratchpad.runQuery()
	next, _ = m.Update(key("esc"))
	if got := next.(Model).currentView; got != viewScratchpad {
		t.Errorf("esc during a query left the screen (view = %v)", got)
	}
}
