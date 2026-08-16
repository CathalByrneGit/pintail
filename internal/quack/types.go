package quack

import (
	"strings"
	"time"
)

// Types shared between the data layer and the screens that display it. They
// live here rather than in the UI because they are what the client returns:
// the `pintail` subcommands and any future consumer get them without importing
// a terminal library.

// Connection represents a single client session on a Quack server, as reported
// by quack_active_connections().
type Connection struct {
	ID       string
	IP       string
	Identity string
	Catalog  string
	Duration time.Duration
	Queries  int
	Status   string // "idle" | "active" | "finished" | "cancelled"
	// Query is the SQL the session is running, when the backend reports one.
	// It is too long for a table column, so the dashboard shows it in the
	// footer for the selected row.
	Query string
}

// ServerInfo is the display-oriented view of a server, derived from a
// ServerConfig. Live status (latency, online) lives in QuackClient.ConnState.
//
// Type and URI are carried here because without them the scratchpad had only
// Host/Port to work from and rendered every target as quack://host:port —
// including local files, which showed up as the nonsensical "quack://:0".
type ServerInfo struct {
	Name string
	Type ConnType
	URI  string // as ServerConfig.DisplayURI() renders it
	Host string
	Port int
	TLS  bool
}

// CatalogSchema is a named namespace in the DuckLake catalog.
type CatalogSchema struct {
	Name   string
	Tables []CatalogTable
	Open   bool // whether the tree node is expanded in the UI
}

// CatalogTable is one relation in the catalog.
type CatalogTable struct {
	Name   string
	Format string // "table" | "view" — what the catalog reports it as
	Rows   int64
	// SizeKnown distinguishes "estimated at 0 rows" from "no estimate
	// available" (views don't carry one), so the UI can stay silent instead of
	// printing a row count the backend never gave us.
	SizeKnown bool
}

// ── tick message ──────────────────────────────────────────────────────────

// QueryResult is returned from any query execution path.
type QueryResult struct {
	Query     string
	Columns   []string
	Rows      [][]string
	ElapsedMs int
	Timestamp time.Time
	Err       string
	Method    string // "cli" | "http" | "offline"
}

// Earlier versions had a mock executor that returned fake demo rows when no
// real connection was available. That was confusing — the scratchpad returned
// results and none of them were real. What replaced it is this: say the
// connection is offline and point at the README. (It was still called
// mockExecute, and took a ServerInfo it ignored, until the last of the mock
// scaffolding went.)

func OfflineResult(query string) QueryResult {
	return QueryResult{
		Query:     query,
		Timestamp: time.Now(),
		Method:    "offline",
		Err:       "no online connection — start a Quack server, point at a .duckdb file, or attach a DuckLake (see README \"Getting started\")",
	}
}

// CutRunes returns at most the first n runes of s. Unlike s[:n] it never
// panics on a short string and never slices mid-codepoint.
func CutRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func SplitTrim(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// IsEmpty reports whether the result has no data rows worth exporting.
func (r *QueryResult) IsEmpty() bool {
	return r == nil || (len(r.Columns) == 0 && len(r.Rows) == 0) || r.Err != ""
}
