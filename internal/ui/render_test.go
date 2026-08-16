package ui

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/CathalByrneGit/pintail/internal/quack"
)

// Every screen, at every width, with data in it.
//
// This is the test that would have caught both of the crashes this project has
// had: ViewTokenList sliced scopeStr[:width-21] and took the app down at 80
// columns, and the scratchpad indexed a stale server list after a connection was
// deleted. Both were in view code, which was the least-covered part of the
// codebase — a panel is only exercised when someone opens it at that size.
//
// Two properties are checked at each size:
//
//   - it renders at all, rather than panicking on a negative slice bound;
//   - nothing overflows the terminal width, because a line wider than the
//     terminal wraps and shunts the rest of the layout down the screen.
//
// Widths go down to 20 deliberately. Nobody runs a 20-column terminal, but
// panel widths are derived by subtracting borders and padding from the total, so
// a small total is how those intermediate values go negative.

// screenSizes covers the narrow end where arithmetic goes negative, the common
// 80 and 120, and a very wide terminal.
var screenSizes = [][2]int{
	{20, 10}, {40, 15}, {60, 20}, {80, 24}, {100, 30}, {120, 40}, {200, 50}, {300, 80},
}

// populatedModel returns a Model with every screen's state filled in, so the
// views render real rows rather than empty states — empty panels are exactly the
// case that never crashes.
func populatedModel(t *testing.T) Model {
	t.Helper()

	cfgs := []quack.ServerConfig{
		{Name: "quack-prod", Type: quack.ConnQuack, Host: "quack.internal.example.com", Port: 9494,
			Token: "qk_" + strings.Repeat("a", 48), TLS: true},
		{Name: "lake-with-a-deliberately-long-name", Type: quack.ConnDuckLake,
			CatalogRef: "quack-prod", StoragePath: "s3://a-bucket-name/with/a/long/prefix",
			StorageSecretRef: "lake_s3"},
		{Name: "local", Type: quack.ConnLocal, Path: "/var/lib/duckdb/analytics.duckdb"},
	}
	secrets := []quack.StorageSecret{{
		Name: "lake_s3", Type: quack.SecretS3, KeyID: "AKIAIOSFODNN7EXAMPLE",
		Secret: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", Region: "eu-west-1",
		Scope: "s3://a-bucket-name/with/a/long/prefix",
	}}
	tokens := []quack.Token{
		quack.BuildToken("etl-pipeline-production", "analytics, raw, staging", "SELECT, INSERT", "2030-01-15"),
		quack.BuildToken("readonly", "*", "SELECT", "never"),
	}

	m := Model{
		configs:     cfgs,
		clients:     quack.InitClients(cfgs, secrets),
		currentView: viewDashboard,
		width:       120,
		height:      40,
	}
	m.data = make([]connData, len(cfgs))
	m.connTable = buildConnectionTable(nil, 0)
	m.tokenMgr = TokenManager{tokens: tokens, secrets: secrets}
	m.tlsGen = NewTLSGenerator(cfgs)
	m.authEditor = NewAuthEditor(tokens, m.clients)
	m.snapshots = NewSnapshotsView(m.clients)
	m.logs = NewLogsView(m.clients)
	m.scratchpad = NewScratchpad(serverInfos(cfgs), m.clients)

	// The add-connection screen is only reachable with a form open, so give it
	// one — with long values, which is what makes the fields overflow.
	m.addForm = newAddServerForm()
	m.addForm.name = "a-connection-name-that-is-really-quite-long"
	m.addForm.host = "quack.some.long.internal.hostname.example.com"
	m.addForm.token = "qk_" + strings.Repeat("b", 48)
	m.addForm.storagePath = "s3://bucket/with/a/deeply/nested/prefix/for/the/lake"

	// Sessions and catalog for the dashboard panels.
	m.data[0].sessions = []quack.Connection{
		{ID: "091A0035", IP: "10.0.1.5", Identity: "analyst-with-a-long-name", Catalog: "_remote",
			Status: "active", Duration: 3 * time.Second, Queries: 42,
			Query: "SELECT count(*) FROM analytics.orders WHERE ts > now() - INTERVAL 1 DAY GROUP BY ALL"},
		{ID: "091A0036", IP: "10.0.1.6", Identity: "etl", Catalog: "_remote", Status: "idle"},
		{ID: "091A0037", IP: "10.0.1.7", Identity: "日本語のユーザー", Catalog: "_remote", Status: "cancelled"},
	}
	m.data[0].reportedCount = "3"
	m.data[0].catalog = []quack.CatalogSchema{{
		Name: "analytics",
		Tables: []quack.CatalogTable{
			{Name: "orders", Format: "table", Rows: 4_800_000, SizeKnown: true},
			{Name: "a_view_with_a_very_long_name_indeed", Format: "view"},
		},
	}}

	// Snapshots and log entries.
	m.snapshots.snapshots = []quack.Snapshot{{
		ID: "7", Time: "2026-08-14 09:06:19.841623+02", SchemaVersion: "3", Author: "etl",
		Raw: map[string]interface{}{"snapshot_id": 7, "schema_version": 3},
	}}
	entries, err := quack.ParseLogRows(quackLogRows(t))
	if err != nil {
		t.Fatalf("parsing the log fixture: %v", err)
	}
	m.logs.entries = entries

	// A query result in the scratchpad, with a wide-character column.
	m.scratchpad.result = &quack.QueryResult{
		Query:   "SELECT * FROM analytics.orders LIMIT 3",
		Columns: []string{"order_id", "customer", "amount", "note"},
		Rows: [][]string{
			{"1", "日本語のお客様", "42.50", "a reasonably long free-text note"},
			{"2", "ACME Corp", "1000.00", ""},
		},
		ElapsedMs: 12,
		Method:    "cli",
	}

	return m
}

// allScreens names each view so a failure says which screen broke.
func allScreens() map[string]appView {
	return map[string]appView{
		"dashboard":  viewDashboard,
		"tokens":     viewTokens,
		"scratchpad": viewScratchpad,
		"addServer":  viewAddServer,
		"tls":        viewTLS,
		"auth":       viewAuth,
		"snapshots":  viewSnapshots,
		"logs":       viewLogs,
	}
}

func TestEveryScreenRendersAtEveryWidth(t *testing.T) {
	for name, view := range allScreens() {
		t.Run(name, func(t *testing.T) {
			for _, size := range screenSizes {
				w, h := size[0], size[1]
				m := populatedModel(t)
				m.currentView = view
				m.width, m.height = w, h
				m.scratchpad.Resize(w, h)
				m.tlsGen.SetWidth(w)

				out := m.View() // panics here are the first thing being checked
				if out == "" {
					t.Errorf("%s rendered nothing at %dx%d", name, w, h)
					continue
				}
				// Report the widest line, not the first over-wide one.
				// lipgloss.JoinVertical pads every line out to the width of the
				// widest, so the first offender is usually the header being
				// padded to match a body line — which is not where the bug is.
				widest, widestLine := 0, ""
				for _, line := range strings.Split(out, "\n") {
					if got := ansi.StringWidth(line); got > widest {
						widest, widestLine = got, line
					}
				}
				if widest > w {
					t.Errorf("%s at %dx%d: widest line is %d cells, overflowing by %d:\n%q",
						name, w, h, widest, widest-w, widestLine)
				}
			}
		})
	}
}

// The same sweep with everything empty. The populated case exercises the
// arithmetic; this one catches views that assume they have at least one row.
func TestEveryScreenRendersWithNoData(t *testing.T) {
	for name, view := range allScreens() {
		t.Run(name, func(t *testing.T) {
			for _, size := range screenSizes {
				w, h := size[0], size[1]
				m := NewModelWithConfigs(nil, nil)
				m.currentView = view
				m.width, m.height = w, h
				m.scratchpad.Resize(w, h)
				m.tlsGen.SetWidth(w)

				if out := m.View(); out == "" {
					t.Errorf("%s rendered nothing at %dx%d with no connections", name, w, h)
				}
			}
		})
	}
}

// Every sub-panel that takes a width, called directly with the awkward values a
// narrow terminal produces. The screens above compose these; calling them
// directly pins down which one is at fault when a width goes negative.
func TestPanelsSurviveNegativeWidths(t *testing.T) {
	m := populatedModel(t)

	panels := map[string]func(w int) string{
		"connections":     func(w int) string { return m.viewConnectionsPanel(w, 10) },
		"catalog":         func(w int) string { return m.viewCatalogPanel(w, 10) },
		"tokenList":       func(w int) string { return m.tokenMgr.ViewTokenList(w, 10) },
		"tokenDetail":     func(w int) string { return m.tokenMgr.ViewTokenDetail(w) },
		"tokenForm":       func(w int) string { return m.tokenMgr.ViewForm(w, 10) },
		"secretList":      func(w int) string { return m.tokenMgr.ViewSecretList(w, 10) },
		"secretDetail":    func(w int) string { return m.tokenMgr.ViewSecretDetail(w) },
		"permGrid":        func(w int) string { return m.authEditor.ViewPermGrid(w) },
		"policyList":      func(w int) string { return m.authEditor.ViewPolicyList(w, 10) },
		"snapshotList":    func(w int) string { return m.snapshots.ViewList(w) },
		"snapshotDetail":  func(w int) string { return m.snapshots.ViewDetail(w) },
		"logTable":        func(w int) string { return m.logs.ViewTable(w) },
		"logDetail":       func(w int) string { return m.logs.ViewDetail(w) },
		"tlsConfig":       func(w int) string { m.tlsGen.SetWidth(w); return m.tlsGen.ViewConfig() },
		"scratchResults":  func(w int) string { return renderResultTable(*m.scratchpad.result, w) },
		"scratchEditor":   func(w int) string { m.scratchpad.Resize(w, 20); return m.scratchpad.ViewEditor() },
		"scratchFooter":   func(w int) string { m.scratchpad.Resize(w, 20); return m.scratchpad.ViewFooter() },
		"dashboardFooter": func(w int) string { m.width = w; return m.viewDashboardFooter() },
		"header":          func(w int) string { m.width = w; return m.viewHeader() },
	}

	// Including 0 and negatives: these are what the layout arithmetic produces
	// from a narrow terminal, and they used to be passed straight into a slice.
	for _, w := range []int{-40, -1, 0, 1, 2, 5, 10, 21, 40, 79, 80, 200} {
		for name, render := range panels {
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Errorf("%s panicked at width %d: %v", name, w, r)
					}
				}()
				_ = render(w)
			}()
		}
	}
}

// The session table used to be built with fixed column widths summing to about
// 87 cells including the table's own padding, against a panel roughly 64 wide on
// a 110-column terminal — so its header and rule wrapped inside the panel on any
// ordinary size. Columns are now chosen to fit.
func TestConnectionTableFitsItsPanel(t *testing.T) {
	for _, total := range []int{60, 80, 100, 110, 120, 160, 200, 300} {
		m := populatedModel(t)
		m.width, m.height = total, 40
		avail := m.connPanelWidth()

		cols := connectionColumns(avail)
		if len(cols) == 0 {
			t.Errorf("width %d: no columns at all", total)
			continue
		}

		sum := 0
		for _, c := range cols {
			sum += c.Width + tableCellPadding
		}
		if sum > avail {
			t.Errorf("terminal %d (panel %d): columns total %d cells, overflowing by %d: %v",
				total, avail, sum, sum-avail, cols)
		}

		// The two columns that identify a row and its state are never dropped —
		// a table of durations with no id is useless.
		var titles []string
		for _, c := range cols {
			titles = append(titles, c.Title)
		}
		for _, required := range []string{"ID", "Status"} {
			found := false
			for _, got := range titles {
				if got == required {
					found = true
				}
			}
			if !found {
				t.Errorf("terminal %d: %q was dropped; columns are %v", total, required, titles)
			}
		}
	}

	// A wide terminal keeps the full set.
	wide := connectionColumns(200)
	if len(wide) != len(connColumns) {
		t.Errorf("a 200-cell panel should keep every column, got %d of %d", len(wide), len(connColumns))
	}

	// Not laid out yet: the full set at preferred widths, rather than nothing.
	if got := connectionColumns(0); len(got) != len(connColumns) {
		t.Errorf("width 0 should give the full set, got %d columns", len(got))
	}
}

// The panel renders the table, so no line inside it may exceed the panel either
// — the check above is on the arithmetic, this one on the result.
func TestConnectionsPanelDoesNotWrapInternally(t *testing.T) {
	for _, total := range []int{80, 100, 110, 120, 160} {
		m := populatedModel(t)
		m.width, m.height = total, 40
		m.connTable.SetColumns(connectionColumns(m.connPanelWidth()))
		m.connTable.SetRows(connectionRows(m.data[0].sessions, m.connPanelWidth()))

		leftW := (total * 60) / 100
		out := m.viewConnectionsPanel(leftW, 20)
		for i, line := range strings.Split(out, "\n") {
			if got := ansi.StringWidth(line); got > leftW {
				t.Errorf("terminal %d: panel line %d is %d cells, wider than the %d-cell panel:\n%q",
					total, i+1, got, leftW, line)
				break
			}
		}
	}
}
