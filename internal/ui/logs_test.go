package ui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/CathalByrneGit/pintail/internal/quack"
)

func TestLogsViewTargets(t *testing.T) {
	clients := []*quack.QuackClient{
		quack.NewQuackClient(quack.ServerConfig{Name: "local-dev", Type: quack.ConnLocal, Path: "/tmp/x.duckdb"}, nil, nil),
		quack.NewQuackClient(quack.ServerConfig{Name: "quack-a", Type: quack.ConnQuack, Host: "a", Port: 9494}, nil, nil),
		quack.NewQuackClient(quack.ServerConfig{Name: "lake", Type: quack.ConnDuckLake, CatalogPath: "/tmp/c", StoragePath: "/tmp/d"}, nil, nil),
		quack.NewQuackClient(quack.ServerConfig{Name: "quack-b", Type: quack.ConnQuack, Host: "b", Port: 9494}, nil, nil),
	}
	v := NewLogsView(clients)

	if !v.HasTarget() {
		t.Fatal("two Quack connections should give a target")
	}
	if got := v.TargetClient().Config.Name; got != "quack-a" {
		t.Errorf("target = %q, want quack-a", got)
	}

	v, _ = v.Update(key("tab"))
	if got := v.TargetClient().Config.Name; got != "quack-b" {
		t.Errorf("after tab, target = %q, want quack-b (local and lake skipped)", got)
	}

	// A setup with no Quack connection has nothing to show and says so.
	none := NewLogsView([]*quack.QuackClient{clients[0], clients[2]})
	if none.HasTarget() {
		t.Error("local + ducklake should have no log target")
	}
	if none.TargetClient() != nil {
		t.Error("TargetClient should be nil with no target")
	}
	if bar := none.ViewTargetBar(); !strings.Contains(bar, "no Quack connection") {
		t.Errorf("target bar should explain the empty state: %q", bar)
	}
	if none.FetchCmd() != nil {
		t.Error("there is nothing to fetch without a target")
	}
	if none.EnableCmd() != nil {
		t.Error("there is nothing to enable without a target")
	}
}

// Logging is off until switched on, and switching it on changes state on
// someone's server — so the panel explains the key rather than doing it by poll.
func TestLogsViewEmptyStateExplainsEnabling(t *testing.T) {
	v := NewLogsView([]*quack.QuackClient{
		quack.NewQuackClient(quack.ServerConfig{Name: "q", Type: quack.ConnQuack, Host: "h", Port: 9494}, nil, nil),
	})

	out := v.ViewTable(120)
	for _, want := range []string{"no entries", "enable_logging('Quack')", "[e]"} {
		if !strings.Contains(out, want) {
			t.Errorf("empty state missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(v.ViewFooter(), "enable logging") {
		t.Errorf("footer should offer the enable key: %s", v.ViewFooter())
	}
}

// The one fetch failure worth explaining: the log type exists only where the
// quack extension is loaded.
func TestLogsViewExplainsMissingLogType(t *testing.T) {
	v := NewLogsView([]*quack.QuackClient{
		quack.NewQuackClient(quack.ServerConfig{Name: "q", Type: quack.ConnQuack, Host: "h", Port: 9494}, nil, nil),
	})
	v, _ = v.Update(logsResultMsg{err: errString("Invalid Input Error: structured_log_schema: 'Quack' not found")})

	out := v.ViewTable(120)
	if !strings.Contains(out, "structured_log_schema") {
		t.Errorf("the backend error should be shown:\n%s", out)
	}
	if !strings.Contains(out, "quack extension is loaded") {
		t.Errorf("the panel should explain what that error means:\n%s", out)
	}
}

func TestLogsViewSelectionAndDetail(t *testing.T) {
	entries, err := quack.ParseLogRows(quackLogRows(t))
	if err != nil {
		t.Fatal(err)
	}
	v := NewLogsView([]*quack.QuackClient{
		quack.NewQuackClient(quack.ServerConfig{Name: "q", Type: quack.ConnQuack, Host: "h", Port: 9494}, nil, nil),
	})
	v, _ = v.Update(logsResultMsg{entries: entries})

	if len(v.entries) != 3 {
		t.Fatalf("got %d entries", len(v.entries))
	}

	// The table truncates the query; the detail panel carries it in full along
	// with the error, which has no column at all.
	if got := v.ViewDetail(100); !strings.Contains(got, "SELECT count(*) FROM orders") {
		t.Errorf("detail should show the full query:\n%s", got)
	}

	v, _ = v.Update(key("down"))
	v, _ = v.Update(key("down"))
	if v.cursor != 2 {
		t.Fatalf("cursor = %d, want 2", v.cursor)
	}
	detail := v.ViewDetail(100)
	if !strings.Contains(detail, "Binder Error") {
		t.Errorf("detail should show the entry's error:\n%s", detail)
	}
	if !strings.Contains(detail, "1 ms") {
		t.Errorf("detail should show the duration:\n%s", detail)
	}

	// Down at the end stays put rather than running off the slice.
	for i := 0; i < 5; i++ {
		v, _ = v.Update(key("down"))
	}
	if v.cursor != 2 {
		t.Errorf("cursor = %d, want it clamped to the last entry", v.cursor)
	}

	// A shorter result set after a refresh must not leave the cursor dangling.
	v, _ = v.Update(logsResultMsg{entries: entries[:1]})
	if v.cursor >= len(v.entries) {
		t.Errorf("cursor = %d with %d entries", v.cursor, len(v.entries))
	}
	_ = v.ViewDetail(100) // would panic if the cursor dangled
}

// Enabling reports its outcome and, on success, refreshes.
func TestLogsViewEnableOutcome(t *testing.T) {
	v := NewLogsView([]*quack.QuackClient{
		quack.NewQuackClient(quack.ServerConfig{Name: "quack-a", Type: quack.ConnQuack, Host: "h", Port: 9494}, nil, nil),
	})

	// Enabling carries the entries back with it, so the screen shows them
	// rather than issuing a second fetch on a fresh connection.
	v, cmd := v.Update(logsEnabledMsg{
		target:  "quack-a",
		entries: []quack.LogEntry{{MessageType: "PREPARE_REQUEST", Query: "SELECT 1"}},
	})
	if v.noticeErr || !strings.Contains(v.notice, "logging enabled on quack-a") {
		t.Errorf("notice = %q (err=%v)", v.notice, v.noticeErr)
	}
	if cmd != nil {
		t.Error("the entries came with the message; no second fetch is needed")
	}
	if len(v.entries) != 1 {
		t.Errorf("got %d entries, want the one that came with the message", len(v.entries))
	}
	if !strings.Contains(v.ViewTargetBar(), "logging enabled") {
		t.Error("the target bar should show the notice")
	}

	v, cmd = v.Update(logsEnabledMsg{target: "quack-a", err: "Permission denied"})
	if !v.noticeErr || !strings.Contains(v.notice, "Permission denied") {
		t.Errorf("notice = %q (err=%v), want the failure reported", v.notice, v.noticeErr)
	}
	if cmd != nil {
		t.Error("a failed enable should not refresh")
	}
}

// The log screen has to render at any size, like every other panel.
func TestLogsScreenRendersAtAnySize(t *testing.T) {
	entries, err := quack.ParseLogRows(quackLogRows(t))
	if err != nil {
		t.Fatal(err)
	}

	cfg := quack.ServerConfig{Name: "quack-a", Type: quack.ConnQuack, Host: "h", Port: 9494}
	m := Model{
		configs:     []quack.ServerConfig{cfg},
		clients:     quack.InitClients([]quack.ServerConfig{cfg}, nil),
		data:        make([]connData, 1),
		currentView: viewLogs,
	}
	m.connTable = buildConnectionTable(nil, 0)
	m.logs = NewLogsView(m.clients)
	m.logs.entries = entries

	for _, size := range [][2]int{{20, 10}, {40, 15}, {80, 24}, {120, 40}, {300, 80}} {
		m.width, m.height = size[0], size[1]
		if out := m.View(); out == "" {
			t.Fatalf("log screen rendered nothing at %dx%d", size[0], size[1])
		}
	}
}

// L opens the log screen from the dashboard; l is still the lake screen.
func TestDashboardOpensLogScreen(t *testing.T) {
	m := threeConnModel(t)
	m.logs = NewLogsView(m.clients)

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("L")})
	if got := next.(Model).currentView; got != viewLogs {
		t.Errorf("L opened view %v, want the log screen", got)
	}

	next, _ = next.(Model).Update(key("esc"))
	if got := next.(Model).currentView; got != viewDashboard {
		t.Errorf("esc from the log screen went to %v", got)
	}

	// Lowercase l still opens the lake screen.
	next, _ = m.Update(key("l"))
	if got := next.(Model).currentView; got != viewSnapshots {
		t.Errorf("l opened view %v, want the snapshots screen", got)
	}
}

// quackLogRows reads the shared sample of duckdb_logs_parsed('Quack') output.
// It lives in testdata rather than in a const so the parser test and the screen
// test assert against exactly the same rows.
func quackLogRows(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "quack_log_rows.json"))
	if err != nil {
		t.Fatalf("reading the log fixture: %v", err)
	}
	return data
}
