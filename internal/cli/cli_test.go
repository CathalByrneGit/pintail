package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/CathalByrneGit/pintail/internal/quack"
)

// testEnv returns an Env whose output is captured and whose connection list is
// the one given, rather than whatever happens to be in the developer's
// ~/.duckdb/pintail.json.
func testEnv(cfgs ...quack.ServerConfig) (Env, *bytes.Buffer) {
	var out bytes.Buffer
	return Env{
		Out:         &out,
		Err:         &out,
		LoadConfigs: func() []quack.ServerConfig { return cfgs },
		LoadSecrets: func() []quack.StorageSecret { return nil },
	}, &out
}

func TestListEmpty(t *testing.T) {
	env, out := testEnv()
	if err := Run(env, []string{"list"}); err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(out.String(), "no connections configured") {
		t.Errorf("output should explain the empty state, got %q", out.String())
	}
}

func TestListTable(t *testing.T) {
	env, out := testEnv(
		quack.ServerConfig{Name: "prod", Type: quack.ConnQuack, Host: "h", Port: 9494},
		quack.ServerConfig{Name: "lake", Type: quack.ConnDuckLake, CatalogPath: "/c.duckdb", StoragePath: "/d"},
	)
	if err := Run(env, []string{"list"}); err != nil {
		t.Fatalf("list: %v", err)
	}
	got := out.String()
	for _, want := range []string{"NAME", "TYPE", "URI", "prod", "quack", "lake", "ducklake"} {
		if !strings.Contains(got, want) {
			t.Errorf("listing is missing %q:\n%s", want, got)
		}
	}
}

// --json has to be machine-readable, which means it must parse — the whole
// reason the flag exists.
func TestListJSON(t *testing.T) {
	env, out := testEnv(quack.ServerConfig{Name: "prod", Type: quack.ConnQuack, Host: "h", Port: 9494})
	if err := Run(env, []string{"list", "--json"}); err != nil {
		t.Fatalf("list --json: %v", err)
	}
	var got []quack.ServerConfig
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("output is not valid JSON (%v):\n%s", err, out.String())
	}
	if len(got) != 1 || got[0].Name != "prod" {
		t.Errorf("decoded %+v, want one connection named prod", got)
	}
}

func TestUnknownAndMissingArguments(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{"no subcommand", nil, "no subcommand"},
		{"unknown subcommand", []string{"frobnicate"}, "unknown subcommand"},
		{"ping without a name", []string{"ping"}, "usage: pintail ping"},
		{"query without sql", []string{"query", "prod"}, "usage: pintail query"},
		{"ping an unknown connection", []string{"ping", "nope"}, `no connection named "nope"`},
		{"query an unknown connection", []string{"query", "nope", "SELECT 1"}, `no connection named "nope"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env, _ := testEnv()
			err := Run(env, tc.args)
			if err == nil {
				t.Fatalf("want an error mentioning %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

func TestVersionAndHelp(t *testing.T) {
	for _, args := range [][]string{{"version"}, {"-v"}, {"--version"}} {
		env, out := testEnv()
		if err := Run(env, args); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if !strings.HasPrefix(out.String(), "pintail v") {
			t.Errorf("%v printed %q, want a version line", args, out.String())
		}
	}
	for _, args := range [][]string{{"help"}, {"-h"}, {"--help"}} {
		env, out := testEnv()
		if err := Run(env, args); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		// The help text is the only documentation a scripting user gets, so it
		// has to name every subcommand it accepts.
		for _, want := range []string{"list", "ping", "query", "version", "help", "--json"} {
			if !strings.Contains(out.String(), want) {
				t.Errorf("help does not mention %q:\n%s", want, out.String())
			}
		}
	}
}

func TestHasFlag(t *testing.T) {
	args := []string{"query", "prod", "SELECT 1", "--json"}
	if !HasFlag(args, "--json") {
		t.Error("--json should be found")
	}
	if HasFlag(args, "--verbose") {
		t.Error("--verbose is not present")
	}
	// The SQL argument must not be mistaken for a flag by a sloppy prefix match.
	if HasFlag([]string{"query", "p", "SELECT '--json'"}, "--json") {
		t.Error("a flag inside the SQL string is not a flag")
	}
}

// An offline connection has to fail with the reason, not a generic error: this
// is the message a cron job's log will contain.
func TestPingOfflineReportsTheReason(t *testing.T) {
	cfg := quack.ServerConfig{Name: "gone", Type: quack.ConnLocal, Path: "/nonexistent/db.duckdb"}
	env, out := testEnv(cfg)

	err := Run(env, []string{"ping", "gone"})
	if err == nil {
		t.Fatal("pinging a missing file should fail")
	}
	if !strings.Contains(out.String(), "offline") {
		t.Errorf("output should say the connection is offline:\n%s", out.String())
	}
}

func TestPingJSONShapeWhenOffline(t *testing.T) {
	cfg := quack.ServerConfig{Name: "gone", Type: quack.ConnLocal, Path: "/nonexistent/db.duckdb"}
	env, out := testEnv(cfg)

	// The error is returned for the exit status; the JSON is still written.
	_ = Run(env, []string{"ping", "gone", "--json"})

	var got map[string]interface{}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("output is not valid JSON (%v):\n%s", err, out.String())
	}
	if got["online"] != false {
		t.Errorf(`online = %v, want false`, got["online"])
	}
	if got["name"] != "gone" {
		t.Errorf(`name = %v, want "gone"`, got["name"])
	}
	if _, ok := got["error"]; !ok {
		t.Error("an offline ping should carry an error field")
	}
}

func TestQueryRefusesAnOfflineConnection(t *testing.T) {
	cfg := quack.ServerConfig{Name: "gone", Type: quack.ConnLocal, Path: "/nonexistent/db.duckdb"}
	env, _ := testEnv(cfg)

	err := Run(env, []string{"query", "gone", "SELECT 1"})
	if err == nil {
		t.Fatal("querying an offline connection should fail")
	}
	if !strings.Contains(err.Error(), "offline") {
		t.Errorf("error = %q, want it to say the connection is offline", err)
	}
}

// The subcommands are the scriptable surface, so they get exercised end to end
// against a real database file — the same client path the TUI uses.
func TestSubcommandsAgainstRealDuckDB(t *testing.T) {
	if _, err := exec.LookPath("duckdb"); err != nil {
		t.Skip("duckdb not in PATH — skipping integration test")
	}

	dbPath := filepath.Join(t.TempDir(), "fixture.duckdb")
	seed := "CREATE TABLE t AS SELECT range AS id, 'row_' || range AS label FROM range(3);"
	if out, err := exec.Command("duckdb", "-no-init", dbPath, "-c", seed).CombinedOutput(); err != nil {
		t.Fatalf("seeding fixture: %v\n%s", err, out)
	}
	cfg := quack.ServerConfig{Name: "fixture", Type: quack.ConnLocal, Path: dbPath}

	t.Run("ping", func(t *testing.T) {
		env, out := testEnv(cfg)
		if err := Run(env, []string{"ping", "fixture"}); err != nil {
			t.Fatalf("ping: %v", err)
		}
		if !strings.Contains(out.String(), "online") {
			t.Errorf("output = %q, want it to report the connection online", out.String())
		}
	})

	t.Run("ping --json", func(t *testing.T) {
		env, out := testEnv(cfg)
		if err := Run(env, []string{"ping", "fixture", "--json"}); err != nil {
			t.Fatalf("ping --json: %v", err)
		}
		var got map[string]interface{}
		if err := json.Unmarshal(out.Bytes(), &got); err != nil {
			t.Fatalf("not valid JSON (%v): %s", err, out.String())
		}
		if got["online"] != true {
			t.Errorf("online = %v, want true", got["online"])
		}
	})

	t.Run("query tab-separated", func(t *testing.T) {
		env, out := testEnv(cfg)
		if err := Run(env, []string{"query", "fixture", "SELECT id, label FROM t ORDER BY id"}); err != nil {
			t.Fatalf("query: %v", err)
		}
		lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
		if len(lines) != 4 {
			t.Fatalf("got %d lines, want a header and 3 rows:\n%s", len(lines), out.String())
		}
		if lines[0] != "id\tlabel" {
			t.Errorf("header = %q, want tab-separated column names", lines[0])
		}
		if lines[1] != "0\trow_0" {
			t.Errorf("first row = %q, want 0\\trow_0", lines[1])
		}
	})

	t.Run("query --json", func(t *testing.T) {
		env, out := testEnv(cfg)
		if err := Run(env, []string{"query", "fixture", "SELECT id FROM t ORDER BY id", "--json"}); err != nil {
			t.Fatalf("query --json: %v", err)
		}
		var got struct {
			Columns   []string   `json:"columns"`
			Rows      [][]string `json:"rows"`
			ElapsedMs int        `json:"elapsed_ms"`
		}
		if err := json.Unmarshal(out.Bytes(), &got); err != nil {
			t.Fatalf("not valid JSON (%v): %s", err, out.String())
		}
		if len(got.Columns) != 1 || got.Columns[0] != "id" {
			t.Errorf("columns = %v, want [id]", got.Columns)
		}
		if len(got.Rows) != 3 {
			t.Errorf("got %d rows, want 3", len(got.Rows))
		}
	})

	// A SQL error must reach the caller as a non-nil error carrying DuckDB's own
	// message, or a script has no way to know what went wrong.
	t.Run("a bad query returns the backend error", func(t *testing.T) {
		env, _ := testEnv(cfg)
		err := Run(env, []string{"query", "fixture", "SELECT no_such_column FROM t"})
		if err == nil {
			t.Fatal("want an error for an invalid column")
		}
		if !strings.Contains(err.Error(), "no_such_column") {
			t.Errorf("error = %q, want it to name the offending column", err)
		}
	})

	t.Run("a statement with no result set says so", func(t *testing.T) {
		env, out := testEnv(cfg)
		if err := Run(env, []string{"query", "fixture", "SET threads = 2"}); err != nil {
			t.Fatalf("query: %v", err)
		}
		if !strings.Contains(out.String(), "(no rows)") {
			t.Errorf("output = %q, want the no-rows notice", out.String())
		}
	})
}

// With no LoadConfigs override the commands read the real config path, so the
// default has to be wired up — otherwise every command would see an empty list.
func TestDefaultsReadTheConfigFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	cfgs := []quack.ServerConfig{{Name: "from-disk", Type: quack.ConnQuack, Host: "h", Port: 9494}}
	if err := quack.SaveServerConfigs(cfgs); err != nil {
		t.Fatalf("writing the config: %v", err)
	}

	var out bytes.Buffer
	if err := Run(Env{Out: &out, Err: &out}, []string{"list"}); err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(out.String(), "from-disk") {
		t.Errorf("list did not read the config file:\n%s", out.String())
	}
	if _, err := os.Stat(filepath.Join(home, ".duckdb", "pintail.json")); err != nil {
		t.Errorf("config was not written where expected: %v", err)
	}
}
