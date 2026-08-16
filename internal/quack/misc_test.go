package quack

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The type and secret-type selectors cycle, and the labels are what the form
// chips show. A selector that does not wrap leaves a field unreachable.
func TestSelectorsCycleAndLabel(t *testing.T) {
	seen := map[ConnType]bool{}
	ct := ConnQuack
	for range AllConnTypes {
		if seen[ct] {
			t.Fatalf("ConnType cycle repeated %q before covering every type", ct)
		}
		seen[ct] = true
		if ct.Label() == "" {
			t.Errorf("ConnType %q has no label", ct)
		}
		ct = ct.Next()
	}
	if ct != ConnQuack {
		t.Errorf("cycle ended at %q, want it back at the start", ct)
	}
	// An unrecognised value must not strand the selector.
	if got := ConnType("nonsense").Next(); got != ConnQuack {
		t.Errorf("Next() on an unknown type = %q, want ConnQuack", got)
	}
	if got := ConnType("nonsense").Label(); got != "nonsense" {
		t.Errorf("Label() should fall back to the raw value, got %q", got)
	}

	st := SecretS3
	for range AllSecretTypes {
		st = st.Next()
	}
	if st != SecretS3 {
		t.Errorf("SecretType cycle ended at %q, want SecretS3", st)
	}
	if got := SecretType("nonsense").Next(); got != SecretS3 {
		t.Errorf("Next() on an unknown secret type = %q, want SecretS3", got)
	}
}

// DisplayURI is what the header shows for each connection, and every type needs
// its own form — a blank one leaves a row unidentifiable.
func TestDisplayURIPerType(t *testing.T) {
	tests := []struct {
		name string
		cfg  ServerConfig
		want string
	}{
		{"quack", ServerConfig{Type: ConnQuack, Host: "h", Port: 9494}, "quack://h:9494"},
		{"local", ServerConfig{Type: ConnLocal, Path: "/d/a.duckdb"}, "/d/a.duckdb"},
		{"ducklake by path", ServerConfig{Type: ConnDuckLake,
			CatalogPath: "/d/c.duckdb", StoragePath: "/d/data"}, "ducklake:"},
		{"ducklake by ref", ServerConfig{Type: ConnDuckLake,
			CatalogRef: "central", StoragePath: "s3://b/l"}, "central"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.cfg.DisplayURI()
			if got == "" {
				t.Fatal("DisplayURI is empty")
			}
			if !strings.Contains(got, tc.want) {
				t.Errorf("DisplayURI = %q, want it to contain %q", got, tc.want)
			}
		})
	}
}

func TestToServerInfoCarriesTheIdentity(t *testing.T) {
	cfg := ServerConfig{Name: "prod", Type: ConnQuack, Host: "h", Port: 9494, Token: "tok"}
	info := cfg.ToServerInfo()
	if info.Name != "prod" {
		t.Errorf("Name = %q", info.Name)
	}
	if info.URI == "" {
		t.Error("URI is empty, so the scratchpad target selector would show a blank row")
	}
}

func TestStorageSecretsEqual(t *testing.T) {
	base := []StorageSecret{{Name: "a", Type: SecretS3, KeyID: "k", Secret: "s", Region: "r"}}

	same := []StorageSecret{{Name: "a", Type: SecretS3, KeyID: "k", Secret: "s", Region: "r"}}
	if !StorageSecretsEqual(base, same) {
		t.Error("identical lists should compare equal")
	}
	if StorageSecretsEqual(base, nil) {
		t.Error("a list and an empty list are not equal")
	}

	// Every field that is persisted has to participate, or an edit to it will
	// not be recognised as a change and never gets saved.
	for _, mutate := range []func(*StorageSecret){
		func(s *StorageSecret) { s.Name = "b" },
		func(s *StorageSecret) { s.Type = SecretGCS },
		func(s *StorageSecret) { s.KeyID = "k2" },
		func(s *StorageSecret) { s.Secret = "s2" },
		func(s *StorageSecret) { s.Region = "r2" },
		func(s *StorageSecret) { s.AccountID = "acct" },
		func(s *StorageSecret) { s.ConnStr = "conn" },
		func(s *StorageSecret) { s.Scope = "scope" },
	} {
		changed := []StorageSecret{base[0]}
		mutate(&changed[0])
		if StorageSecretsEqual(base, changed) {
			t.Errorf("a change to %+v was not detected", changed[0])
		}
	}
}

func TestQueryResultIsEmpty(t *testing.T) {
	tests := []struct {
		name string
		r    *QueryResult
		want bool
	}{
		{"nil", nil, true},
		{"no columns or rows", &QueryResult{}, true},
		{"an error is not exportable", &QueryResult{Columns: []string{"a"}, Rows: [][]string{{"1"}}, Err: "boom"}, true},
		{"columns with no rows still has a header", &QueryResult{Columns: []string{"a"}}, false},
		{"rows", &QueryResult{Columns: []string{"a"}, Rows: [][]string{{"1"}}}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.r.IsEmpty(); got != tc.want {
				t.Errorf("IsEmpty() = %v, want %v", got, tc.want)
			}
		})
	}
}

// cliError exists because Go's bare "exit status 1" told the user nothing. The
// subprocess's stderr has to win where there is any.
func TestCliErrorPrefersStderr(t *testing.T) {
	cmd := exec.Command("sh", "-c", "echo 'Binder Error: nope' >&2; exit 1")
	_, err := cmd.Output()
	if err == nil {
		t.Fatal("expected the command to fail")
	}
	if got := cliError(err); !strings.Contains(got, "Binder Error") {
		t.Errorf("cliError = %q, want the stderr text", got)
	}

	// With no stderr there is nothing better than Go's own message.
	if got := cliError(context.Canceled); got != context.Canceled.Error() {
		t.Errorf("cliError = %q, want the error's own text", got)
	}
}

// InitClients has to hand every client the resolvers, or a DuckLake connection
// referencing another connection cannot resolve it at query time.
func TestInitClientsWiresResolvers(t *testing.T) {
	cfgs := []ServerConfig{
		{Name: "central", Type: ConnQuack, Host: "catalog", Port: 9494, Token: "tok"},
		{Name: "lake", Type: ConnDuckLake, CatalogRef: "central",
			StoragePath: "s3://b/l", StorageSecretRef: "s3"},
	}
	secrets := []StorageSecret{{Name: "s3", Type: SecretS3, KeyID: "AKIA", Secret: "shh"}}

	clients := InitClients(cfgs, secrets)
	if len(clients) != 2 {
		t.Fatalf("got %d clients, want 2", len(clients))
	}

	// The lake's prologue can only name the catalog connection if the resolver
	// reached it, and the secret only if the secret resolver did.
	prefix := clients[1].attachPrefix()
	for _, want := range []string{"quack://catalog:9494", "TOKEN 'tok'", "KEY_ID 'AKIA'"} {
		if !strings.Contains(prefix, want) {
			t.Errorf("prologue is missing %q, so a resolver was not wired:\n%s", want, prefix)
		}
	}
}

// A fresh install has to produce a config that loads, or the first run is broken.
func TestDefaultConfigsAreUsable(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	got := LoadServerConfigs()
	if len(got) == 0 {
		t.Fatal("a fresh install should get at least one stub connection")
	}
	for _, cfg := range got {
		if cfg.Name == "" || cfg.Type == "" {
			t.Errorf("default connection is incomplete: %+v", cfg)
		}
		if cfg.DisplayURI() == "" {
			t.Errorf("default connection %q has no displayable URI", cfg.Name)
		}
	}
}

// A DuckLake whose catalog file is missing is offline, and says which file.
func TestPingDuckLakeMissingCatalog(t *testing.T) {
	cfg := ServerConfig{Name: "lake", Type: ConnDuckLake,
		CatalogPath: filepath.Join(t.TempDir(), "absent.duckdb"), StoragePath: "/tmp/d"}
	c := NewQuackClient(cfg, nil, nil)

	if _, err := c.Ping(context.Background()); err == nil {
		t.Fatal("a missing catalog file should not ping successfully")
	}
	st := c.GetState()
	if st.Online {
		t.Error("state should be offline")
	}
	if st.ErrMsg == "" {
		t.Error("state should carry a reason")
	}
}

// A DuckLake pointed at a real catalog file pings via a stat, with no extension
// download needed.
func TestPingDuckLakeExistingCatalog(t *testing.T) {
	dir := t.TempDir()
	catalog := filepath.Join(dir, "catalog.duckdb")
	if err := os.WriteFile(catalog, []byte("placeholder"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := ServerConfig{Name: "lake", Type: ConnDuckLake, CatalogPath: catalog, StoragePath: dir}
	c := NewQuackClient(cfg, nil, nil)

	lat, err := c.Ping(context.Background())
	if err != nil {
		t.Fatalf("ping: %v", err)
	}
	if !c.GetState().Online {
		t.Error("a catalog file that exists should read as online")
	}
	if lat < 0 || lat > 10*time.Second {
		t.Errorf("latency = %v, implausible for a stat", lat)
	}
}

// The construction options are the seam tests use to reach code behind an
// online check; if they did not take effect those tests would pass vacuously.
func TestClientOptions(t *testing.T) {
	cfg := ServerConfig{Name: "q", Type: ConnQuack, Host: "h", Port: 9494}

	c := NewQuackClient(cfg, nil, nil,
		WithState(ConnState{Online: true, Method: "forced"}),
		WithCLI("/some/duckdb"))
	if st := c.GetState(); !st.Online || st.Method != "forced" {
		t.Errorf("WithState did not take effect: %+v", st)
	}
	if !c.HasCLI() {
		t.Error("WithCLI should report a CLI as available")
	}

	// The empty path means "no CLI", which is how a test exercises the
	// missing-duckdb path on a machine that has one. It needs the online state
	// too: Query checks reachability first, so an unpinged client reports
	// "offline" before it ever looks for a binary.
	none := NewQuackClient(cfg, nil, nil,
		WithCLI(""), WithState(ConnState{Online: true}))
	if none.HasCLI() {
		t.Error(`WithCLI("") should report no CLI`)
	}
	res := none.Query(context.Background(), "SELECT 1")
	if res.Err == "" {
		t.Fatal("a query with no CLI should fail")
	}
	if !strings.Contains(res.Err, "duckdb CLI not found") {
		t.Errorf("Err = %q, want it to name the missing CLI", res.Err)
	}
}

// Parquet export re-runs the query on the backend and writes a real file.
func TestCopyToParquetAgainstRealDuckDB(t *testing.T) {
	if _, err := exec.LookPath("duckdb"); err != nil {
		t.Skip("duckdb not in PATH")
	}

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "fixture.duckdb")
	if out, err := exec.Command("duckdb", "-no-init", dbPath, "-c",
		"CREATE TABLE t AS SELECT range AS id FROM range(5);").CombinedOutput(); err != nil {
		t.Fatalf("seeding: %v\n%s", err, out)
	}

	c := NewQuackClient(ServerConfig{Name: "f", Type: ConnLocal, Path: dbPath}, nil, nil)
	if _, err := c.Ping(context.Background()); err != nil {
		t.Fatalf("ping: %v", err)
	}

	// A path with a quote in it must not break out of the COPY statement.
	out := filepath.Join(dir, "it's an export.parquet")
	if err := c.CopyToParquet(context.Background(), "SELECT id FROM t ORDER BY id;", out); err != nil {
		t.Fatalf("CopyToParquet: %v", err)
	}
	info, err := os.Stat(out)
	if err != nil {
		t.Fatalf("no file was written: %v", err)
	}
	if info.Size() == 0 {
		t.Error("the parquet file is empty")
	}

	// And it must be readable back as parquet with the right row count.
	readBack := "SELECT count(*) AS n FROM read_parquet('" + SQLQuote(out) + "');"
	res := c.Query(context.Background(), readBack)
	if res.Err != "" {
		t.Fatalf("reading the export back: %s", res.Err)
	}
	if len(res.Rows) != 1 || res.Rows[0][0] != "5" {
		t.Errorf("export contains %v rows, want 5", res.Rows)
	}
}

// ToSQL is the multi-line form shown on screen and written to export files; it
// must be runnable SQL, not just a rendering.
func TestStorageSecretToSQL(t *testing.T) {
	s := StorageSecret{Name: "lake_s3", Type: SecretS3,
		KeyID: "AKIA", Secret: "sh'h", Region: "eu-west-1", Scope: "s3://b/p"}

	got := s.ToSQL()
	for _, want := range []string{
		"CREATE OR REPLACE SECRET lake_s3",
		"TYPE s3", "KEY_ID 'AKIA'", "SECRET 'sh''h'",
		"REGION 'eu-west-1'", "SCOPE 's3://b/p'",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("ToSQL missing %q:\n%s", want, got)
		}
	}
	if !strings.HasSuffix(strings.TrimSpace(got), ");") {
		t.Errorf("statement is not terminated:\n%s", got)
	}

	// Azure takes a connection string instead.
	az := StorageSecret{Name: "az", Type: SecretAzure, ConnStr: "Account=x;Key=y"}
	if got := az.ToSQL(); !strings.Contains(got, "CONNECTION_STRING 'Account=x;Key=y'") {
		t.Errorf("azure ToSQL missing the connection string:\n%s", got)
	}
}

// Sessions has a per-type answer. The local branch reports us, and needs no
// server — worth pinning because it is what the dashboard shows for a file.
func TestSessionsPerConnectionType(t *testing.T) {
	t.Run("local reports itself", func(t *testing.T) {
		cfg := ServerConfig{Name: "f", Type: ConnLocal, Path: "/data/analytics.duckdb"}
		c := NewQuackClient(cfg, nil, nil,
			WithState(ConnState{Online: true}), WithCLI("/some/duckdb"))

		conns, reported, err := c.Sessions(context.Background())
		if err != nil {
			t.Fatalf("Sessions: %v", err)
		}
		if len(conns) != 1 {
			t.Fatalf("got %d sessions, want 1", len(conns))
		}
		if conns[0].Catalog != "analytics.duckdb" {
			t.Errorf("Catalog = %q, want the file's base name", conns[0].Catalog)
		}
		if conns[0].Status != "active" {
			t.Errorf("Status = %q, want active", conns[0].Status)
		}
		if reported != "" {
			t.Errorf("reportedCount = %q, want empty: a file has no backend count", reported)
		}
	})

	t.Run("offline is an error rather than an empty list", func(t *testing.T) {
		c := NewQuackClient(ServerConfig{Name: "f", Type: ConnLocal, Path: "/x"}, nil, nil,
			WithCLI("/some/duckdb"))
		if _, _, err := c.Sessions(context.Background()); err == nil {
			t.Error("an offline connection should report an error, not zero sessions")
		}
	})

	t.Run("no CLI is an error", func(t *testing.T) {
		c := NewQuackClient(ServerConfig{Name: "f", Type: ConnLocal, Path: "/x"}, nil, nil,
			WithState(ConnState{Online: true}), WithCLI(""))
		if _, _, err := c.Sessions(context.Background()); err == nil {
			t.Error("no CLI should report an error")
		}
	})
}
