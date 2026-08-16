package quack

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

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

// ── SQL generators ────────────────────────────────────────────────────────

// timeTravelSQL renders the actual DuckLake time-travel pattern:
//
//	SELECT … FROM <table> AT (VERSION => N)
//
// This is non-destructive — it just queries the table as of snapshot N.
func TimeTravelSQL(cfg ServerConfig, snapshotID string) string {
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
func ExpireSnapshotsSQL(cfg ServerConfig, snapshotID string) string {
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

// Snapshots lists the DuckLake snapshots for this connection.
func (c *QuackClient) Snapshots(ctx context.Context) ([]Snapshot, error) {
	if !c.HasCLI() {
		return nil, fmt.Errorf("duckdb CLI not available")
	}
	state := c.GetState()
	if !state.Online {
		return nil, fmt.Errorf("connection offline (%s)", state.ErrMsg)
	}

	// snapshots() is a table macro DuckLake registers in the attached catalog's
	// default schema; it expands to ducklake_snapshots('_lake'), so both
	// spellings are valid once attachPrefix() has run ATTACH ... AS _lake.
	sql := "SELECT * FROM _lake.snapshots() ORDER BY snapshot_id DESC LIMIT 50;"
	// Output, not CombinedOutput: on success this output is parsed as JSON, and
	// folding stderr into it would let any duckdb warning corrupt the parse.
	// cliError still recovers stderr on failure, which is where it belongs.
	out, err := c.invocation(sql, "-json").command(ctx, c.cliPath).Output()
	if err != nil {
		return nil, fmt.Errorf("snapshot query failed: %s", cliError(err))
	}
	return parseSnapshotRows(out)
}

func parseSnapshotRows(data []byte) ([]Snapshot, error) {
	data = lastJSONArray(bytes.TrimSpace(data))
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
