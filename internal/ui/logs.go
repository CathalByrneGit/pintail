package ui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/CathalByrneGit/pintail/internal/quack"
)

// ── types ─────────────────────────────────────────────────────────────────

type logsResultMsg struct {
	idx     int
	entries []quack.LogEntry
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
	clients []*quack.QuackClient
	// quackIdxs are the indices of connections whose backend has a Quack log.
	quackIdxs []int
	targetPos int

	entries []quack.LogEntry
	cursor  int
	loading bool
	errMsg  string

	// notice reports the outcome of enabling logging, which is a change to
	// server state and so is never done implicitly by a poll.
	notice    string
	noticeErr bool
}

// ── constructor ───────────────────────────────────────────────────────────

func NewLogsView(clients []*quack.QuackClient) LogsView {
	v := LogsView{clients: clients}
	for i, c := range clients {
		// The log is a Quack-server thing: a local file has no message log, and
		// a DuckLake catalog is reached without the protocol.
		if c.Config.Type == quack.ConnQuack {
			v.quackIdxs = append(v.quackIdxs, i)
		}
	}
	return v
}

// HasTarget reports whether any connection can have a Quack log.
func (v LogsView) HasTarget() bool { return len(v.quackIdxs) > 0 }

// TargetClient returns the currently-selected Quack client, or nil.
func (v LogsView) TargetClient() *quack.QuackClient {
	if !v.HasTarget() {
		return nil
	}
	return v.clients[v.quackIdxs[v.targetPos]]
}

// ── commands ──────────────────────────────────────────────────────────────

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
		entries, err := c.Logs(ctx)
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
		if err := c.EnableLogging(ctx); err != nil {
			return logsEnabledMsg{target: c.Config.Name, err: firstLine(err.Error())}
		}
		return logsEnabledMsg{target: c.Config.Name}
	}
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
