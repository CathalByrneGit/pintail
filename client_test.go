package main

import (
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
			conns, err := parseSessionRows([]byte(tc.json), cfg)
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
		if _, err := parseSessionRows([]byte(in), ServerConfig{}); err == nil {
			t.Errorf("parseSessionRows(%q): want error, got nil", in)
		}
	}
}

// QueryAsync used to fall through to the HTTP path for every connection type.
// Local and DuckLake configs have no Host/Port, so that dialed http://:0 and
// reported "no endpoint responded", discarding the real failure.
func TestQueryAsyncErrorReporting(t *testing.T) {
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
			name:       "quack still falls back to HTTP and reports that failure",
			cfg:        ServerConfig{Name: "quack", Type: ConnQuack, Host: "127.0.0.1", Port: 1},
			online:     true,
			wantMethod: "http",
			wantErrHas: "no endpoint responded",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := NewQuackClient(tc.cfg, nil, nil)
			// Force the state under test rather than depending on whether a
			// duckdb binary happens to be installed on the test machine.
			c.state = ConnState{Online: tc.online, ErrMsg: "forced by test"}
			c.hasCLI = false

			msg, isQuery := c.QueryAsync("SELECT 1;", tc.cfg.ToServerInfo())().(queryResultMsg)
			if !isQuery {
				t.Fatalf("QueryAsync returned %T, want queryResultMsg", msg)
			}
			if msg.result == nil {
				t.Fatal("QueryAsync returned a nil result")
			}
			if msg.result.Err == "" {
				t.Fatal("want an error, got a successful result")
			}
			if msg.result.Method != tc.wantMethod {
				t.Errorf("Method = %q, want %q", msg.result.Method, tc.wantMethod)
			}
			if !strings.Contains(msg.result.Err, tc.wantErrHas) {
				t.Errorf("Err = %q, want it to contain %q", msg.result.Err, tc.wantErrHas)
			}
			if tc.wantNotHas != "" && strings.Contains(msg.result.Err, tc.wantNotHas) {
				t.Errorf("Err = %q, want it NOT to contain %q", msg.result.Err, tc.wantNotHas)
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
