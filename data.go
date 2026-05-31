package main

import (
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

// CatalogTable is a physical table entry backed by object storage.
type CatalogTable struct {
	Name   string
	Format string // parquet | delta | iceberg
	Rows   int64
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
