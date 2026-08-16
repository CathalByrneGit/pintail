package ui

import (
	"context"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/CathalByrneGit/pintail/internal/quack"
)

// Bubble Tea plumbing for the client's synchronous API.
//
// The client itself returns values and errors and knows nothing about the
// update loop; the messages and the tea.Cmd wrappers that carry results back to
// it live here. That split is what keeps "talks to duckdb" separable from
// "draws a terminal" — the client can be driven from the `pintail query`
// subcommand, or a test, without a Bubble Tea program in the picture.

// ── messages ──────────────────────────────────────────────────────────────

// pingResultMsg is sent when a server ping completes.
type pingResultMsg struct {
	idx     int
	latency time.Duration
	err     error
}

// queryResultMsg is sent when a query completes. The result always carries its
// own error (in QueryResult.Err), so there is no separate error field.
type queryResultMsg struct {
	result *quack.QueryResult
}

// sessionResultMsg is sent when a session poll completes.
type sessionResultMsg struct {
	// idx identifies which connection this result describes. Without it the
	// dashboard stored one global result set, so with several servers online
	// the last responder won and nothing said whose data was on screen.
	idx         int
	connections []quack.Connection
	// reportedCount is the connection count the backend gave us, if any. It is
	// carried separately from `connections` because DuckDB exposes a count but
	// not always a per-connection listing — the two are different facts.
	reportedCount string
	err           error
}

// catalogResultMsg is sent when a catalog poll completes.
type catalogResultMsg struct {
	idx     int // which connection this catalog belongs to
	catalog []quack.CatalogSchema
	err     error
}

// ── commands ──────────────────────────────────────────────────────────────

// pingServerCmd launches an async ping for clients[idx].
func pingServerCmd(c *quack.QuackClient, idx int) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		lat, err := c.Ping(ctx)
		return pingResultMsg{idx: idx, latency: lat, err: err}
	}
}

// queryCmd runs a query and delivers the result to the update loop. Cancelling
// ctx aborts it — the CLI subprocess is killed with it — which is what makes
// ctrl+c in the scratchpad able to interrupt a long-running statement.
func queryCmd(c *quack.QuackClient, ctx context.Context, sql string) tea.Cmd {
	return func() tea.Msg {
		return queryResultMsg{result: c.Query(ctx, sql)}
	}
}

// fetchSessionsCmd polls one connection for its active sessions.
func fetchSessionsCmd(c *quack.QuackClient, idx int) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		conns, reported, err := c.Sessions(ctx)
		return sessionResultMsg{idx: idx, connections: conns, reportedCount: reported, err: err}
	}
}

// fetchCatalogCmd lists one connection's relations.
func fetchCatalogCmd(c *quack.QuackClient, idx int) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		schemas, err := c.Catalog(ctx)
		return catalogResultMsg{idx: idx, catalog: schemas, err: err}
	}
}
