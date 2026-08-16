package ui

import (
	"strings"
	"testing"

	"github.com/CathalByrneGit/pintail/internal/quack"
)

func lakeView(t *testing.T) SnapshotsView {
	t.Helper()
	cfgs := []quack.ServerConfig{
		{Name: "quack-a", Type: quack.ConnQuack, Host: "h", Port: 9494},
		{Name: "lake-a", Type: quack.ConnDuckLake, CatalogPath: "/tmp/a.duckdb", StoragePath: "/tmp/da"},
		{Name: "lake-b", Type: quack.ConnDuckLake, CatalogPath: "/tmp/b.duckdb", StoragePath: "/tmp/db"},
	}
	return NewSnapshotsView(quack.InitClients(cfgs, nil))
}

// The panel applies to backends with CapSnapshots, so a Quack connection must
// not be offered as a target.
func TestSnapshotsViewTargets(t *testing.T) {
	v := lakeView(t)
	if !v.HasLake() {
		t.Fatal("two DuckLake connections should give a target")
	}
	if got := v.TargetClient().Config.Name; got != "lake-a" {
		t.Errorf("target = %q, want lake-a", got)
	}

	v, _ = v.Update(key("tab"))
	if got := v.TargetClient().Config.Name; got != "lake-b" {
		t.Errorf("after tab, target = %q, want lake-b (the Quack connection is skipped)", got)
	}
	// tab wraps rather than running off the end.
	v, _ = v.Update(key("tab"))
	if got := v.TargetClient().Config.Name; got != "lake-a" {
		t.Errorf("tab should wrap, got %q", got)
	}

	none := NewSnapshotsView(quack.InitClients(
		[]quack.ServerConfig{{Name: "q", Type: quack.ConnQuack, Host: "h", Port: 9494}}, nil))
	if none.HasLake() || none.TargetClient() != nil {
		t.Error("a Quack-only setup has no snapshot target")
	}
	if none.FetchCmd() != nil {
		t.Error("nothing to fetch without a target")
	}
	if bar := none.ViewTargetBar(); !strings.Contains(bar, "no DuckLake connection") {
		t.Errorf("target bar should explain the empty state: %q", bar)
	}
	if f := none.ViewFooter(); !strings.Contains(f, "back") {
		t.Errorf("footer should still offer a way out: %q", f)
	}
}

// Cursor movement must stay inside the slice, including after a refresh returns
// fewer rows than are on screen — the shape of bug that has taken this app down
// before.
func TestSnapshotsViewCursorStaysInBounds(t *testing.T) {
	v := lakeView(t)
	snaps := []quack.Snapshot{
		{ID: "3", Time: "2026-08-14T09:00:00Z", SchemaVersion: "2"},
		{ID: "2", Time: "2026-08-13T09:00:00Z", SchemaVersion: "2"},
		{ID: "1", Time: "2026-08-12T09:00:00Z", SchemaVersion: "1"},
	}
	v, _ = v.Update(snapshotsResultMsg{snapshots: snaps})

	// Up at the top stays put.
	v, _ = v.Update(key("up"))
	if v.cursor != 0 {
		t.Errorf("cursor = %d, want 0", v.cursor)
	}
	for i := 0; i < 10; i++ {
		v, _ = v.Update(key("down"))
	}
	if v.cursor != 2 {
		t.Errorf("cursor = %d, want it clamped to the last snapshot", v.cursor)
	}

	// k/j are the same as up/down.
	v, _ = v.Update(key("k"))
	if v.cursor != 1 {
		t.Errorf("k should move up, cursor = %d", v.cursor)
	}
	v, _ = v.Update(key("j"))
	if v.cursor != 2 {
		t.Errorf("j should move down, cursor = %d", v.cursor)
	}

	// A shorter result set must not leave the cursor dangling.
	v, _ = v.Update(snapshotsResultMsg{snapshots: snaps[:1]})
	if v.cursor >= len(v.snapshots) {
		t.Fatalf("cursor = %d with %d snapshots", v.cursor, len(v.snapshots))
	}
	_ = v.ViewDetail(100) // would panic if it dangled
}

func TestSnapshotsViewReportsFetchOutcome(t *testing.T) {
	v := lakeView(t)

	v, _ = v.Update(snapshotsResultMsg{err: errString("Catalog Error: _lake.snapshots does not exist")})
	if got := v.ViewList(100); !strings.Contains(got, "Catalog Error") {
		t.Errorf("the backend error should be shown:\n%s", got)
	}
	if v.snapshots != nil {
		t.Error("a failed fetch should not leave stale rows on screen")
	}

	// An empty result is not an error, and says how to retry.
	v, _ = v.Update(snapshotsResultMsg{})
	got := v.ViewList(100)
	if !strings.Contains(got, "no snapshots returned") || !strings.Contains(got, "[r]") {
		t.Errorf("empty state should explain itself:\n%s", got)
	}

	// r refreshes.
	v, cmd := v.Update(key("r"))
	if cmd == nil {
		t.Error("[r] should issue a refresh")
	}
	if !v.loading {
		t.Error("a refresh should show as loading")
	}
	if got := v.ViewList(100); !strings.Contains(got, "loading") {
		t.Errorf("the loading state should be visible:\n%s", got)
	}
}

// The detail panel shows the snapshot's fields and both generated statements,
// with the destructive one labelled as such.
func TestSnapshotsViewDetailShowsBothStatements(t *testing.T) {
	v := lakeView(t)
	v, _ = v.Update(snapshotsResultMsg{snapshots: []quack.Snapshot{{
		ID: "7", Time: "2026-08-14T09:00:00Z", SchemaVersion: "3", Author: "etl",
		Raw: map[string]interface{}{"snapshot_id": 7, "schema_version": 3, "author": "etl"},
	}}})

	got := v.ViewDetail(120)
	for _, want := range []string{
		"TIME-TRAVEL READ", "AT (VERSION => 7)",
		"EXPIRE THIS SNAPSHOT", "ducklake_expire_snapshots",
		"DESTRUCTIVE", "copy into the scratchpad",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("detail is missing %q:\n%s", want, got)
		}
	}

	// Raw fields are listed in a stable order so the panel does not shuffle
	// between renders.
	first := v.ViewDetail(120)
	for i := 0; i < 5; i++ {
		if v.ViewDetail(120) != first {
			t.Fatal("detail panel is not stable between renders")
		}
	}
}

// Nothing selected: the panel says so rather than indexing an empty slice.
func TestSnapshotsViewDetailWithNothingSelected(t *testing.T) {
	v := lakeView(t)
	if got := v.ViewDetail(100); !strings.Contains(got, "select a snapshot") {
		t.Errorf("want a prompt to select something, got:\n%s", got)
	}
}
