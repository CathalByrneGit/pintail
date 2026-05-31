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

// ── mock data ─────────────────────────────────────────────────────────────

var mockConnections = []Connection{
	{ID: "c01", IP: "10.0.1.5", Identity: "analyst1", Catalog: "analytics", Duration: 14 * time.Minute, Queries: 23, Status: "active"},
	{ID: "c02", IP: "10.0.1.7", Identity: "etl_bot", Catalog: "raw", Duration: 2*time.Hour + 3*time.Minute, Queries: 1847, Status: "active"},
	{ID: "c03", IP: "10.0.2.3", Identity: "ml_svc", Catalog: "analytics", Duration: 47 * time.Minute, Queries: 312, Status: "idle"},
	{ID: "c04", IP: "10.0.2.8", Identity: "dashboard", Catalog: "analytics", Duration: 8 * time.Minute, Queries: 5, Status: "active"},
	{ID: "c05", IP: "192.168.1.9", Identity: "unknown_client", Catalog: "—", Duration: 2 * time.Minute, Queries: 0, Status: "error"},
}

var mockCatalog = []CatalogSchema{
	{Name: "analytics", Open: true, Tables: []CatalogTable{
		{Name: "orders", Format: "parquet", Rows: 4_823_901},
		{Name: "customers", Format: "parquet", Rows: 281_450},
		{Name: "events", Format: "parquet", Rows: 92_047_332},
	}},
	{Name: "raw", Open: true, Tables: []CatalogTable{
		{Name: "logs", Format: "parquet", Rows: 412_983_010},
		{Name: "metrics", Format: "delta", Rows: 88_291_004},
		{Name: "traces", Format: "parquet", Rows: 12_003_441},
	}},
	{Name: "staging", Open: false, Tables: []CatalogTable{
		{Name: "orders_staging", Format: "parquet", Rows: 10_231},
		{Name: "events_staging", Format: "parquet", Rows: 882_331},
	}},
}

// refreshConnections simulates live metric changes on each tick.
func refreshConnections(conns []Connection) []Connection {
	increments := []int{3, 127, 0, 1, 0}
	updated := make([]Connection, len(conns))
	copy(updated, conns)
	for i := range updated {
		updated[i].Duration += 2 * time.Second
		if updated[i].Status == "active" && i < len(increments) {
			updated[i].Queries += increments[i]
		}
	}
	return updated
}
