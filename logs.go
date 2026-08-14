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

// LogEntry is one row of the Quack message log. Fields are strings because the
// log's schema is registered by the extension and can gain columns between
// versions; anything unrecognised stays available in Raw for the detail panel.
type LogEntry struct {
	Timestamp    string
	MessageType  string
	ConnectionID string
	ClientQuery  string // client_query_id, for correlating client and server logs
	Query        string
	DurationMs   string
	ResponseType string
	Err          string
	Raw          map[string]interface{}
}

// Failed reports whether this entry recorded an error.
func (e LogEntry) Failed() bool {
	return e.Err != "" || strings.EqualFold(e.ResponseType, "ERROR")
}

type logsResultMsg struct {
	idx     int
	entries []LogEntry
	err     error
}

// logsEnabledMsg is the result of turning logging on for a server.
type logsEnabledMsg struct {
	target string
	err    string
}

// LogsView is the Quack message-log screen.
//
// The log is the closest thing Quack has to request tracing: every message with
// its type, the server-issued connection id, the SQL, the round-trip duration
// and any error. It lives on the server, so it is read with quack_query the same
// way the session list is.
type LogsView struct {
	clients []*QuackClient
	// quackIdxs are the indices of connections whose backend has a Quack log.
	quackIdxs []int
	targetPos int

	entries []LogEntry
	cursor  int
	loading bool
	errMsg  string

	// notice reports the outcome of enabling logging, which is a change to
	// server state and so is never done implicitly by a poll.
	notice    string
	noticeErr bool

	width int
}

// ── constructor ───────────────────────────────────────────────────────────

func NewLogsView(clients []*QuackClient) LogsView {
	v := LogsView{clients: clients}
	for i, c := range clients {
		// The log is a Quack-server thing: a local file has no message log, and
		// a DuckLake catalog is reached without the protocol.
		if c.Config.Type == ConnQuack {
			v.quackIdxs = append(v.quackIdxs, i)
		}
	}
	return v
}

// HasTarget reports whether any connection can have a Quack log.
func (v LogsView) HasTarget() bool { return len(v.quackIdxs) > 0 }

// TargetClient returns the currently-selected Quack client, or nil.
func (v LogsView) TargetClient() *QuackClient {
	if !v.HasTarget() {
		return nil
	}
	return v.clients[v.quackIdxs[v.targetPos]]
}

// ── commands ──────────────────────────────────────────────────────────────

// logSQL reads the most recent entries. SELECT * rather than named columns: the
// log's schema comes from the extension, so asking for a column it does not have
// would fail the whole fetch. Entries are filtered in Go rather than SQL for the
// same reason.
const logSQL = `SELECT * FROM duckdb_logs_parsed('Quack') ORDER BY timestamp DESC LIMIT 200`

// FetchCmd loads the log for the current target.
func (v LogsView) FetchCmd() tea.Cmd {
	c := v.TargetClient()
	if c == nil {
		return nil
	}
	idx := v.quackIdxs[v.targetPos]
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		if !c.HasCLI() {
			return logsResultMsg{idx: idx, err: fmt.Errorf("duckdb CLI not found in PATH")}
		}
		cmd := exec.CommandContext(ctx, c.cliPath, "-json", "-c", c.Config.quackQuerySQL(logSQL))
		out, err := cmd.Output()
		if err != nil {
			return logsResultMsg{idx: idx, err: fmt.Errorf("%s", cliError(err))}
		}
		entries, err := parseLogRows(out)
		return logsResultMsg{idx: idx, entries: entries, err: err}
	}
}

// EnableCmd turns on Quack logging for the current target.
//
// This changes state on someone's server, so it is bound to a key rather than
// done by the poll: an admin tool should not quietly start logging every query
// on a production instance because a screen was opened.
func (v LogsView) EnableCmd() tea.Cmd {
	c := v.TargetClient()
	if c == nil {
		return nil
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := c.runServerSQL(ctx, "CALL enable_logging('Quack')"); err != nil {
			return logsEnabledMsg{target: c.Config.Name, err: firstLine(err.Error())}
		}
		return logsEnabledMsg{target: c.Config.Name}
	}
}

// parseLogRows converts duckdb_logs_parsed output into entries, newest first.
//
// Pintail's own log-reading query shows up in the log it is reading; those rows
// are dropped so the panel shows the server's traffic rather than the fact that
// it is being watched.
func parseLogRows(data []byte) ([]LogEntry, error) {
	data = lastJSONArray(bytes.TrimSpace(data))
	if len(data) == 0 || string(data) == "[]" {
		return nil, nil
	}
	var rows []map[string]interface{}
	if err := json.Unmarshal(data, &rows); err != nil {
		return nil, err
	}

	field := func(row map[string]interface{}, key string) string {
		if v, ok := row[key]; ok && v != nil {
			return fmt.Sprintf("%v", v)
		}
		return ""
	}

	entries := make([]LogEntry, 0, len(rows))
	for _, row := range rows {
		q := field(row, "query")
		if strings.Contains(q, "duckdb_logs_parsed") {
			continue // our own poll
		}
		entries = append(entries, LogEntry{
			Timestamp:    field(row, "timestamp"),
			MessageType:  field(row, "message_type"),
			ConnectionID: field(row, "quack_connection_id"),
			ClientQuery:  field(row, "client_query_id"),
			Query:        q,
			DurationMs:   field(row, "duration_ms"),
			ResponseType: field(row, "response_type"),
			Err:          field(row, "error"),
			Raw:          row,
		})
	}
	return entries, nil
}

// ── Update ────────────────────────────────────────────────────────────────

func (v LogsView) Update(msg tea.Msg) (LogsView, tea.Cmd) {
	switch msg := msg.(type) {
	case logsResultMsg:
		v.loading = false
		if msg.err != nil {
			v.errMsg = msg.err.Error()
			return v, nil
		}
		v.errMsg = ""
		v.entries = msg.entries
		if v.cursor >= len(v.entries) {
			v.cursor = 0
		}
		return v, nil

	case logsEnabledMsg:
		if msg.err != "" {
			v.notice = "could not enable logging on " + msg.target + ": " + msg.err
			v.noticeErr = true
			return v, nil
		}
		v.notice = "logging enabled on " + msg.target
		v.noticeErr = false
		v.loading = true
		return v, v.FetchCmd()

	case tea.KeyMsg:
		switch msg.String() {
		case "up", "k":
			if v.cursor > 0 {
				v.cursor--
			}
		case "down", "j":
			if v.cursor < len(v.entries)-1 {
				v.cursor++
			}
		case "r":
			if v.HasTarget() {
				v.loading = true
				return v, v.FetchCmd()
			}
		case "e":
			if v.HasTarget() {
				v.notice = "enabling logging…"
				v.noticeErr = false
				return v, v.EnableCmd()
			}
		case "tab":
			if len(v.quackIdxs) > 1 {
				v.targetPos = (v.targetPos + 1) % len(v.quackIdxs)
				v.entries = nil
				v.cursor = 0
				v.errMsg = ""
				v.notice = ""
				v.loading = true
				return v, v.FetchCmd()
			}
		}
	}
	return v, nil
}

// ── views ─────────────────────────────────────────────────────────────────

func (v LogsView) ViewTargetBar() string {
	if !v.HasTarget() {
		return "  " + redStyle.Render("✕ no Quack connection configured") +
			mutedStyle.Render("  the message log is a Quack server feature")
	}
	c := v.TargetClient()
	bar := "  " + mutedStyle.Render("target  ") + labelStyle.Render(c.Config.Name) +
		mutedStyle.Render("  "+c.Config.DisplayURI())
	if len(v.quackIdxs) > 1 {
		bar += mutedStyle.Render(fmt.Sprintf("  (server %d of %d)", v.targetPos+1, len(v.quackIdxs)))
	}
	if v.notice != "" {
		if v.noticeErr {
			bar += "   " + redStyle.Render("✕ "+v.notice)
		} else {
			bar += "   " + greenStyle.Render("✓ "+v.notice)
		}
	}
	return bar
}

// ViewTable renders the log as fixed-width columns sized to the panel.
func (v LogsView) ViewTable(width int) string {
	lines := []string{labelStyle.Render("QUACK MESSAGE LOG")}

	switch {
	case !v.HasTarget():
		return strings.Join(append(lines, "", mutedStyle.Render("nothing to show")), "\n")
	case v.loading:
		return strings.Join(append(lines, "", amberStyle.Render("⟳ loading…")), "\n")
	case v.errMsg != "":
		lines = append(lines, "", redStyle.Render("✕ "+truncate(firstLine(v.errMsg), width-4)))
		// The one failure worth explaining: the log type only exists where the
		// quack extension is loaded, and only carries rows once enabled.
		if strings.Contains(v.errMsg, "structured_log_schema") {
			lines = append(lines, "",
				mutedStyle.Render("  the Quack log type is not registered on that instance —"),
				mutedStyle.Render("  it appears once the quack extension is loaded there"))
		}
		return strings.Join(lines, "\n")
	case len(v.entries) == 0:
		return strings.Join(append(lines, "",
			mutedStyle.Render("no entries"),
			"",
			mutedStyle.Render("Quack logging is off until it is turned on."),
			mutedStyle.Render("press [e] to run CALL enable_logging('Quack') on the server,"),
			mutedStyle.Render("then [r] to refresh"),
		), "\n")
	}

	// time | type | conn | ms | response — query takes what is left
	tw, mt, cn, ms, rt := 12, 18, 10, 6, 18
	qw := width - (tw + mt + cn + ms + rt + 12)
	if qw < 10 {
		qw = 10
	}

	lines = append(lines, "",
		mutedStyle.Render(
			padRight("TIME", tw)+" "+padRight("MESSAGE", mt)+" "+padRight("CONN", cn)+" "+
				padRight("MS", ms)+" "+padRight("RESPONSE", rt)+" "+padRight("QUERY", qw)))

	for i, e := range v.entries {
		cursor := "  "
		if i == v.cursor {
			cursor = amberStyle.Render("▶ ")
		}

		style := brightStyle
		if e.Failed() {
			style = redStyle
		}

		row := padRight(truncate(clockOf(e.Timestamp), tw), tw) + " " +
			padRight(truncate(e.MessageType, mt), mt) + " " +
			padRight(truncate(e.ConnectionID, cn), cn) + " " +
			padRight(truncate(e.DurationMs, ms), ms) + " " +
			padRight(truncate(e.ResponseType, rt), rt) + " " +
			truncate(firstLine(e.Query), qw)
		lines = append(lines, cursor+style.Render(row))
	}
	return strings.Join(lines, "\n")
}

// ViewDetail shows everything about the highlighted entry, since the table has
// to truncate the query and cannot show an error at all.
func (v LogsView) ViewDetail(width int) string {
	lines := []string{labelStyle.Render("ENTRY")}
	if v.cursor >= len(v.entries) {
		return strings.Join(append(lines, "", mutedStyle.Render("select an entry above")), "\n")
	}

	e := v.entries[v.cursor]
	lines = append(lines, "",
		row("time", brightStyle.Render(e.Timestamp)),
		row("message", brightStyle.Render(e.MessageType)),
		row("response", brightStyle.Render(e.ResponseType)),
		row("conn", brightStyle.Render(e.ConnectionID)),
		row("query id", brightStyle.Render(e.ClientQuery)),
		row("duration", brightStyle.Render(e.DurationMs+" ms")),
	)
	if e.Query != "" {
		lines = append(lines, "", labelStyle.Render("SQL"), "",
			renderCodeBlock(e.Query, width-4))
	}
	if e.Err != "" {
		lines = append(lines, "", redStyle.Render("error"), "",
			redStyle.Render(truncate(e.Err, (width-4)*3)))
	}
	return strings.Join(lines, "\n")
}

func (v LogsView) ViewFooter() string {
	if !v.HasTarget() {
		return footerStyle.Render(keyBadge("esc") + " back to dashboard")
	}
	keys := []string{
		keyBadge("↑↓") + " select",
		keyBadge("r") + " refresh",
		keyBadge("e") + " enable logging",
	}
	if len(v.quackIdxs) > 1 {
		keys = append(keys, keyBadge("tab")+" cycle server")
	}
	keys = append(keys, keyBadge("esc")+" back")
	return footerStyle.Render(strings.Join(keys, "   "))
}

// clockOf keeps just the time part of a log timestamp, which is all that fits
// and all that is useful when every row is from the same session.
func clockOf(ts string) string {
	if i := strings.Index(ts, " "); i >= 0 && len(ts) > i+1 {
		return ts[i+1:]
	}
	return ts
}
