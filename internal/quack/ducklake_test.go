package quack

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseDuckLakeSnapshots(t *testing.T) {
	cfg := ServerConfig{Name: "lake", Type: ConnDuckLake, CatalogPath: "/tmp/c.duckdb"}
	const rows = `[
	  {"snapshot_id":3,"snapshot_time":"2026-08-14T09:00:00Z","schema_version":2},
	  {"snapshot_id":2,"snapshot_time":"not a timestamp","schema_version":2},
	  {"snapshot_id":1,"snapshot_time":null,"schema_version":1}
	]`

	got, err := parseDuckLakeSnapshots([]byte(rows), cfg)
	if err != nil {
		t.Fatalf("parseDuckLakeSnapshots: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d pseudo-sessions, want 3", len(got))
	}
	if got[0].ID != "s3" {
		t.Errorf("ID = %q, want s3", got[0].ID)
	}
	if got[0].Identity != "snap v2" {
		t.Errorf("Identity = %q, want the schema version", got[0].Identity)
	}
	if got[0].IP != cfg.CatalogPath {
		t.Errorf("IP = %q, want the catalog path", got[0].IP)
	}
	if got[0].Duration <= 0 {
		t.Errorf("Duration = %v, want the age of the snapshot", got[0].Duration)
	}
	// An unparseable or null timestamp must not stop the row being listed; it
	// just has no age.
	if got[1].Duration != 0 || got[2].Duration != 0 {
		t.Errorf("a bad timestamp should leave Duration zero, got %v and %v",
			got[1].Duration, got[2].Duration)
	}
}

func TestParseDuckLakeSnapshotsEmpty(t *testing.T) {
	cfg := ServerConfig{Name: "lake", Type: ConnDuckLake}
	for _, in := range []string{"", "  ", "[]"} {
		if _, err := parseDuckLakeSnapshots([]byte(in), cfg); err == nil {
			t.Errorf("parseDuckLakeSnapshots(%q): want an error rather than a silent empty list", in)
		}
	}
}

// Both generated statements have to carry the ATTACH prologue, or pasting them
// into a fresh session does nothing useful. The expire statement is the
// destructive one and must say so.
func TestSnapshotSQLGenerators(t *testing.T) {
	direct := ServerConfig{Name: "lake", Type: ConnDuckLake,
		CatalogPath: "/tmp/c.duckdb", StoragePath: "/tmp/d"}
	viaRef := ServerConfig{Name: "lake", Type: ConnDuckLake,
		CatalogRef: "central", StoragePath: "s3://b/lake"}

	tt := TimeTravelSQL(direct, "7")
	for _, want := range []string{
		"ATTACH 'ducklake:/tmp/c.duckdb' AS _lake (DATA_PATH '/tmp/d')",
		"USE _lake",
		"AT (VERSION => 7)",
		"non-destructive",
	} {
		if !strings.Contains(tt, want) {
			t.Errorf("time-travel SQL missing %q:\n%s", want, tt)
		}
	}

	exp := ExpireSnapshotsSQL(direct, "7")
	for _, want := range []string{
		"DESTRUCTIVE",
		"ducklake_expire_snapshots('_lake', versions => [7])",
		"cannot be recovered",
	} {
		if !strings.Contains(exp, want) {
			t.Errorf("expire SQL missing %q:\n%s", want, exp)
		}
	}

	// A catalog-by-reference lake needs the two-step attach, and must not
	// silently fall back to a catalog path it does not have.
	ref := TimeTravelSQL(viaRef, "2")
	for _, want := range []string{"<catalog conn central>", "ATTACH 'ducklake:_catalog' AS _lake"} {
		if !strings.Contains(ref, want) {
			t.Errorf("catalog-ref SQL missing %q:\n%s", want, ref)
		}
	}
}

// The snapshot listing runs real SQL against a real DuckLake. The ducklake
// extension has to be downloadable for this to run at all, so it is skipped
// where it is not — CI is where it counts.
func TestSnapshotsAgainstRealDuckLake(t *testing.T) {
	if _, err := exec.LookPath("duckdb"); err != nil {
		t.Skip("duckdb not in PATH")
	}
	if out, err := exec.Command("duckdb", "-no-init", "-c",
		"INSTALL ducklake; LOAD ducklake;").CombinedOutput(); err != nil {
		t.Skipf("ducklake extension unavailable here: %s", strings.TrimSpace(string(out)))
	}

	dir := t.TempDir()
	catalog := filepath.Join(dir, "catalog.duckdb")
	data := filepath.Join(dir, "data")

	seed := `INSTALL ducklake; LOAD ducklake;
	         ATTACH 'ducklake:` + catalog + `' AS lake (DATA_PATH '` + data + `');
	         USE lake;
	         CREATE TABLE orders AS SELECT range AS id FROM range(100);
	         INSERT INTO orders SELECT range + 100 FROM range(50);`
	if out, err := exec.Command("duckdb", "-no-init", "-c", seed).CombinedOutput(); err != nil {
		t.Fatalf("seeding the lake: %v\n%s", err, out)
	}

	cfg := ServerConfig{Name: "lake", Type: ConnDuckLake, CatalogPath: catalog, StoragePath: data}
	c := NewQuackClient(cfg, nil, nil)
	if _, err := c.Ping(context.Background()); err != nil {
		t.Fatalf("ping: %v", err)
	}

	snaps, err := c.Snapshots(context.Background())
	if err != nil {
		t.Fatalf("Snapshots: %v", err)
	}
	// Creating the table and inserting are separate commits, so there is more
	// than one snapshot and they come back newest first.
	if len(snaps) < 2 {
		t.Fatalf("got %d snapshots, want at least 2 (create + insert)", len(snaps))
	}
	if snaps[0].ID == "" || snaps[0].SchemaVersion == "" {
		t.Errorf("snapshot fields are empty: %+v", snaps[0])
	}
	if snaps[0].ID <= snaps[1].ID {
		t.Errorf("snapshots should be newest first, got %q then %q", snaps[0].ID, snaps[1].ID)
	}

	// And the generated time-travel statement has to actually run.
	tt := TimeTravelSQL(cfg, snaps[len(snaps)-1].ID)
	tt = strings.ReplaceAll(tt, "<table_name>", "orders")
	if out, err := exec.Command("duckdb", "-no-init", "-c", tt).CombinedOutput(); err != nil {
		t.Errorf("generated time-travel SQL does not run: %v\n%s\n--- SQL:\n%s", err, out, tt)
	}
}
