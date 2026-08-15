package main

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func secretResolverFor(secrets ...StorageSecret) SecretResolver {
	return func(name string) (StorageSecret, bool) {
		for _, s := range secrets {
			if s.Name == name {
				return s, true
			}
		}
		return StorageSecret{}, false
	}
}

func configResolverFor(cfgs ...ServerConfig) ConfigResolver {
	return func(name string) (ServerConfig, bool) {
		for _, c := range cfgs {
			if c.Name == name {
				return c, true
			}
		}
		return ServerConfig{}, false
	}
}

func TestAttachPrefix(t *testing.T) {
	s3 := StorageSecret{Name: "lake_s3", Type: SecretS3, KeyID: "AKIA", Secret: "shh", Region: "us-east-1"}
	catalog := ServerConfig{Name: "central", Type: ConnQuack, Host: "catalog.internal", Port: 9494, Token: "qk_tok"}

	tests := []struct {
		name     string
		cfg      ServerConfig
		wantHas  []string
		wantNone []string
	}{
		{
			name:     "plain local file needs no prologue",
			cfg:      ServerConfig{Name: "l", Type: ConnLocal, Path: "/data/a.duckdb"},
			wantNone: []string{"ATTACH", "CREATE OR REPLACE SECRET"},
		},
		{
			name: "local file with a storage secret still gets the secret",
			cfg: ServerConfig{Name: "l", Type: ConnLocal, Path: "/data/a.duckdb",
				StorageSecretRef: "lake_s3"},
			wantHas:  []string{"INSTALL httpfs", "CREATE OR REPLACE SECRET _storage", "KEY_ID 'AKIA'"},
			wantNone: []string{"ATTACH"}, // opened positionally instead
		},
		{
			name: "remote local path is attached read-only after the secret",
			cfg: ServerConfig{Name: "l", Type: ConnLocal, Path: "s3://bucket/a.duckdb",
				StorageSecretRef: "lake_s3"},
			wantHas: []string{
				"CREATE OR REPLACE SECRET _storage",
				"ATTACH 's3://bucket/a.duckdb' AS _local (READ_ONLY)",
				"USE _local",
			},
		},
		{
			// The quack extension defaults SSL on for any non-local host, so a
			// plaintext connection has to say so explicitly.
			name: "quack attaches with its token and disables ssl",
			cfg:  ServerConfig{Name: "q", Type: ConnQuack, Host: "h", Port: 9494, Token: "qk_tok"},
			wantHas: []string{
				"ATTACH 'quack://h:9494' AS _remote (TOKEN 'qk_tok', DISABLE_SSL true)",
				"USE _remote",
			},
		},
		{
			name: "quack with TLS keeps ssl on",
			cfg:  ServerConfig{Name: "q", Type: ConnQuack, Host: "h", Port: 443, Token: "qk_tok", TLS: true},
			wantHas: []string{
				"ATTACH 'quack://h:443' AS _remote (TOKEN 'qk_tok', DISABLE_SSL false)",
			},
			wantNone: []string{"DISABLE_SSL true"},
		},
		{
			name: "ducklake with a catalog path",
			cfg: ServerConfig{Name: "lake", Type: ConnDuckLake,
				CatalogPath: "/tmp/catalog.duckdb", StoragePath: "/tmp/data"},
			wantHas: []string{
				"INSTALL ducklake; LOAD ducklake;",
				"ATTACH 'ducklake:/tmp/catalog.duckdb' AS _lake (DATA_PATH '/tmp/data')",
			},
		},
		{
			name: "ducklake via catalog_ref emits the two-step attach",
			cfg: ServerConfig{Name: "lake", Type: ConnDuckLake,
				CatalogRef: "central", StoragePath: "s3://bucket/lake", StorageSecretRef: "lake_s3"},
			wantHas: []string{
				"CREATE OR REPLACE SECRET _storage",
				"ATTACH 'quack://catalog.internal:9494' AS _catalog (TOKEN 'qk_tok', DISABLE_SSL true)",
				"ATTACH 'ducklake:_catalog' AS _lake (DATA_PATH 's3://bucket/lake')",
			},
		},
		{
			name: "catalog_ref wins over catalog_path",
			cfg: ServerConfig{Name: "lake", Type: ConnDuckLake,
				CatalogRef: "central", CatalogPath: "/ignored.duckdb", StoragePath: "/tmp/d"},
			wantNone: []string{"/ignored.duckdb"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.cfg.AttachPrefix(configResolverFor(catalog), secretResolverFor(s3))
			for _, want := range tc.wantHas {
				if !strings.Contains(got, want) {
					t.Errorf("prefix is missing %q:\n%s", want, got)
				}
			}
			for _, unwanted := range tc.wantNone {
				if strings.Contains(got, unwanted) {
					t.Errorf("prefix should not contain %q:\n%s", unwanted, got)
				}
			}
		})
	}
}

// Values with a single quote used to be interpolated raw, producing a script
// duckdb could not parse (TOKEN 'ab'cd').
func TestGeneratedSQLEscapesQuotes(t *testing.T) {
	awkward := StorageSecret{Name: "s", Type: SecretS3, KeyID: "ak'id", Secret: "pa'ss", Scope: "s3://bu'cket"}

	inline := awkward.ToSQLInline("_storage")
	for _, want := range []string{"KEY_ID 'ak''id'", "SECRET 'pa''ss'", "SCOPE 's3://bu''cket'"} {
		if !strings.Contains(inline, want) {
			t.Errorf("inline secret SQL missing %q:\n%s", want, inline)
		}
	}

	cfg := ServerConfig{Name: "q", Type: ConnQuack, Host: "h", Port: 1, Token: "ab'cd"}
	prefix := cfg.AttachPrefix(nil, nil)
	if !strings.Contains(prefix, "TOKEN 'ab''cd'") {
		t.Errorf("token was not escaped:\n%s", prefix)
	}

	lake := ServerConfig{Name: "l", Type: ConnDuckLake, CatalogPath: "/a'b.duckdb", StoragePath: "/c'd"}
	lakePrefix := lake.AttachPrefix(nil, nil)
	for _, want := range []string{"ducklake:/a''b.duckdb", "DATA_PATH '/c''d'"} {
		if !strings.Contains(lakePrefix, want) {
			t.Errorf("ducklake path was not escaped (%q):\n%s", want, lakePrefix)
		}
	}
}

// The prologue has to reach the CLI for every connection type. Local
// connections used to drop it, so a local path with a storage_secret_ref never
// created its secret.
func TestCLIArgs(t *testing.T) {
	s3 := StorageSecret{Name: "lake_s3", Type: SecretS3, KeyID: "AKIA", Secret: "shh"}

	tests := []struct {
		name         string
		cfg          ServerConfig
		wantPosition string // expected argv[0], or "" when there is none
		wantScriptIn []string
	}{
		{
			name:         "local file is opened positionally",
			cfg:          ServerConfig{Type: ConnLocal, Path: "/data/a.duckdb"},
			wantPosition: "/data/a.duckdb",
		},
		{
			name:         "local file with a secret keeps both the file and the prologue",
			cfg:          ServerConfig{Type: ConnLocal, Path: "/data/a.duckdb", StorageSecretRef: "lake_s3"},
			wantPosition: "/data/a.duckdb",
			wantScriptIn: []string{"CREATE OR REPLACE SECRET _storage", "SELECT 1"},
		},
		{
			name:         "remote local path is not passed positionally",
			cfg:          ServerConfig{Type: ConnLocal, Path: "s3://bucket/a.duckdb", StorageSecretRef: "lake_s3"},
			wantPosition: "",
			wantScriptIn: []string{"ATTACH 's3://bucket/a.duckdb' AS _local (READ_ONLY)"},
		},
		{
			name:         "quack is reached through the prologue only",
			cfg:          ServerConfig{Type: ConnQuack, Host: "h", Port: 9494},
			wantPosition: "",
			wantScriptIn: []string{"ATTACH 'quack://h:9494'"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := NewQuackClient(tc.cfg, nil, secretResolverFor(s3))
			args := c.cliArgs("SELECT 1", "-json")

			if tc.wantPosition != "" {
				if args[0] != tc.wantPosition {
					t.Fatalf("argv[0] = %q, want %q (full: %v)", args[0], tc.wantPosition, args)
				}
			} else if args[0] != "-json" {
				t.Fatalf("argv[0] = %q, want the flags to come first (full: %v)", args[0], args)
			}

			if args[len(args)-2] != "-c" {
				t.Fatalf("expected -c before the script, got %v", args)
			}
			script := args[len(args)-1]
			if !strings.HasSuffix(script, "SELECT 1") {
				t.Errorf("script does not end with the query: %q", script)
			}
			for _, want := range tc.wantScriptIn {
				if !strings.Contains(script, want) {
					t.Errorf("script missing %q: %q", want, script)
				}
			}
		})
	}
}

// A remote local path cannot be stat'd, so it must not be reported as a
// missing file — that left the connection permanently offline and unqueryable.
func TestPingLocalRemotePathIsUnprobed(t *testing.T) {
	c := NewQuackClient(ServerConfig{Name: "r", Type: ConnLocal, Path: "s3://bucket/a.duckdb"}, nil, nil)

	if _, err := c.Ping(context.Background()); err != nil {
		t.Fatalf("ping returned an error: %v", err)
	}
	st := c.GetState()
	if !st.Online {
		t.Error("remote path should be treated as reachable, not missing")
	}
	if st.Method != "uri" {
		t.Errorf("method = %q, want %q so the UI can say it was not probed", st.Method, "uri")
	}

	// An on-disk path still gets a real stat, and a missing one is still offline.
	missing := NewQuackClient(ServerConfig{Name: "m", Type: ConnLocal, Path: "/nope/missing.duckdb"}, nil, nil)
	if _, err := missing.Ping(context.Background()); err == nil {
		t.Error("a missing file should still fail its ping")
	}
	if st := missing.GetState(); st.Online || st.Method != "stat" {
		t.Errorf("state = %+v, want an offline stat result", st)
	}
}

// End-to-end: a local connection that references a storage secret must still
// query its on-disk file, with the CREATE SECRET running ahead of the query.
func TestLocalWithSecretQueriesAgainstRealDuckDB(t *testing.T) {
	if _, err := exec.LookPath("duckdb"); err != nil {
		t.Skip("duckdb not in PATH — skipping integration test")
	}

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "fixture.duckdb")
	seed := "CREATE TABLE t AS SELECT 42 AS answer;"
	if out, err := exec.Command("duckdb", dbPath, "-c", seed).CombinedOutput(); err != nil {
		t.Fatalf("seeding: %v\n%s", err, out)
	}

	cfg := ServerConfig{Name: "local", Type: ConnLocal, Path: dbPath, StorageSecretRef: "lake_s3"}
	secret := StorageSecret{Name: "lake_s3", Type: SecretS3, KeyID: "AKIA", Secret: "shh", Region: "us-east-1"}
	c := NewQuackClient(cfg, nil, secretResolverFor(secret))
	if _, err := c.Ping(context.Background()); err != nil {
		t.Fatalf("ping: %v", err)
	}

	msg := c.QueryAsync(context.Background(), "SELECT answer FROM t;")().(queryResultMsg)

	// A storage secret needs the httpfs extension, which duckdb downloads on
	// first use. Where that download is blocked the prologue cannot complete —
	// but the failure still proves it ran, which is what this test is about.
	if strings.Contains(msg.result.Err, "httpfs") {
		t.Skipf("extension download unavailable in this environment; prologue did run: %s", msg.result.Err)
	}

	if msg.result.Err != "" {
		t.Fatalf("query failed with the secret prologue in place: %s", msg.result.Err)
	}
	if len(msg.result.Rows) != 1 || msg.result.Rows[0][0] != "42" {
		t.Errorf("rows = %v, want [[42]]", msg.result.Rows)
	}

	// And the secret really was created in that session.
	verify := c.QueryAsync(context.Background(), "SELECT name FROM duckdb_secrets();")().(queryResultMsg)
	if verify.result.Err != "" {
		t.Fatalf("duckdb_secrets query failed: %s", verify.result.Err)
	}
	found := false
	for _, row := range verify.result.Rows {
		if len(row) > 0 && row[0] == "_storage" {
			found = true
		}
	}
	if !found {
		t.Errorf("the _storage secret was not created; rows = %v", verify.result.Rows)
	}
}
