package quack

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestParseLogRows(t *testing.T) {
	entries, err := ParseLogRows(quackLogRows(t))
	if err != nil {
		t.Fatalf("ParseLogRows: %v", err)
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
		entries, err := ParseLogRows([]byte(in))
		if err != nil {
			t.Errorf("ParseLogRows(%q): %v", in, err)
		}
		if len(entries) != 0 {
			t.Errorf("ParseLogRows(%q) = %+v, want none", in, entries)
		}
	}
}

// Only Quack connections have a message log: a local file has none, and a
// DuckLake catalog is reached without the protocol.

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

// The log screen is only useful if the entries can be read back, and in the
// duckdb CLI the default log storage is the console: enable_logging succeeds,
// the process prints its log lines to stdout, and duckdb_logs stays empty. This
// pins both reasons the statement names a storage, against the real binary.
func TestEnableLoggingUsesAReadableStorageAgainstRealDuckDB(t *testing.T) {
	if _, err := exec.LookPath("duckdb"); err != nil {
		t.Skip("duckdb not in PATH")
	}

	run := func(enable string) (rows int, raw string, err error) {
		sql := enable + "; SELECT 1 AS x; SELECT count(*) AS n FROM duckdb_logs;"
		out, cmdErr := exec.Command("duckdb", "-no-init", "-json", "-c", sql).Output()
		if cmdErr != nil {
			return 0, string(out), cmdErr
		}
		n, parseErr := firstStringValue(out, "n")
		if parseErr != nil {
			return 0, string(out), parseErr
		}
		v, convErr := strconv.Atoi(n)
		return v, string(out), convErr
	}

	// The bare call is what Pintail used to send. Two things go wrong with it,
	// and either one is enough to make the panel useless.
	rows, raw, err := run("CALL enable_logging()")
	switch {
	case err != nil:
		// The console storage writes log lines into stdout, which is the same
		// stream the -json results arrive on — so the output stops being
		// parseable at all. Worth knowing: it means a server logging to its
		// console corrupts every reply Pintail reads from it.
		if !strings.Contains(raw, "logging settings have been changed") {
			t.Errorf("unexpected failure for the bare call: %v\n%s", err, raw)
		}
	case rows == 0:
		// Parsed fine, but the log is empty: the entries went to the console.
	default:
		t.Logf("the bare call yielded %d readable rows on this build", rows)
	}

	// Naming a queryable storage is what makes the panel possible, and keeps
	// stdout clean enough to parse.
	rows, raw, err = run("CALL enable_logging(storage = 'memory')")
	if err != nil {
		t.Fatalf("storage = 'memory' should parse cleanly: %v\n%s", err, raw)
	}
	if rows == 0 {
		t.Error("with storage = 'memory' the log must be readable from duckdb_logs")
	}

	// And the statement Pintail actually sends has to name one.
	if !strings.Contains(enableLoggingSQL, "storage") {
		t.Errorf("enableLoggingSQL does not name a storage, so the log will be unreadable: %s",
			enableLoggingSQL)
	}
}
