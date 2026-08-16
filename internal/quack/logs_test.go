package quack

import (
	"os"
	"path/filepath"
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
