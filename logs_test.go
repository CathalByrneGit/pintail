package main

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// A row of duckdb_logs_parsed('Quack') as the Quack reference documents it.
const quackLogRows = `[
  {"timestamp":"2026-08-14 09:06:19.841623+02","type":"Quack","log_level":"DEBUG",
   "message_type":"PREPARE_REQUEST","quack_connection_id":"091A003553E7E67B615B73D6BE81FD2E",
   "client_query_id":18,"query":"SELECT count(*) FROM orders","server":"http://localhost:9494",
   "duration_ms":41,"response_type":"PREPARE_RESPONSE","error":null},
  {"timestamp":"2026-08-14 09:06:20.100000+02","type":"Quack","log_level":"DEBUG",
   "message_type":"FETCH_REQUEST","quack_connection_id":"091A003553E7E67B615B73D6BE81FD2E",
   "client_query_id":19,"query":null,"server":"http://localhost:9494",
   "duration_ms":3,"response_type":"FETCH_RESPONSE","error":null},
  {"timestamp":"2026-08-14 09:06:21.000000+02","type":"Quack","log_level":"ERROR",
   "message_type":"PREPARE_REQUEST","quack_connection_id":"091A003553E7E67B615B73D6BE81FD2E",
   "client_query_id":20,"query":"SELECT nope FROM orders","server":"http://localhost:9494",
   "duration_ms":1,"response_type":"ERROR","error":"Binder Error: nope not found"},
  {"timestamp":"2026-08-14 09:06:22.000000+02","type":"Quack","log_level":"DEBUG",
   "message_type":"PREPARE_REQUEST","quack_connection_id":"091A003553E7E67B615B73D6BE81FD2E",
   "client_query_id":21,"query":"SELECT * FROM duckdb_logs_parsed('Quack') ORDER BY timestamp DESC LIMIT 200",
   "server":"http://localhost:9494","duration_ms":2,"response_type":"PREPARE_RESPONSE","error":null}
]`

func TestParseLogRows(t *testing.T) {
	entries, err := parseLogRows([]byte(quackLogRows))
	if err != nil {
		t.Fatalf("parseLogRows: %v", err)
	}

	// Four rows in, three out: our own log-reading query is dropped so the panel
	// shows the server's traffic rather than the fact that it is being watched.
	if len(entries) != 3 {
		t.Fatalf("got %d entries, want 3 (the poll's own row filtered):\n%+v", len(entries), entries)
	}
	for _, e := range entries {
		if strings.Contains(e.Query, "duckdb_logs_parsed") {
			t.Errorf("the poll's own query survived filtering: %q", e.Query)
		}
	}

	first := entries[0]
	if first.MessageType != "PREPARE_REQUEST" {
		t.Errorf("MessageType = %q", first.MessageType)
	}
	if first.Query != "SELECT count(*) FROM orders" {
		t.Errorf("Query = %q", first.Query)
	}
	if first.DurationMs != "41" {
		t.Errorf("DurationMs = %q, want 41", first.DurationMs)
	}
	if first.ConnectionID == "" {
		t.Error("ConnectionID should carry the server-issued id")
	}
	if first.Failed() {
		t.Error("a PREPARE_RESPONSE is not a failure")
	}

	// A null query (FETCH carries no SQL) must not become the string "<nil>".
	if entries[1].Query != "" {
		t.Errorf("Query = %q, want empty for a FETCH", entries[1].Query)
	}

	// The error row is marked as failed by either signal.
	failed := entries[2]
	if !failed.Failed() {
		t.Error("a row with an error and response_type ERROR should be failed")
	}
	if !strings.Contains(failed.Err, "Binder Error") {
		t.Errorf("Err = %q", failed.Err)
	}
	if _, ok := failed.Raw["log_level"]; !ok {
		t.Error("Raw should keep columns the struct does not model")
	}
}

func TestParseLogRowsEmpty(t *testing.T) {
	for _, in := range []string{"", "  ", "[]"} {
		entries, err := parseLogRows([]byte(in))
		if err != nil {
			t.Errorf("parseLogRows(%q): %v", in, err)
		}
		if len(entries) != 0 {
			t.Errorf("parseLogRows(%q) = %+v, want none", in, entries)
		}
	}
}

// Only Quack connections have a message log: a local file has none, and a
// DuckLake catalog is reached without the protocol.
func TestLogsViewTargets(t *testing.T) {
	clients := []*QuackClient{
		NewQuackClient(ServerConfig{Name: "local-dev", Type: ConnLocal, Path: "/tmp/x.duckdb"}, nil, nil),
		NewQuackClient(ServerConfig{Name: "quack-a", Type: ConnQuack, Host: "a", Port: 9494}, nil, nil),
		NewQuackClient(ServerConfig{Name: "lake", Type: ConnDuckLake, CatalogPath: "/tmp/c", StoragePath: "/tmp/d"}, nil, nil),
		NewQuackClient(ServerConfig{Name: "quack-b", Type: ConnQuack, Host: "b", Port: 9494}, nil, nil),
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
	none := NewLogsView([]*QuackClient{clients[0], clients[2]})
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
	v := NewLogsView([]*QuackClient{
		NewQuackClient(ServerConfig{Name: "q", Type: ConnQuack, Host: "h", Port: 9494}, nil, nil),
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
	v := NewLogsView([]*QuackClient{
		NewQuackClient(ServerConfig{Name: "q", Type: ConnQuack, Host: "h", Port: 9494}, nil, nil),
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
	entries, err := parseLogRows([]byte(quackLogRows))
	if err != nil {
		t.Fatal(err)
	}
	v := NewLogsView([]*QuackClient{
		NewQuackClient(ServerConfig{Name: "q", Type: ConnQuack, Host: "h", Port: 9494}, nil, nil),
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
	v := NewLogsView([]*QuackClient{
		NewQuackClient(ServerConfig{Name: "quack-a", Type: ConnQuack, Host: "h", Port: 9494}, nil, nil),
	})

	v, cmd := v.Update(logsEnabledMsg{target: "quack-a"})
	if v.noticeErr || !strings.Contains(v.notice, "logging enabled on quack-a") {
		t.Errorf("notice = %q (err=%v)", v.notice, v.noticeErr)
	}
	if cmd == nil {
		t.Error("a successful enable should refresh the log")
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
	entries, err := parseLogRows([]byte(quackLogRows))
	if err != nil {
		t.Fatal(err)
	}

	cfg := ServerConfig{Name: "quack-a", Type: ConnQuack, Host: "h", Port: 9494}
	m := Model{
		configs:     []ServerConfig{cfg},
		clients:     InitClients([]ServerConfig{cfg}, nil),
		data:        make([]connData, 1),
		currentView: viewLogs,
	}
	m.connTable = buildConnectionTable(nil)
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
