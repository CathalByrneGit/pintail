package quack

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// parseSessionRows used to slice the connection_id to exactly 4 bytes, which
// panicked whenever a server reported a short id (small integers are the
// common case) and could split a multibyte client_context mid-codepoint.
func TestParseSessionRows(t *testing.T) {
	cfg := ServerConfig{Name: "prod", Host: "10.0.0.1"}

	tests := []struct {
		name         string
		json         string
		wantID       string
		wantIdentity string
	}{
		{
			name:         "short integer id",
			json:         `[{"connection_id":1,"client_context":"analyst1"}]`,
			wantID:       "1",
			wantIdentity: "analyst1",
		},
		{
			name:         "long id is cut to the column width",
			json:         `[{"connection_id":"c0128afe-11","client_context":"analyst1"}]`,
			wantID:       "c012",
			wantIdentity: "analyst1",
		},
		{
			name:         "long client_context is cut to 16",
			json:         `[{"connection_id":42,"client_context":"a-very-long-client-context-string"}]`,
			wantID:       "42",
			wantIdentity: "a-very-long-clie",
		},
		{
			name:         "multibyte client_context stays valid utf-8",
			json:         `[{"connection_id":7,"client_context":"日本語のクライアントコンテキスト"}]`,
			wantID:       "7",
			wantIdentity: "日本語のクライアントコンテキスト",
		},
		{
			name:         "missing fields fall back to positional id and config name",
			json:         `[{"something_else":true}]`,
			wantID:       "c01",
			wantIdentity: "prod",
		},
		{
			name:         "empty id falls back rather than blanking the column",
			json:         `[{"connection_id":"","client_context":""}]`,
			wantID:       "c01",
			wantIdentity: "prod",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			conns, _, err := parseSessionRows([]byte(tc.json), cfg)
			if err != nil {
				t.Fatalf("parseSessionRows: %v", err)
			}
			if len(conns) != 1 {
				t.Fatalf("got %d connections, want 1", len(conns))
			}
			if conns[0].ID != tc.wantID {
				t.Errorf("ID = %q, want %q", conns[0].ID, tc.wantID)
			}
			if conns[0].Identity != tc.wantIdentity {
				t.Errorf("Identity = %q, want %q", conns[0].Identity, tc.wantIdentity)
			}
			if !utf8Valid(conns[0].Identity) {
				t.Errorf("Identity is not valid UTF-8: %q", conns[0].Identity)
			}
		})
	}
}

func TestParseSessionRowsEmpty(t *testing.T) {
	for _, in := range []string{"", "   ", "[]"} {
		if _, _, err := parseSessionRows([]byte(in), ServerConfig{}); err == nil {
			t.Errorf("parseSessionRows(%q): want error, got nil", in)
		}
	}
}

// Query used to fall through to an HTTP path for every connection type.
// For local and DuckLake that dialed http://:0; for Quack it POSTed JSON at
// endpoints no Quack server serves. Either way the real failure was replaced
// with "no endpoint responded".
func TestQueryErrorReporting(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.duckdb")
	if err := os.WriteFile(dbPath, []byte("not a database"), 0600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name       string
		cfg        ServerConfig
		online     bool
		wantMethod string
		wantErrHas string
		wantNotHas string
	}{
		{
			name:       "offline connection reports offline, not a transport error",
			cfg:        ServerConfig{Name: "gone", Type: ConnLocal, Path: filepath.Join(dir, "missing.duckdb")},
			online:     false,
			wantMethod: "offline",
			wantErrHas: "no online connection",
		},
		{
			name:       "local without the CLI names the real prerequisite",
			cfg:        ServerConfig{Name: "local", Type: ConnLocal, Path: dbPath},
			online:     true,
			wantMethod: "cli",
			wantErrHas: "duckdb CLI not found in PATH",
			wantNotHas: "no endpoint responded",
		},
		{
			name:       "ducklake without the CLI names the real prerequisite",
			cfg:        ServerConfig{Name: "lake", Type: ConnDuckLake, CatalogPath: dbPath, StoragePath: dir},
			online:     true,
			wantMethod: "cli",
			wantErrHas: "duckdb CLI not found in PATH",
			wantNotHas: "no endpoint responded",
		},
		{
			// Quack needs the CLI too: its wire protocol is a binary message on
			// POST /quack, which duckdb speaks and we do not.
			name:       "quack without the CLI names the real prerequisite",
			cfg:        ServerConfig{Name: "quack", Type: ConnQuack, Host: "127.0.0.1", Port: 1},
			online:     true,
			wantMethod: "cli",
			wantErrHas: "duckdb CLI not found in PATH",
			wantNotHas: "no endpoint responded",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := NewQuackClient(tc.cfg, nil, nil)
			// Force the state under test rather than depending on whether a
			// duckdb binary happens to be installed on the test machine.
			c.state = ConnState{Online: tc.online, ErrMsg: "forced by test"}
			c.hasCLI = false

			res := c.Query(context.Background(), "SELECT 1;")
			if res == nil {
				t.Fatal("Query returned a nil result")
			}
			if res.Err == "" {
				t.Fatal("want an error, got a successful result")
			}
			if res.Method != tc.wantMethod {
				t.Errorf("Method = %q, want %q", res.Method, tc.wantMethod)
			}
			if !strings.Contains(res.Err, tc.wantErrHas) {
				t.Errorf("Err = %q, want it to contain %q", res.Err, tc.wantErrHas)
			}
			if tc.wantNotHas != "" && strings.Contains(res.Err, tc.wantNotHas) {
				t.Errorf("Err = %q, want it NOT to contain %q", res.Err, tc.wantNotHas)
			}
		})
	}
}

func utf8Valid(s string) bool {
	for _, r := range s {
		if r == '�' {
			return false
		}
	}
	return true
}

// current_setting() reads come back as one row with one column; the reader has
// to survive a prologue statement printing its own array first.
func TestFirstStringValue(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		column  string
		want    string
		wantErr bool
	}{
		{
			name:   "reads the named column",
			in:     `[{"value":"quack_nop_authorization"}]`,
			column: "value",
			want:   "quack_nop_authorization",
		},
		{
			// The prologue-emits-output case: a quack_query script can print an
			// array of its own ahead of the answer.
			name:   "takes the last statement's array",
			in:     "[{\"Success\":true}]\n[{\"value\":\"pintail_authz\"}]",
			column: "value",
			want:   "pintail_authz",
		},
		{"null becomes empty", `[{"value":null}]`, "value", "", false},
		{"non-string is stringified", `[{"value":42}]`, "value", "42", false},
		{"no rows is an error", `[]`, "value", "", true},
		{"missing column is an error", `[{"other":1}]`, "value", "", true},
		{"garbage is an error", `not json`, "value", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := firstStringValue([]byte(tc.in), tc.column)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}
