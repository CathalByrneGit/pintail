package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// ── types ─────────────────────────────────────────────────────────────────

// Snapshot is one row from <catalog>.snapshots(). All fields are strings so we
// don't have to know the DuckLake schema version in advance — the JSON values
// are rendered verbatim.
type Snapshot struct {
	ID            string
	Time          string
	SchemaVersion string
	Author        string
	Raw           map[string]interface{} // full row for the detail panel
}

// snapshotsResultMsg is sent on the Bubble Tea bus when a snapshot fetch returns.
type snapshotsResultMsg struct {
	idx       int // which client this is for
	snapshots []Snapshot
	err       error
}

// SnapshotsView is the DuckLake snapshots panel state.
type SnapshotsView struct {
	clients []*QuackClient
	// Indices into clients that are DuckLake connections.
	lakeIdxs []int
	// Index into lakeIdxs identifying the currently-targeted lake.
	targetPos int

	snapshots []Snapshot
	cursor    int
	loading   bool
	errMsg    string
}

// ── constructor ───────────────────────────────────────────────────────────

func NewSnapshotsView(clients []*QuackClient) SnapshotsView {
	v := SnapshotsView{clients: clients}
	// Snapshot panel applies to any backend with CapSnapshots — currently
	// only DuckLake, but the capability check decouples this UI from the
	// underlying type so adding a fourth backend with snapshot semantics
	// won't require changes here.
	for i, c := range clients {
		if c.Config.Supports(CapSnapshots) {
			v.lakeIdxs = append(v.lakeIdxs, i)
		}
	}
	return v
}

// HasLake reports whether any DuckLake connection is configured.
func (v SnapshotsView) HasLake() bool { return len(v.lakeIdxs) > 0 }

// TargetClient returns the currently-selected DuckLake client, or nil.
func (v SnapshotsView) TargetClient() *QuackClient {
	if !v.HasLake() {
		return nil
	}
	return v.clients[v.lakeIdxs[v.targetPos]]
}

// FetchCmd returns a tea.Cmd that loads snapshots for the current target.
func (v SnapshotsView) FetchCmd() tea.Cmd {
	c := v.TargetClient()
	if c == nil {
		return nil
	}
	idx := v.lakeIdxs[v.targetPos]
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		snaps, err := fetchSnapshots(ctx, c)
		return snapshotsResultMsg{idx: idx, snapshots: snaps, err: err}
	}
}

// ── Update ────────────────────────────────────────────────────────────────

func (v SnapshotsView) Update(msg tea.Msg) (SnapshotsView, tea.Cmd) {
	switch msg := msg.(type) {
	case snapshotsResultMsg:
		v.loading = false
		if msg.err != nil {
			v.errMsg = msg.err.Error()
			v.snapshots = nil
		} else {
			v.errMsg = ""
			v.snapshots = msg.snapshots
			if v.cursor >= len(v.snapshots) {
				v.cursor = 0
			}
		}
		return v, nil

	case tea.KeyMsg:
		switch msg.String() {
		case "up", "k":
			if v.cursor > 0 {
				v.cursor--
			}
		case "down", "j":
			if v.cursor < len(v.snapshots)-1 {
				v.cursor++
			}
		case "tab":
			if len(v.lakeIdxs) > 1 {
				v.targetPos = (v.targetPos + 1) % len(v.lakeIdxs)
				v.snapshots = nil
				v.cursor = 0
				v.loading = true
				return v, v.FetchCmd()
			}
		case "r":
			v.loading = true
			return v, v.FetchCmd()
		}
	}
	return v, nil
}

// ── View helpers ──────────────────────────────────────────────────────────

func (v SnapshotsView) ViewTargetBar() string {
	if !v.HasLake() {
		return "  " + redStyle.Render("✕ no DuckLake connection configured") +
			mutedStyle.Render("  add one with [a] from the dashboard")
	}
	c := v.TargetClient()
	cnt := mutedStyle.Render(fmt.Sprintf("  (lake %d of %d)", v.targetPos+1, len(v.lakeIdxs)))
	return "  " +
		mutedStyle.Render("target  ") +
		labelStyle.Render(c.Config.Name) +
		mutedStyle.Render("  "+c.Config.DisplayURI()) +
		cnt
}

func (v SnapshotsView) ViewList(width int) string {
	lines := []string{labelStyle.Render("SNAPSHOTS"), ""}

	switch {
	case !v.HasLake():
		lines = append(lines, mutedStyle.Render("nothing to show"))
	case v.loading:
		lines = append(lines, amberStyle.Render("⟳ loading…"))
	case v.errMsg != "":
		lines = append(lines, redStyle.Render("✕ "+v.errMsg))
	case len(v.snapshots) == 0:
		lines = append(lines, mutedStyle.Render("no snapshots returned"))
		lines = append(lines, mutedStyle.Render("press [r] to retry"))
	default:
		for i, s := range v.snapshots {
			cursor := "  "
			nameStyle := brightStyle
			if i == v.cursor {
				cursor = amberStyle.Render("▶ ")
				nameStyle = labelStyle
			}
			label := "snap " + s.ID
			detail := mutedStyle.Render("  v" + s.SchemaVersion + "  " + truncate(s.Time, 19))
			lines = append(lines, cursor+nameStyle.Render(label)+detail)
		}
	}
	return strings.Join(lines, "\n")
}

func (v SnapshotsView) ViewDetail(width int) string {
	lines := []string{labelStyle.Render("DETAIL"), ""}

	if !v.HasLake() || v.cursor >= len(v.snapshots) {
		lines = append(lines, mutedStyle.Render("select a snapshot on the left"))
		return strings.Join(lines, "\n")
	}

	s := v.snapshots[v.cursor]

	// Field rows from the raw map (sorted by key for stability).
	keys := make([]string, 0, len(s.Raw))
	for k := range s.Raw {
		keys = append(keys, k)
	}
	// Simple alpha sort
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j-1] > keys[j]; j-- {
			keys[j-1], keys[j] = keys[j], keys[j-1]
		}
	}
	for _, k := range keys {
		val := fmt.Sprintf("%v", s.Raw[k])
		lines = append(lines,
			mutedStyle.Render(padRight(k, 16))+brightStyle.Render(truncate(val, width-20)))
	}

	// Generated SQL for time-travel and snapshot expiration — read-only display.
	c := v.TargetClient()
	if c != nil {
		lines = append(lines,
			"",
			mutedStyle.Render(hrule(width-4)),
			"",
			labelStyle.Render("TIME-TRAVEL READ"),
			"",
			renderCodeBlock(timeTravelSQL(c.Config, s.ID), width-4),
			"",
			labelStyle.Render("EXPIRE THIS SNAPSHOT"),
			"",
			renderCodeBlock(expireSnapshotsSQL(c.Config, s.ID), width-4),
			"",
			mutedStyle.Render("(destructive — copy into the scratchpad to run)"),
		)
	}
	return strings.Join(lines, "\n")
}

func (v SnapshotsView) ViewFooter() string {
	if !v.HasLake() {
		return footerStyle.Render(keyBadge("esc") + " back to dashboard")
	}
	keys := []string{
		keyBadge("↑↓") + " select",
		keyBadge("r") + " refresh",
	}
	if len(v.lakeIdxs) > 1 {
		keys = append(keys, keyBadge("tab")+" cycle lake")
	}
	keys = append(keys, keyBadge("esc")+" back")
	return footerStyle.Render(strings.Join(keys, "   "))
}

// ── SQL generators ────────────────────────────────────────────────────────

// timeTravelSQL renders the actual DuckLake time-travel pattern:
//
//	SELECT … FROM <table> AT (VERSION => N)
//
// This is non-destructive — it just queries the table as of snapshot N.
func timeTravelSQL(cfg ServerConfig, snapshotID string) string {
	return fmt.Sprintf(
		"-- Read tables as of snapshot %s (non-destructive)\n"+
			"%s\n"+
			"SELECT * FROM <table_name> AT (VERSION => %s);",
		snapshotID,
		strings.TrimSpace(catalogAttachOnly(cfg)),
		snapshotID,
	)
}

// expireSnapshotsSQL renders the snapshot-expiration call. This is the closest
// thing DuckLake has to a "rollback" — it physically removes the data files
// for the listed snapshots. The older state cannot be recovered after this.
//
// (DuckLake has no in-place rollback. To restore an old state in a new
// database, use:  CREATE DATABASE new_db FROM <current_db> (SNAPSHOT_ID => N).)
func expireSnapshotsSQL(cfg ServerConfig, snapshotID string) string {
	return fmt.Sprintf(
		"-- DESTRUCTIVE: physically remove snapshot %s and its data files.\n"+
			"-- Older state cannot be recovered after this.\n"+
			"%s\n"+
			"CALL ducklake_expire_snapshots('_lake', versions => [%s]);",
		snapshotID,
		strings.TrimSpace(catalogAttachOnly(cfg)),
		snapshotID,
	)
}

// catalogAttachOnly returns just the ATTACH portion of the prefix, formatted
// across multiple lines for readability inside the displayed SQL block.
func catalogAttachOnly(cfg ServerConfig) string {
	switch {
	case cfg.CatalogRef != "":
		return fmt.Sprintf("ATTACH '<catalog conn %s>' AS _catalog;\nATTACH 'ducklake:_catalog' AS _lake (DATA_PATH '%s');\nUSE _lake;",
			cfg.CatalogRef, cfg.StoragePath)
	default:
		return fmt.Sprintf("ATTACH 'ducklake:%s' AS _lake (DATA_PATH '%s');\nUSE _lake;",
			cfg.CatalogPath, cfg.StoragePath)
	}
}

// ── snapshot fetch ────────────────────────────────────────────────────────

func fetchSnapshots(ctx context.Context, c *QuackClient) ([]Snapshot, error) {
	if !c.HasCLI() {
		return nil, fmt.Errorf("duckdb CLI not available")
	}
	state := c.GetState()
	if !state.Online {
		return nil, fmt.Errorf("connection offline (%s)", state.ErrMsg)
	}

	// Since attachPrefix() already does `USE _lake`, the snapshots() function
	// is resolved against the attached catalog (it's a method on _lake, not a
	// standalone function — `ducklake_snapshots()` does not exist).
	sql := "SELECT * FROM _lake.snapshots() ORDER BY snapshot_id DESC LIMIT 50;"
	script := c.attachPrefix() + sql

	cmd := exec.CommandContext(ctx, c.cliPath, "-json", "-c", script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("snapshot query failed: %s", strings.TrimSpace(string(out)))
	}
	return parseSnapshotRows(out)
}

func parseSnapshotRows(data []byte) ([]Snapshot, error) {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || string(data) == "[]" {
		return nil, nil
	}
	var rows []map[string]interface{}
	if err := json.Unmarshal(data, &rows); err != nil {
		return nil, err
	}
	snaps := make([]Snapshot, 0, len(rows))
	for _, row := range rows {
		s := Snapshot{Raw: row}
		if v, ok := row["snapshot_id"]; ok {
			s.ID = fmt.Sprintf("%v", v)
		}
		if v, ok := row["snapshot_time"]; ok {
			s.Time = fmt.Sprintf("%v", v)
		}
		if v, ok := row["schema_version"]; ok {
			s.SchemaVersion = fmt.Sprintf("%v", v)
		}
		if v, ok := row["author"]; ok {
			s.Author = fmt.Sprintf("%v", v)
		}
		snaps = append(snaps, s)
	}
	return snaps, nil
}
