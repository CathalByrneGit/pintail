package quack

import (
	"strings"
	"testing"
)

// The real shape of a quack_active_connections() row, as the quack extension
// defines it: server_id, connection_id, query, state, query_started_at.
func TestParseQuackActiveConnections(t *testing.T) {
	cfg := ServerConfig{Name: "prod", Type: ConnQuack, Host: "catalog.internal", Port: 9494}

	const rows = `[
	  {"server_id":"srv-a","connection_id":"c0128afe-11","query":"SELECT count(*) FROM orders","state":"active","query_started_at":"2026-08-14 12:00:00"},
	  {"server_id":"srv-a","connection_id":"7","query":"","state":"idle","query_started_at":null},
	  {"server_id":"srv-b","connection_id":"9","query":"VACUUM","state":"cancelled","query_started_at":"2026-08-14 11:59:00"}
	]`

	conns, reported, err := parseSessionRows([]byte(rows), cfg)
	if err != nil {
		t.Fatalf("parseSessionRows: %v", err)
	}
	if reported != "" {
		t.Errorf("reportedCount = %q, want empty — these rows carry no count", reported)
	}
	if len(conns) != 3 {
		t.Fatalf("got %d connections, want 3", len(conns))
	}

	// Row 1: running query.
	if conns[0].ID != "c012" {
		t.Errorf("ID = %q, want it cut to the column width", conns[0].ID)
	}
	if conns[0].Identity != "srv-a" {
		t.Errorf("Identity = %q, want the server id", conns[0].Identity)
	}
	if conns[0].Status != "active" {
		t.Errorf("Status = %q, want the reported state", conns[0].Status)
	}
	if conns[0].Query != "SELECT count(*) FROM orders" {
		t.Errorf("Query = %q, want the running SQL", conns[0].Query)
	}
	if conns[0].Duration <= 0 {
		t.Errorf("Duration = %v, want it derived from query_started_at", conns[0].Duration)
	}
	if conns[0].IP != cfg.Host {
		t.Errorf("IP = %q, want the configured host", conns[0].IP)
	}

	// Row 2: idle, so no query and no start time to measure from.
	if conns[1].Status != "idle" {
		t.Errorf("Status = %q, want idle", conns[1].Status)
	}
	if conns[1].Query != "" {
		t.Errorf("Query = %q, want empty for an idle session", conns[1].Query)
	}
	if conns[1].Duration != 0 {
		t.Errorf("Duration = %v, want zero when query_started_at is null", conns[1].Duration)
	}

	// Row 3: the states quack_active_connections() reports are passed through
	// verbatim. Whether the table gives each one a glyph is the UI's business
	// and is asserted there.
	if conns[2].Status != "cancelled" {
		t.Errorf("Status = %q, want cancelled", conns[2].Status)
	}
}

// Rows in the older shape must still parse, so a backend reporting the previous
// field names doesn't suddenly show an empty panel.
func TestParseSessionRowsAcceptsLegacyFields(t *testing.T) {
	cfg := ServerConfig{Name: "prod", Host: "10.0.0.1"}
	const rows = `[{"connection_id":3,"client_context":"analyst1","catalog":"_remote",
	                "connected_since":"2026-08-14T12:00:00Z","connection_count":"4"}]`

	conns, reported, err := parseSessionRows([]byte(rows), cfg)
	if err != nil {
		t.Fatalf("parseSessionRows: %v", err)
	}
	if reported != "4" {
		t.Errorf("reportedCount = %q, want 4", reported)
	}
	if len(conns) != 1 {
		t.Fatalf("got %d connections, want 1", len(conns))
	}
	if conns[0].Identity != "analyst1" {
		t.Errorf("Identity = %q, want the client context", conns[0].Identity)
	}
	if conns[0].Catalog != "_remote" {
		t.Errorf("Catalog = %q", conns[0].Catalog)
	}
	if conns[0].Duration <= 0 {
		t.Errorf("Duration = %v, want it derived from connected_since", conns[0].Duration)
	}
	// No state reported: default to active rather than blank.
	if conns[0].Status != "active" {
		t.Errorf("Status = %q, want active by default", conns[0].Status)
	}
}

// The session query has to reach the server, which means quack_query() rather
// than the ATTACH prologue — the function reports on whichever process runs it.
func TestQuackSessionQueryShape(t *testing.T) {
	cfg := ServerConfig{Name: "prod", Type: ConnQuack, Host: "h", Port: 9494, Token: "qk_tok"}

	opts := strings.Join(cfg.quackQueryOptions(), ", ")
	if !strings.Contains(opts, "token = 'qk_tok'") {
		t.Errorf("options = %q, want the token as a named parameter", opts)
	}
	if !strings.Contains(opts, "disable_ssl = true") {
		t.Errorf("options = %q, want ssl disabled for a plaintext connection", opts)
	}

	tls := ServerConfig{Name: "p", Type: ConnQuack, Host: "h", Port: 443, Token: "t", TLS: true}
	if got := strings.Join(tls.quackQueryOptions(), ", "); !strings.Contains(got, "disable_ssl = false") {
		t.Errorf("options = %q, want ssl kept on for a TLS connection", got)
	}

	// A quote in the token must not break out of the literal.
	awkward := ServerConfig{Name: "p", Type: ConnQuack, Host: "h", Port: 9494, Token: "ab'cd"}
	if got := strings.Join(awkward.quackQueryOptions(), ", "); !strings.Contains(got, "token = 'ab''cd'") {
		t.Errorf("options = %q, want the token escaped", got)
	}
}
