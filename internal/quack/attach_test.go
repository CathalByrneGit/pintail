package quack

import (
	"context"
	"io"
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
			// The extension defaults SSL on for any non-local host, so a
			// plaintext connection to one has to say so explicitly.
			name: "plaintext to a non-local host disables ssl",
			cfg:  ServerConfig{Name: "q", Type: ConnQuack, Host: "h", Port: 9494, Token: "qk_tok"},
			wantHas: []string{
				"INSTALL quack; LOAD quack;",
				"CREATE OR REPLACE SECRET _quack_remote (TYPE quack, TOKEN 'qk_tok', SCOPE 'quack:h:9494')",
				"ATTACH 'quack:h:9494' AS _remote (DISABLE_SSL true)",
				"USE _remote",
			},
			// TOKEN is not an ATTACH option in the published extension build.
			wantNone: []string{"AS _remote (TOKEN"},
		},
		{
			// TLS to a non-local host is already the extension's default, so the
			// option is omitted rather than restated. DISABLE_SSL as an ATTACH
			// option postdates the extension build published for DuckDB v1.5.5,
			// and emitting it unconditionally broke every Quack connection.
			name: "TLS to a non-local host needs no option at all",
			cfg:  ServerConfig{Name: "q", Type: ConnQuack, Host: "h", Port: 443, Token: "qk_tok", TLS: true},
			wantHas: []string{
				"CREATE OR REPLACE SECRET _quack_remote (TYPE quack, TOKEN 'qk_tok', SCOPE 'quack:h:443')",
				"ATTACH 'quack:h:443' AS _remote;",
			},
			wantNone: []string{"DISABLE_SSL"},
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
				"CREATE OR REPLACE SECRET _quack_catalog (TYPE quack, TOKEN 'qk_tok', SCOPE 'quack:catalog.internal:9494')",
				"ATTACH 'quack:catalog.internal:9494' AS _catalog (DISABLE_SSL true)",
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
func TestInvocation(t *testing.T) {
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
			wantScriptIn: []string{"ATTACH 'quack:h:9494'"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := NewQuackClient(tc.cfg, nil, secretResolverFor(s3))
			inv := c.invocation("SELECT 1", "-json")

			if tc.wantPosition != "" {
				if inv.Args[0] != tc.wantPosition {
					t.Fatalf("argv[0] = %q, want %q (full: %v)", inv.Args[0], tc.wantPosition, inv.Args)
				}
			} else if inv.Args[0] != "-no-init" {
				t.Fatalf("argv[0] = %q, want the base flags to come first (full: %v)", inv.Args[0], inv.Args)
			}

			// The script goes on stdin. Nothing resembling SQL belongs in argv:
			// that is what keeps tokens out of /proc/<pid>/cmdline.
			for _, a := range inv.Args {
				if strings.Contains(a, "SELECT 1") || a == "-c" {
					t.Errorf("script leaked into argv: %v", inv.Args)
				}
			}
			if !strings.HasSuffix(inv.Script, "SELECT 1") {
				t.Errorf("script does not end with the query: %q", inv.Script)
			}
			for _, want := range tc.wantScriptIn {
				if !strings.Contains(inv.Script, want) {
					t.Errorf("script missing %q: %q", want, inv.Script)
				}
			}
		})
	}
}

// Every duckdb subprocess must skip the operator's ~/.duckdbrc and stop at the
// first error. Without -no-init a .duckdbrc containing `ATTACH ':memory:' AS
// _lake` breaks every DuckLake query; without -bail a script on stdin keeps
// going after a failed ATTACH and runs the caller's query against the wrong
// catalog, because unlike `-c` a piped script does not abort on error.
func TestBaseCLIFlagsArePresentEverywhere(t *testing.T) {
	cfg := ServerConfig{Name: "q", Type: ConnQuack, Host: "h", Port: 9494, Token: "sekret"}
	c := NewQuackClient(cfg, nil, nil)

	for _, tc := range []struct {
		name string
		inv  cliInvocation
	}{
		{"query invocation", c.invocation("SELECT 1", "-json")},
		{"query invocation without flags", c.invocation("COPY (SELECT 1) TO 'x'")},
		{"server invocation", c.serverInvocation("SELECT 1", "-json")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, want := range []string{"-no-init", "-bail"} {
				found := false
				for _, a := range tc.inv.Args {
					if a == want {
						found = true
					}
				}
				if !found {
					t.Errorf("argv is missing %s: %v", want, tc.inv.Args)
				}
			}
		})
	}
}

// A bearer token and object-store credentials must never appear in argv, where
// any process on the machine can read them out of /proc/<pid>/cmdline. They
// belong in the script, which travels on stdin.
func TestCredentialsNeverReachArgv(t *testing.T) {
	const token = "tok-must-not-leak"
	const key = "AKIA-must-not-leak"
	const secret = "s3cret-must-not-leak"

	s3 := StorageSecret{Name: "lake_s3", Type: SecretS3, KeyID: key, Secret: secret}
	resolve := secretResolverFor(s3)

	cases := map[string]ServerConfig{
		"quack token": {Name: "q", Type: ConnQuack, Host: "h", Port: 9494, Token: token},
		"local with storage secret": {
			Name: "l", Type: ConnLocal, Path: "/d/a.duckdb", StorageSecretRef: "lake_s3",
		},
		"ducklake with storage secret": {
			Name: "dl", Type: ConnDuckLake, CatalogPath: "/d/c.duckdb",
			StoragePath: "s3://b/d", StorageSecretRef: "lake_s3",
		},
	}

	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			c := NewQuackClient(cfg, nil, resolve)
			invs := []cliInvocation{
				c.invocation("SELECT 1", "-json"),
				c.serverInvocation("SELECT 1", "-json"),
			}
			for _, inv := range invs {
				for _, a := range inv.Args {
					for _, bad := range []string{token, key, secret} {
						if strings.Contains(a, bad) {
							t.Errorf("credential %q found in argv: %v", bad, inv.Args)
						}
					}
				}
			}
		})
	}
}

// The command must actually wire the script to stdin — an invocation whose
// script never reaches the process would run an empty script and silently
// return nothing.
func TestInvocationCommandWiresStdin(t *testing.T) {
	c := NewQuackClient(ServerConfig{Type: ConnQuack, Host: "h", Port: 9494}, nil, nil)
	inv := c.invocation("SELECT 1", "-json")
	cmd := inv.command(context.Background(), "/nonexistent/duckdb")

	if cmd.Stdin == nil {
		t.Fatal("command has no stdin; the script would never reach duckdb")
	}
	got, err := io.ReadAll(cmd.Stdin)
	if err != nil {
		t.Fatalf("reading stdin: %v", err)
	}
	if string(got) != inv.Script {
		t.Errorf("stdin = %q, want the invocation script %q", got, inv.Script)
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

	res := c.Query(context.Background(), "SELECT answer FROM t;")

	// A storage secret needs the httpfs extension, which duckdb downloads on
	// first use. Where that download is blocked the prologue cannot complete —
	// but the failure still proves it ran, which is what this test is about.
	if strings.Contains(res.Err, "httpfs") {
		t.Skipf("extension download unavailable in this environment; prologue did run: %s", res.Err)
	}

	if res.Err != "" {
		t.Fatalf("query failed with the secret prologue in place: %s", res.Err)
	}
	if len(res.Rows) != 1 || res.Rows[0][0] != "42" {
		t.Errorf("rows = %v, want [[42]]", res.Rows)
	}

	// And the secret really was created in that session.
	verify := c.Query(context.Background(), "SELECT name FROM duckdb_secrets();")
	if verify.Err != "" {
		t.Fatalf("duckdb_secrets query failed: %s", verify.Err)
	}
	found := false
	for _, row := range verify.Rows {
		if len(row) > 0 && row[0] == "_storage" {
			found = true
		}
	}
	if !found {
		t.Errorf("the _storage secret was not created; rows = %v", verify.Rows)
	}
}

// The SSL option is the bug that broke every Quack connection, so the whole
// matrix is pinned here.
//
// The extension decides for itself: QuackUri::IsLocal treats localhost,
// 127.0.0.1 and ::1 as plaintext and everything else as SSL. Pintail emitted
// DISABLE_SSL unconditionally, and that option only reached ATTACH in
// duckdb-quack 7e80f7f — after the build published for DuckDB v1.5.5. So a
// loopback connection, which is what the README's own walkthrough sets up,
// failed with `Binder Error: Unrecognized option for attach "disable_ssl"`. The
// rule now is to say something only when it differs from what the extension
// would do unprompted.
func TestSSLOptionOnlyAppearsWhenItChangesTheDefault(t *testing.T) {
	tests := []struct {
		name    string
		host    string
		tls     bool
		wantOpt string // "" means no SSL option at all
		whyItIs string
	}{
		{
			name: "loopback plaintext is the extension's default",
			host: "127.0.0.1", tls: false, wantOpt: "",
			whyItIs: "IsLocal() is true, so SSL is already off",
		},
		{
			name: "localhost plaintext is the extension's default",
			host: "localhost", tls: false, wantOpt: "",
			whyItIs: "IsLocal() is true, so SSL is already off",
		},
		{
			name: "LOCALHOST is matched case-insensitively",
			host: "LocalHost", tls: false, wantOpt: "",
			whyItIs: "the extension lowercases the host before comparing",
		},
		{
			name: "ipv6 loopback is local too",
			host: "::1", tls: false, wantOpt: "",
			whyItIs: "IsLocal() lists ::1",
		},
		{
			name: "TLS on loopback is not the default and must be stated",
			host: "127.0.0.1", tls: true, wantOpt: "DISABLE_SSL false",
			whyItIs: "local defaults to plaintext, so SSL has to be asked for",
		},
		{
			name: "TLS to a remote host is the default",
			host: "quack.example.com", tls: true, wantOpt: "",
			whyItIs: "non-local defaults to SSL",
		},
		{
			name: "plaintext to a remote host must be stated",
			host: "quack.example.com", tls: false, wantOpt: "DISABLE_SSL true",
			whyItIs: "non-local defaults to SSL, so plaintext has to be asked for",
		},
		{
			// A host that merely contains "localhost" is not loopback.
			name: "a host containing localhost is not local",
			host: "not-localhost.example.com", tls: true, wantOpt: "",
			whyItIs: "IsLocal() compares the whole host, not a substring",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := ServerConfig{Name: "q", Type: ConnQuack, Host: tc.host, Port: 9494, Token: "tok", TLS: tc.tls}

			attach := strings.Join(cfg.quackAttachOptions(), ", ")
			query := strings.Join(cfg.quackQueryOptions(), ", ")

			if tc.wantOpt == "" {
				if strings.Contains(attach, "DISABLE_SSL") {
					t.Errorf("ATTACH options = %q, want no SSL option (%s)", attach, tc.whyItIs)
				}
				if strings.Contains(query, "disable_ssl") {
					t.Errorf("quack_query options = %q, want no SSL option (%s)", query, tc.whyItIs)
				}
				return
			}

			if !strings.Contains(attach, tc.wantOpt) {
				t.Errorf("ATTACH options = %q, want %q (%s)", attach, tc.wantOpt, tc.whyItIs)
			}
			// The table function spells the same option as a named parameter.
			wantQuery := strings.ToLower(strings.Replace(tc.wantOpt, " ", " = ", 1))
			if !strings.Contains(query, wantQuery) {
				t.Errorf("quack_query options = %q, want %q (%s)", query, wantQuery, tc.whyItIs)
			}
		})
	}
}

// With neither a token nor an SSL override there is nothing to put in
// parentheses, and `ATTACH '…' AS x ()` does not parse. The extension resolves a
// `CREATE SECRET (TYPE quack, …)` when TOKEN is absent, so this is a real
// configuration rather than a degenerate one.
func TestAttachWithNoOptionsOmitsTheParentheses(t *testing.T) {
	cfg := ServerConfig{Name: "q", Type: ConnQuack, Host: "localhost", Port: 9494}

	if got := cfg.quackAttachOptions(); len(got) != 0 {
		t.Fatalf("options = %v, want none for a tokenless loopback connection", got)
	}

	prefix := cfg.AttachPrefix(nil, nil)
	if strings.Contains(prefix, "()") {
		t.Errorf("empty parentheses in the prologue:\n%s", prefix)
	}
	if !strings.Contains(prefix, "ATTACH 'quack:localhost:9494' AS _remote;") {
		t.Errorf("want a bare ATTACH, got:\n%s", prefix)
	}
	// No token means no secret at all, rather than one with an empty credential.
	if strings.Contains(prefix, "SECRET") {
		t.Errorf("a secret was created for a tokenless connection:\n%s", prefix)
	}

	// Same for the quack_query call: no dangling comma.
	sql := cfg.quackQuerySQL("SELECT 1")
	if strings.Contains(sql, ", )") || strings.Contains(sql, ",)") {
		t.Errorf("dangling comma in the quack_query call:\n%s", sql)
	}
	if !strings.Contains(sql, "quack_query('quack:localhost:9494', 'SELECT 1')") {
		t.Errorf("want a two-argument call, got:\n%s", sql)
	}
}
