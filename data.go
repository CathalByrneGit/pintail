package main

import (
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// ── domain types ──────────────────────────────────────────────────────────

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

type tickMsg time.Time

// tickCmd fires every 2 seconds to simulate live data updates.
func tickCmd() tea.Cmd {
	return tea.Tick(2*time.Second, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

// Earlier versions shipped fabricated "demo" connections and a fake catalog
// tree so the dashboard looked populated when offline. That was removed for
// being confusing — it looked real and none of it was — leaving behind a set of
// nil stubs and a no-op refresh function, which are now gone too. The dashboard
// shows what a backend actually reported, per connection, and nothing else. See
// the README "Getting started" section for how to give it something to report.

// ── width-safe helpers ────────────────────────────────────────────────────
//
// Panel widths are derived from the terminal size (leftW = width*30/100 and
// friends), so on a narrow terminal the "width - padding" arithmetic every
// view does can land below zero. strings.Repeat panics on a negative count
// and s[:negative] panics too, which took the whole TUI down. These two
// helpers absorb that.

// hrule renders a horizontal rule n cells wide, clamping to empty rather
// than panicking when the caller's width arithmetic goes negative.
func hrule(n int) string {
	if n < 1 {
		return ""
	}
	return strings.Repeat("─", n)
}

// cutRunes returns at most the first n runes of s. Unlike s[:n] it never
// panics on a short string and never slices mid-codepoint.
func cutRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
