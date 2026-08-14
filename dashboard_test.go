package main

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// threeConnModel builds a dashboard with three connections and no metadata yet.
func threeConnModel(t *testing.T) Model {
	t.Helper()
	configs := []ServerConfig{
		{Name: "central", Type: ConnQuack, Host: "a", Port: 9494},
		{Name: "lake-prod", Type: ConnDuckLake, CatalogPath: "/tmp/c.duckdb", StoragePath: "/tmp/d"},
		{Name: "local-dev", Type: ConnLocal, Path: "/tmp/x.duckdb"},
	}
	m := Model{
		configs:     configs,
		clients:     InitClients(configs, nil),
		wasOnline:   make([]bool, len(configs)),
		data:        make([]connData, len(configs)),
		currentView: viewDashboard,
		width:       120,
		height:      40,
	}
	m.connTable = buildConnectionTable(nil)
	return m
}

func sessionsFor(names ...string) []Connection {
	out := make([]Connection, 0, len(names))
	for _, n := range names {
		out = append(out, Connection{ID: n, Identity: n, Status: "active"})
	}
	return out
}

// Results from different connections used to share one global slot, so the last
// responder won and the panels described a server the user had not selected.
func TestSessionResultsAreStoredPerConnection(t *testing.T) {
	m := threeConnModel(t)

	next, _ := m.Update(sessionResultMsg{idx: 0, connections: sessionsFor("a1", "a2"), reportedCount: "2"})
	next, _ = next.(Model).Update(sessionResultMsg{idx: 2, connections: sessionsFor("c1"), reportedCount: "1"})
	m = next.(Model)

	if got := len(m.data[0].sessions); got != 2 {
		t.Errorf("connection 0 has %d sessions, want 2 — a later result clobbered it", got)
	}
	if got := len(m.data[2].sessions); got != 1 {
		t.Errorf("connection 2 has %d sessions, want 1", got)
	}
	if got := len(m.data[1].sessions); got != 0 {
		t.Errorf("connection 1 has %d sessions, want none — it never reported", got)
	}
	if m.data[0].reportedCount != "2" || m.data[2].reportedCount != "1" {
		t.Errorf("reported counts crossed over: %q and %q",
			m.data[0].reportedCount, m.data[2].reportedCount)
	}
}

func TestCatalogResultsAndErrorsAreStoredPerConnection(t *testing.T) {
	m := threeConnModel(t)

	good := []CatalogSchema{{Name: "analytics", Tables: []CatalogTable{{Name: "orders"}}}}
	next, _ := m.Update(catalogResultMsg{idx: 1, catalog: good})
	next, _ = next.(Model).Update(catalogResultMsg{idx: 0, err: errString("Binder Error: nope")})
	m = next.(Model)

	if len(m.data[1].catalog) != 1 || m.data[1].catalog[0].Name != "analytics" {
		t.Errorf("connection 1 catalog = %+v, want the analytics schema", m.data[1].catalog)
	}
	if m.data[1].catalogErr != "" {
		t.Errorf("connection 1 picked up connection 0's error: %q", m.data[1].catalogErr)
	}
	if !strings.Contains(m.data[0].catalogErr, "Binder Error") {
		t.Errorf("connection 0 error = %q, want the binder error", m.data[0].catalogErr)
	}
	if len(m.data[0].catalog) != 0 {
		t.Error("connection 0 should have no catalog data")
	}

	// A failed refresh keeps the last good listing rather than blanking it.
	next, _ = m.Update(catalogResultMsg{idx: 1, err: errString("transient failure")})
	m = next.(Model)
	if len(m.data[1].catalog) != 1 {
		t.Error("a failed refresh discarded the last known-good catalog")
	}
	if !strings.Contains(m.data[1].catalogErr, "transient") {
		t.Errorf("error not recorded: %q", m.data[1].catalogErr)
	}
}

// Late results from a connection that has been deleted must not be applied to
// whatever now occupies that index.
func TestResultsForUnknownConnectionsAreDropped(t *testing.T) {
	m := threeConnModel(t)

	for _, idx := range []int{-1, 3, 99} {
		next, _ := m.Update(sessionResultMsg{idx: idx, connections: sessionsFor("ghost")})
		m = next.(Model)
		next, _ = m.Update(catalogResultMsg{idx: idx, catalog: []CatalogSchema{{Name: "ghost"}}})
		m = next.(Model)
	}

	for i, d := range m.data {
		if len(d.sessions) != 0 || len(d.catalog) != 0 {
			t.Errorf("connection %d picked up data from an out-of-range result: %+v", i, d)
		}
	}
}

func TestDashboardConnectionSelection(t *testing.T) {
	m := threeConnModel(t)
	m.data[0].sessions = sessionsFor("a1", "a2")
	m.data[1].sessions = sessionsFor("b1")
	m.data[2].sessions = sessionsFor("c1", "c2", "c3")

	tests := []struct {
		key      string
		wantIdx  int
		wantName string
	}{
		{"]", 1, "lake-prod"},
		{"]", 2, "local-dev"},
		{"]", 0, "central"},   // wraps
		{"[", 2, "local-dev"}, // wraps backwards
		{"2", 1, "lake-prod"}, // direct dial
		{"1", 0, "central"},
		{"9", 0, "central"}, // out of range: ignored
	}

	for _, tc := range tests {
		next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(tc.key)})
		m = next.(Model)
		if m.selected != tc.wantIdx {
			t.Fatalf("after %q selected = %d, want %d", tc.key, m.selected, tc.wantIdx)
		}
		if got := m.selectedName(); got != tc.wantName {
			t.Errorf("after %q selectedName = %q, want %q", tc.key, got, tc.wantName)
		}
		// The table follows the selection.
		if got, want := len(m.connTable.Rows()), len(m.data[tc.wantIdx].sessions); got != want {
			t.Errorf("after %q table has %d rows, want %d", tc.key, got, want)
		}
	}
}

// The panels have to name the connection they describe, otherwise a table of
// sessions says nothing about whose sessions it is.
func TestDashboardPanelsNameTheirConnection(t *testing.T) {
	m := threeConnModel(t)
	m.data[1].sessions = sessionsFor("b1")
	m.data[1].reportedCount = "4"
	m.data[1].catalog = []CatalogSchema{{Name: "analytics", Open: true,
		Tables: []CatalogTable{{Name: "orders", Format: "table", Rows: 10, SizeKnown: true}}}}
	m.selectConnection(1)

	conns := m.viewConnectionsPanel(70, 20)
	if !strings.Contains(conns, "lake-prod") {
		t.Errorf("connections panel does not name the connection:\n%s", conns)
	}
	if !strings.Contains(conns, "backend reports 4") {
		t.Errorf("connections panel does not show the reported count:\n%s", conns)
	}

	cat := m.viewCatalogPanel(50, 20)
	if !strings.Contains(cat, "lake-prod") || !strings.Contains(cat, "orders") {
		t.Errorf("catalog panel does not name the connection or its tables:\n%s", cat)
	}

	// The header marks which connection is selected and shows its digit.
	header := m.viewHeader()
	for _, want := range []string{"lake-prod", "2:"} {
		if !strings.Contains(header, want) {
			t.Errorf("header missing %q:\n%s", want, header)
		}
	}
}

// Deleting a connection has to drop its metadata, or the remaining entries
// describe the wrong servers.
func TestDeletingAConnectionRealignsData(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	m := threeConnModel(t)
	m.data[0].sessions = sessionsFor("a1")
	m.data[1].sessions = sessionsFor("b1", "b2")
	m.data[2].sessions = sessionsFor("c1", "c2", "c3")
	m.scratchpad = NewScratchpad(nil, nil)
	m.selected = 2

	// Delete the middle connection from the connection manager's list panel.
	m.addForm = newAddServerForm()
	m.addForm.focusIdx = -2
	m.addForm.listCursor = 1
	next, _ := m.updateAddServer(key("d"))
	m = next.(Model)

	if len(m.configs) != 2 || m.configs[0].Name != "central" || m.configs[1].Name != "local-dev" {
		t.Fatalf("configs after delete = %+v", m.configs)
	}
	if len(m.data) != 2 {
		t.Fatalf("data has %d entries, want 2", len(m.data))
	}
	if len(m.data[0].sessions) != 1 {
		t.Errorf("central's sessions changed: %+v", m.data[0].sessions)
	}
	if got := len(m.data[1].sessions); got != 3 {
		t.Errorf("local-dev has %d sessions, want its own 3 — data did not shift with the configs", got)
	}
	if m.selected != 1 {
		t.Errorf("selected = %d, want it clamped to 1", m.selected)
	}
}

func TestAddingAConnectionExtendsData(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	m := threeConnModel(t)
	m.scratchpad = NewScratchpad(nil, nil)
	m.data[0].sessions = sessionsFor("a1")

	m.addForm = newAddServerForm()
	m.addForm.connType = ConnLocal
	m.addForm.name = "new-one"
	m.addForm.path = "/tmp/new.duckdb"
	m.addForm.focusIdx = len(m.addForm.visibleFields()) - 1
	next, _ := m.updateAddServer(key("enter"))
	m = next.(Model)

	if len(m.configs) != 4 {
		t.Fatalf("configs = %d, want 4", len(m.configs))
	}
	if len(m.data) != 4 {
		t.Errorf("data = %d entries, want 4 so the new connection has somewhere to report", len(m.data))
	}
	if len(m.data[0].sessions) != 1 {
		t.Error("existing metadata was disturbed by the add")
	}
}

// The whole dashboard has to render at any terminal size, with and without
// metadata — the header gained per-connection markers and both panels gained
// titles, all of which do width arithmetic.
func TestDashboardRendersAtAnySize(t *testing.T) {
	populated := threeConnModel(t)
	populated.data[0].sessions = sessionsFor("a1", "a2")
	populated.data[0].reportedCount = "2"
	populated.data[1].catalogErr = "Catalog Error: something went wrong at length"
	populated.data[2].catalog = []CatalogSchema{{Name: "main", Open: true,
		Tables: []CatalogTable{{Name: "events", Format: "table", Rows: 100, SizeKnown: true}}}}

	for _, m := range []Model{threeConnModel(t), populated} {
		for _, size := range [][2]int{{20, 10}, {40, 15}, {80, 24}, {120, 40}, {300, 80}} {
			m.width, m.height = size[0], size[1]
			for sel := 0; sel < len(m.configs); sel++ {
				m.selected = sel
				if out := m.View(); out == "" {
					t.Fatalf("dashboard rendered nothing at %dx%d", size[0], size[1])
				}
			}
		}
	}
}

// errString adapts a message to the error interface for table-driven tests.
type errString string

func (e errString) Error() string { return string(e) }
