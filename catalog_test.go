package main

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseCatalogRows(t *testing.T) {
	const rows = `[
	  {"table_schema":"analytics","table_name":"orders","estimated_size":5000,"object_type":"table"},
	  {"table_schema":"analytics","table_name":"recent","estimated_size":null,"object_type":"view"},
	  {"table_schema":"main","table_name":"events","estimated_size":0,"object_type":"table"}
	]`

	got, err := parseCatalogRows([]byte(rows))
	if err != nil {
		t.Fatalf("parseCatalogRows: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d schemas, want 2 (analytics, main)", len(got))
	}

	// Schema order follows first appearance in the result set.
	if got[0].Name != "analytics" || got[1].Name != "main" {
		t.Errorf("schema order = %q, %q; want analytics, main", got[0].Name, got[1].Name)
	}

	orders, recent := got[0].Tables[0], got[0].Tables[1]
	if orders.Format != "table" || orders.Rows != 5000 || !orders.SizeKnown {
		t.Errorf("orders = %+v, want a table of 5000 rows with a known size", orders)
	}
	if recent.Format != "view" || recent.SizeKnown {
		t.Errorf("recent = %+v, want a view with no size estimate", recent)
	}

	// An explicit zero is a real estimate and must not be confused with "no
	// estimate available" — that distinction is what SizeKnown exists for.
	events := got[1].Tables[0]
	if !events.SizeKnown || events.Rows != 0 {
		t.Errorf("events = %+v, want a known size of 0", events)
	}
}

func TestParseCatalogRowsEmpty(t *testing.T) {
	for _, in := range []string{"", "  ", "[]"} {
		if _, err := parseCatalogRows([]byte(in)); err == nil {
			t.Errorf("parseCatalogRows(%q): want error, got nil", in)
		}
	}
}

// The catalog and session queries are only as good as the SQL they contain,
// and the previous versions referenced a column and a function that do not
// exist (information_schema.tables.estimated_size, duckdb_connections()).
// Those failures are invisible in unit tests, so this exercises both against
// a real duckdb binary. Skipped when duckdb is not installed.
func TestMetadataQueriesAgainstRealDuckDB(t *testing.T) {
	if _, err := exec.LookPath("duckdb"); err != nil {
		t.Skip("duckdb not in PATH — skipping integration test")
	}

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "fixture.duckdb")

	seed := `CREATE SCHEMA analytics;
	         CREATE TABLE analytics.orders AS SELECT range AS id FROM range(5000);
	         CREATE TABLE main.events AS SELECT range AS id FROM range(100);
	         CREATE VIEW main.recent AS SELECT * FROM main.events LIMIT 10;`
	if out, err := exec.Command("duckdb", dbPath, "-c", seed).CombinedOutput(); err != nil {
		t.Fatalf("seeding fixture: %v\n%s", err, out)
	}

	cfg := ServerConfig{Name: "fixture", Type: ConnLocal, Path: dbPath}
	c := NewQuackClient(cfg, nil, nil)
	if !c.HasCLI() {
		t.Fatal("client did not find the duckdb CLI")
	}
	if _, err := c.Ping(context.Background()); err != nil {
		t.Fatalf("ping: %v", err)
	}

	t.Run("catalog", func(t *testing.T) {
		msg, ok := c.FetchCatalogCmd(0)().(catalogResultMsg)
		if !ok {
			t.Fatal("FetchCatalogCmd returned the wrong message type")
		}
		if msg.err != nil {
			t.Fatalf("catalog fetch failed: %v", msg.err)
		}

		found := map[string]CatalogTable{}
		for _, schema := range msg.catalog {
			for _, tbl := range schema.Tables {
				found[schema.Name+"."+tbl.Name] = tbl
			}
		}
		if got, want := len(found), 3; got != want {
			t.Fatalf("got %d relations %v, want %d", got, found, want)
		}
		if tbl := found["analytics.orders"]; tbl.Rows != 5000 || !tbl.SizeKnown {
			t.Errorf("analytics.orders = %+v, want 5000 rows", tbl)
		}
		if tbl := found["main.recent"]; tbl.Format != "view" {
			t.Errorf("main.recent = %+v, want it reported as a view", tbl)
		}
	})

	t.Run("sessions", func(t *testing.T) {
		// A local connection reports its own session without going to SQL, so
		// drive the Quack path's query directly against the fixture file.
		quackLike := ServerConfig{Name: "quack-like", Type: ConnQuack}
		qc := NewQuackClient(quackLike, nil, nil)
		qc.state = ConnState{Online: true}

		sql := `SELECT current_connection_id() AS connection_id,
		               session_user()          AS client_context,
		               current_database()      AS catalog,
		               (SELECT count FROM duckdb_connection_count()) AS connection_count;`
		out, err := exec.Command("duckdb", dbPath, "-json", "-c", sql).CombinedOutput()
		if err != nil {
			t.Fatalf("session query is not valid SQL for this duckdb: %v\n%s", err, out)
		}

		conns, reported, err := parseSessionRows(out, quackLike)
		if err != nil {
			t.Fatalf("parseSessionRows: %v\n%s", err, out)
		}
		if len(conns) != 1 {
			t.Fatalf("got %d connections, want 1", len(conns))
		}
		if reported == "" {
			t.Error("backend reported no connection count")
		}
		if conns[0].Status != "active" {
			t.Errorf("status = %q, want active", conns[0].Status)
		}
	})

	t.Run("queries surface real errors", func(t *testing.T) {
		msg, ok := c.QueryAsync("SELECT no_such_column FROM analytics.orders;", cfg.ToServerInfo())().(queryResultMsg)
		if !ok {
			t.Fatal("QueryAsync returned the wrong message type")
		}
		if msg.result.Err == "" {
			t.Fatal("want an error for an invalid column")
		}
		// The DuckDB message must reach the user, not a transport guess.
		if !strings.Contains(msg.result.Err, "no_such_column") {
			t.Errorf("Err = %q, want it to name the offending column", msg.result.Err)
		}
		if strings.Contains(msg.result.Err, "no endpoint responded") {
			t.Errorf("Err = %q, HTTP fallback should not apply to local connections", msg.result.Err)
		}
	})

	t.Run("successful query returns rows", func(t *testing.T) {
		msg := c.QueryAsync("SELECT id FROM analytics.orders ORDER BY id LIMIT 3;", cfg.ToServerInfo())().(queryResultMsg)
		if msg.result.Err != "" {
			t.Fatalf("unexpected error: %s", msg.result.Err)
		}
		if len(msg.result.Rows) != 3 {
			t.Fatalf("got %d rows, want 3", len(msg.result.Rows))
		}
		if msg.result.Method != "cli" {
			t.Errorf("method = %q, want cli", msg.result.Method)
		}
		if msg.result.Rows[0][0] != "0" {
			t.Errorf("first row = %v, want id 0", msg.result.Rows[0])
		}
	})
}
