package main

import (
	"context"
	"os"
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
		msg, ok := c.QueryAsync(context.Background(), "SELECT no_such_column FROM analytics.orders;")().(queryResultMsg)
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

	t.Run("a prologue statement that emits output does not break the result", func(t *testing.T) {
		// This is what CI hit: with a storage secret in the prologue, CREATE
		// SECRET printed its own [{"Success":true}] ahead of the query's rows.
		// PRAGMA version stands in for it here — it emits a row array too, and
		// needs no extension download.
		r, err := c.queryCLI(context.Background(), "PRAGMA version; SELECT 42 AS answer;")
		if err != nil {
			t.Fatalf("multi-statement query failed: %v", err)
		}
		if len(r.Rows) != 1 || len(r.Columns) != 1 || r.Columns[0] != "answer" {
			t.Fatalf("got columns %v rows %v, want the last statement's result", r.Columns, r.Rows)
		}
		if r.Rows[0][0] != "42" {
			t.Errorf("row = %v, want 42", r.Rows[0])
		}
	})

	t.Run("successful query returns rows", func(t *testing.T) {
		msg := c.QueryAsync(context.Background(), "SELECT id FROM analytics.orders ORDER BY id LIMIT 3;")().(queryResultMsg)
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

	// -bail: a script on stdin does not abort on error the way `duckdb -c` does.
	// Without it, a failing prologue statement is reported and then the caller's
	// query runs anyway — against whatever catalog happened to be current.
	t.Run("a failing statement stops the script", func(t *testing.T) {
		_, err := c.queryCLI(context.Background(),
			"SELECT * FROM does_not_exist; SELECT 42 AS answer;")
		if err == nil {
			t.Fatal("want an error; the script should have stopped at the bad statement")
		}
		if strings.Contains(err.Error(), "42") {
			t.Errorf("the second statement ran after the failure: %v", err)
		}
	})
}

// -no-init: without it the operator's ~/.duckdbrc executes ahead of Pintail's
// prologue. A .duckdbrc only has to attach a database under one of Pintail's
// own aliases to break every query on the connection, and its output lands on
// stdout in the middle of the JSON being parsed.
func TestInitFileIsIgnoredAgainstRealDuckDB(t *testing.T) {
	if _, err := exec.LookPath("duckdb"); err != nil {
		t.Skip("duckdb not in PATH — skipping integration test")
	}

	home := t.TempDir()
	// The collision that matters: _lake is the alias Pintail's own DuckLake
	// prologue attaches, so this rc file makes every DuckLake query fail with
	// `database with name "_lake" already exists`.
	rc := "ATTACH ':memory:' AS _lake;\nSELECT 'init ran' AS note;\n"
	if err := os.WriteFile(filepath.Join(home, ".duckdbrc"), []byte(rc), 0o600); err != nil {
		t.Fatalf("writing .duckdbrc: %v", err)
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // windows

	dbPath := filepath.Join(t.TempDir(), "fixture.duckdb")
	// -no-init here too: seeding must not trip over the rc file we just wrote.
	if out, err := exec.Command("duckdb", "-no-init", dbPath, "-c", "CREATE TABLE t(a int);").CombinedOutput(); err != nil {
		t.Fatalf("seeding fixture: %v\n%s", err, out)
	}

	c := NewQuackClient(ServerConfig{Name: "fixture", Type: ConnLocal, Path: dbPath}, nil, nil)
	if _, err := c.Ping(context.Background()); err != nil {
		t.Fatalf("ping: %v", err)
	}

	r, err := c.queryCLI(context.Background(), "ATTACH ':memory:' AS _lake; SELECT 42 AS answer;")
	if err != nil {
		t.Fatalf("the init file was not skipped: %v", err)
	}
	if len(r.Rows) != 1 || len(r.Columns) != 1 || r.Columns[0] != "answer" {
		t.Fatalf("got columns %v rows %v, want only the query's own result", r.Columns, r.Rows)
	}
	if r.Rows[0][0] != "42" {
		t.Errorf("row = %v, want 42", r.Rows[0])
	}
}

// `duckdb -json -c` prints one JSON array per statement that produces a result,
// and every script Pintail sends is a prologue followed by the caller's
// statement. A CREATE SECRET in that prologue emits [{"Success":true}] of its
// own, which made the whole response unparseable — so every query on a
// connection with a storage secret came back as "unexpected response".
//
// This is the exact output CI saw before the fix.
func TestParserTakesTheLastStatementsOutput(t *testing.T) {
	const twoArrays = `[{"Success":true}]
[{"answer":42}]`

	r, err := parseJSONRows("SELECT answer FROM t;", []byte(twoArrays))
	if err != nil {
		t.Fatalf("parseJSONRows: %v", err)
	}
	if len(r.Rows) != 1 || r.Rows[0][0] != "42" {
		t.Fatalf("rows = %v, want the query's own result [[42]]", r.Rows)
	}
	if len(r.Columns) != 1 || r.Columns[0] != "answer" {
		t.Errorf("columns = %v, want [answer]", r.Columns)
	}
}

func TestLastJSONArray(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"single array is unchanged", `[{"a":1}]`, `[{"a":1}]`},
		{"prologue output is skipped", "[{\"Success\":true}]\n[{\"a\":1}]", `[{"a":1}]`},
		{"several prologue statements", "[]\n[{\"Success\":true}]\n[{\"a\":1}]", `[{"a":1}]`},
		// The last array wins even when empty: a query returning no rows must
		// not be reported as the prologue's output.
		{"empty final result stays empty", "[{\"Success\":true}]\n[]", `[]`},
		{"empty input", "", ""},
		// Anything unparseable comes back untouched so the caller can report it.
		{"not json", "Parser Error: syntax", "Parser Error: syntax"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := string(lastJSONArray([]byte(tc.in))); got != tc.want {
				t.Errorf("lastJSONArray(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// The same multi-array output reaches every other parser, since they all read a
// script with a prologue too.
func TestAllParsersSkipPrologueOutput(t *testing.T) {
	const prologue = "[{\"Success\":true}]\n"

	t.Run("catalog", func(t *testing.T) {
		got, err := parseCatalogRows([]byte(prologue +
			`[{"table_schema":"main","table_name":"t","estimated_size":5,"object_type":"table"}]`))
		if err != nil {
			t.Fatalf("parseCatalogRows: %v", err)
		}
		if len(got) != 1 || got[0].Tables[0].Name != "t" {
			t.Errorf("catalog = %+v", got)
		}
	})

	t.Run("sessions", func(t *testing.T) {
		conns, _, err := parseSessionRows([]byte(prologue+
			`[{"connection_id":"c1","state":"active"}]`), ServerConfig{Name: "s"})
		if err != nil {
			t.Fatalf("parseSessionRows: %v", err)
		}
		if len(conns) != 1 || conns[0].ID != "c1" {
			t.Errorf("conns = %+v", conns)
		}
	})

	t.Run("snapshots", func(t *testing.T) {
		snaps, err := parseSnapshotRows([]byte(prologue + `[{"snapshot_id":3,"schema_version":2}]`))
		if err != nil {
			t.Fatalf("parseSnapshotRows: %v", err)
		}
		if len(snaps) != 1 || snaps[0].ID != "3" {
			t.Errorf("snapshots = %+v", snaps)
		}
	})

	t.Run("logs", func(t *testing.T) {
		entries, err := parseLogRows([]byte(prologue + `[{"message_type":"PREPARE_REQUEST","query":"SELECT 1"}]`))
		if err != nil {
			t.Fatalf("parseLogRows: %v", err)
		}
		if len(entries) != 1 || entries[0].MessageType != "PREPARE_REQUEST" {
			t.Errorf("entries = %+v", entries)
		}
	})
}
