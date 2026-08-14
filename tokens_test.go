package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The display block elides the token value; the export must not, or the
// exported CREATE SECRET statement is unusable. Both used to share the
// eliding code path.
func TestTokenSQLElidesOnlyForDisplay(t *testing.T) {
	tok := buildToken("etl_pipeline_prod", "analytics, raw", "SELECT, INSERT", "never")
	if len(tok.Value) < 40 {
		t.Fatalf("generated token is unexpectedly short: %q", tok.Value)
	}

	tests := []struct {
		name     string
		full     bool
		wantFull bool
	}{
		{name: "display form elides the value", full: false, wantFull: false},
		{name: "export form keeps the value", full: true, wantFull: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sql := tokenSQL(tok, tc.full)
			hasFull := strings.Contains(sql, tok.Value)
			if hasFull != tc.wantFull {
				t.Errorf("tokenSQL(full=%v) contains full value = %v, want %v\n%s",
					tc.full, hasFull, tc.wantFull, sql)
			}
			if !tc.wantFull && !strings.Contains(sql, "…") {
				t.Errorf("elided form should mark the cut with an ellipsis:\n%s", sql)
			}
			if !strings.Contains(sql, "TYPE quack") {
				t.Errorf("statement lost its TYPE clause:\n%s", sql)
			}
		})
	}
}

func TestExportTokenSQLIsRunnable(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	tok := buildToken("etl", "*", "SELECT", "never")
	path, err := exportTokenSQL(tok)
	if err != nil {
		t.Fatalf("exportTokenSQL: %v", err)
	}
	if want := filepath.Join(home, ".duckdb", "pintail_tokens.sql"); path != want {
		t.Errorf("path = %q, want %q", path, want)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading export: %v", err)
	}
	body := string(data)
	if !strings.Contains(body, tok.Value) {
		t.Errorf("export does not contain the full token value — the statement is unusable:\n%s", body)
	}
	if strings.Contains(body, "…") {
		t.Errorf("export contains an elided value:\n%s", body)
	}
	if !strings.Contains(body, tok.Name) {
		t.Errorf("export does not name the token:\n%s", body)
	}
}

func TestMaskTokenNeverRevealsMoreThanThePrefix(t *testing.T) {
	tests := []struct{ name, in string }{
		{"empty", ""},
		{"short", "qk_ab"},
		{"exactly seven", "qk_abcd"},
		{"full length", "qk_0123456789abcdef0123456789abcdef"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := maskToken(tc.in)
			if len(tc.in) > 7 && strings.Contains(got, tc.in) {
				t.Errorf("maskToken(%q) = %q, leaks the whole value", tc.in, got)
			}
			if len(tc.in) <= 7 && strings.Contains(got, tc.in) && tc.in != "" {
				t.Errorf("maskToken(%q) = %q, short values should be fully masked", tc.in, got)
			}
		})
	}
}
