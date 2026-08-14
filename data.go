package main

import (
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// ── domain types ──────────────────────────────────────────────────────────

// Connection represents a single active client session on a Quack server.
type Connection struct {
	ID       string
	IP       string
	Identity string
	Catalog  string
	Duration time.Duration
	Queries  int
	Status   string // "active" | "idle" | "error"
}

// ServerInfo is the display-oriented view of a server, derived from a
// ServerConfig. Live status (latency, online) lives in QuackClient.ConnState.
type ServerInfo struct {
	Name string
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

// ── mock / seed data ──────────────────────────────────────────────────────
//
// Earlier versions of Pintail shipped with fabricated "demo" connections and
// a fake catalog tree so the dashboard would look populated even when offline.
// That turned out to be confusing — it looks real, but none of it is. The
// honest default is empty state until a real server is reachable. See the
// "Getting started" section in the README for how to spin up a local
// Quack server, a local .duckdb file, or a test DuckLake to actually drive
// the dashboard.

var mockConnections []Connection = nil

var mockCatalog []CatalogSchema = nil

// refreshConnections used to fake live metric changes per tick. It now just
// returns its input unchanged — the dashboard reflects whatever the live
// session fetch returned, and nothing else.
func refreshConnections(conns []Connection) []Connection {
	return conns
}

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
