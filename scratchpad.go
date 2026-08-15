package main

import (
	"context"
	"encoding/csv"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// ── types ─────────────────────────────────────────────────────────────────

// Scratchpad holds all state for the interactive SQL editor view.
type Scratchpad struct {
	editor    textarea.Model
	resultsVP viewport.Model

	result     *QueryResult
	history    []HistoryEntry
	historyIdx int // -1 = not browsing; ≥0 = index into history

	servers   []ServerInfo
	serverIdx int

	clients []*QuackClient // may be nil entries or nil slice when unconfigured
	running bool           // true while an async query is in flight

	// cancelQuery aborts the in-flight query, killing the duckdb subprocess
	// with it. Without this the only way out of a slow query was to kill the
	// whole app, since ctrl+c quits.
	cancelQuery context.CancelFunc

	// Export prompt state: when true, the next key is treated as a format
	// selection (c=CSV, p=Parquet, anything else cancels).
	exportPrompt bool
	// One-shot status message after an export attempt; clears on next action.
	exportMsg string

	width  int
	height int
}

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

// HistoryEntry records a completed query.
type HistoryEntry struct {
	Query     string
	RowCount  int
	ElapsedMs int
	Timestamp time.Time
}

// ── constructor ───────────────────────────────────────────────────────────

func NewScratchpad(servers []ServerInfo, clients []*QuackClient) Scratchpad {
	ta := textarea.New()
	ta.Placeholder = "-- write SQL here and press ctrl+r to run\nSELECT * FROM analytics.orders LIMIT 10;"
	ta.ShowLineNumbers = true
	ta.Focus()
	ta.CharLimit = 0
	ta.SetWidth(80)
	ta.SetHeight(7)

	// Style overrides
	ta.FocusedStyle.Base = lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(colorDuckYellow)
	ta.FocusedStyle.LineNumber = lipgloss.NewStyle().Foreground(colorMuted)
	ta.FocusedStyle.Text = lipgloss.NewStyle().Foreground(colorBrightWhite)
	ta.FocusedStyle.Placeholder = lipgloss.NewStyle().Foreground(colorMuted).Italic(true)
	ta.BlurredStyle.Base = lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(colorPanelBorder)

	vp := viewport.New(80, 10)
	vp.Style = lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(colorPanelBorder).
		PaddingLeft(1).PaddingRight(1)

	return Scratchpad{
		editor:     ta,
		resultsVP:  vp,
		servers:    servers,
		clients:    clients,
		serverIdx:  0,
		historyIdx: -1,
	}
}

// SetTargets replaces the list of connections the scratchpad can target,
// keeping serverIdx inside the new bounds. Deleting connections in the
// connection manager used to leave the index dangling past the end of the
// shorter slice, panicking on the next render of this screen.
func (sp *Scratchpad) SetTargets(servers []ServerInfo, clients []*QuackClient) {
	sp.servers = servers
	sp.clients = clients
	if sp.serverIdx >= len(servers) {
		sp.serverIdx = len(servers) - 1
	}
	if sp.serverIdx < 0 {
		sp.serverIdx = 0
	}
}

// target returns the currently-selected server. The second return is false
// when there is nothing to target — no connections configured, or an index
// that outlived the slice it pointed into.
func (sp Scratchpad) target() (ServerInfo, bool) {
	if sp.serverIdx < 0 || sp.serverIdx >= len(sp.servers) {
		return ServerInfo{}, false
	}
	return sp.servers[sp.serverIdx], true
}

// Resize updates internal component sizes to match the terminal.
func (sp *Scratchpad) Resize(w, h int) {
	sp.width = w
	sp.height = h

	editorW := w - 4
	sp.editor.SetWidth(editorW)

	// header≈3, editor≈9(+border), statusBar≈1, footer≈2, borders≈4
	resultsH := h - 3 - 11 - 1 - 2 - 4
	if resultsH < 3 {
		resultsH = 3
	}
	sp.resultsVP.Width = w - 4
	sp.resultsVP.Height = resultsH
}

// ── Update ────────────────────────────────────────────────────────────────

func (sp Scratchpad) Update(msg tea.Msg) (Scratchpad, tea.Cmd) {
	switch msg := msg.(type) {

	// ── async query result ────────────────────────────────────────────────
	case queryResultMsg:
		sp.running = false
		sp.cancelQuery = nil
		sp.result = msg.result
		if sp.result != nil {
			sp.resultsVP.SetContent(renderResultTable(*sp.result, sp.resultsVP.Width-4))
			sp.resultsVP.GotoTop()
			// Record to history on success
			if sp.result.Err == "" {
				q := sp.result.Query
				if len(sp.history) == 0 || sp.history[len(sp.history)-1].Query != q {
					sp.history = append(sp.history, HistoryEntry{
						Query:     q,
						RowCount:  len(sp.result.Rows),
						ElapsedMs: sp.result.ElapsedMs,
						Timestamp: sp.result.Timestamp,
					})
					if len(sp.history) > 50 {
						sp.history = sp.history[1:]
					}
				}
			}
		}
		return sp, nil

	case tea.KeyMsg:
		// Export format prompt is open — next key chooses CSV/Parquet/cancel
		if sp.exportPrompt {
			sp.exportPrompt = false
			switch msg.String() {
			case "c", "C":
				if sp.result != nil {
					if path, err := exportCSV(*sp.result); err == nil {
						sp.exportMsg = "✓ exported CSV → " + path
					} else {
						sp.exportMsg = "✕ CSV export failed: " + err.Error()
					}
				}
			case "p", "P":
				if sp.result != nil && len(sp.clients) > sp.serverIdx {
					c := sp.clients[sp.serverIdx]
					if path, err := exportParquet(c, sp.result.Query); err == nil {
						sp.exportMsg = "✓ exported Parquet → " + path
					} else {
						sp.exportMsg = "✕ Parquet export failed: " + err.Error()
					}
				}
			}
			return sp, nil
		}

		switch msg.String() {

		case "ctrl+r":
			if sp.running {
				return sp, nil
			}
			sp.exportMsg = ""
			return sp.runQuery()

		// Interrupt an in-flight query. ctrl+c is the psql convention and is
		// routed here by the root model while a query is running; esc does the
		// same so there is a way out that doesn't risk quitting the app.
		case "ctrl+c", "esc":
			return sp.cancel(), nil

		case "ctrl+e":
			if sp.result != nil && !sp.result.IsEmpty() {
				sp.exportPrompt = true
			}
			return sp, nil

		case "ctrl+p":
			return sp.historyPrev(), nil

		case "ctrl+n":
			return sp.historyNext(), nil

		case "ctrl+l":
			sp.result = nil
			sp.resultsVP.SetContent("")
			return sp, nil

		case "tab":
			if len(sp.servers) > 1 {
				sp.serverIdx = (sp.serverIdx + 1) % len(sp.servers)
			}
			return sp, nil

		case "pgup", "ctrl+b":
			// Call viewport's page-up method directly. The bubbles viewport
			// default keymap doesn't bind ctrl+b/ctrl+f, so forwarding the
			// raw msg to viewport.Update was silently doing nothing for them.
			sp.resultsVP.ViewUp()
			return sp, nil

		case "pgdown", "ctrl+f":
			sp.resultsVP.ViewDown()
			return sp, nil

		case "ctrl+u":
			// half-page up (vim/less convention)
			sp.resultsVP.HalfViewUp()
			return sp, nil

		case "ctrl+d":
			// half-page down
			sp.resultsVP.HalfViewDown()
			return sp, nil

		case "alt+up":
			// single-line scroll up — works on every keyboard, no Fn dance
			sp.resultsVP.LineUp(1)
			return sp, nil

		case "alt+down":
			sp.resultsVP.LineDown(1)
			return sp, nil

		case "home":
			sp.resultsVP.GotoTop()
			return sp, nil

		case "end":
			sp.resultsVP.GotoBottom()
			return sp, nil

		default:
			var cmd tea.Cmd
			sp.editor, cmd = sp.editor.Update(msg)
			sp.historyIdx = -1
			return sp, cmd
		}

	default:
		var cmd tea.Cmd
		sp.editor, cmd = sp.editor.Update(msg)
		return sp, cmd
	}
}

func (sp Scratchpad) runQuery() (Scratchpad, tea.Cmd) {
	sql := strings.TrimSpace(sp.editor.Value())
	if sql == "" {
		return sp, nil
	}

	if _, ok := sp.target(); !ok {
		return sp, func() tea.Msg {
			return queryResultMsg{result: &QueryResult{
				Query:     sql,
				Err:       "no connection configured — add one from the dashboard with [a]",
				Timestamp: time.Now(),
				Method:    "offline",
			}}
		}
	}

	// Resolve the right client for the selected server
	var client *QuackClient
	if sp.clients != nil && sp.serverIdx < len(sp.clients) {
		client = sp.clients[sp.serverIdx]
	}

	// No client at all — return an offline result rather than fabricating.
	if client == nil {
		return sp, func() tea.Msg {
			r := offlineResult(sql)
			return queryResultMsg{result: &r}
		}
	}

	sp.running = true

	// The cancel func is kept so ctrl+c / esc can abort this query; the
	// deadline still applies on top of it.
	ctx, cancel := context.WithTimeout(context.Background(), QueryTimeout())
	sp.cancelQuery = cancel

	// Async: run in a goroutine, result comes back as queryResultMsg
	return sp, client.QueryAsync(ctx, sql)
}

// Running reports whether a query is in flight, so the root model knows whether
// ctrl+c means "interrupt the query" or "quit the app".
func (sp Scratchpad) Running() bool { return sp.running }

// cancel aborts an in-flight query. The result message still arrives, carrying
// the reason, so the status line updates through the normal path.
func (sp Scratchpad) cancel() Scratchpad {
	if sp.cancelQuery != nil {
		sp.cancelQuery()
		sp.cancelQuery = nil
	}
	return sp
}

func (sp Scratchpad) historyPrev() Scratchpad {
	if len(sp.history) == 0 {
		return sp
	}
	if sp.historyIdx < 0 {
		sp.historyIdx = len(sp.history) - 1
	} else if sp.historyIdx > 0 {
		sp.historyIdx--
	}
	sp.editor.SetValue(sp.history[sp.historyIdx].Query)
	return sp
}

func (sp Scratchpad) historyNext() Scratchpad {
	if sp.historyIdx < 0 {
		return sp
	}
	if sp.historyIdx < len(sp.history)-1 {
		sp.historyIdx++
		sp.editor.SetValue(sp.history[sp.historyIdx].Query)
	} else {
		sp.historyIdx = -1
		sp.editor.SetValue("")
	}
	return sp
}

// ── View helpers ──────────────────────────────────────────────────────────

func (sp Scratchpad) ViewEditor() string {
	srv, ok := sp.target()
	if !ok {
		return lipgloss.JoinVertical(lipgloss.Left,
			"  "+mutedStyle.Render("target  ")+
				redStyle.Render("none")+
				mutedStyle.Render("  add a connection from the dashboard with [a]"),
			"",
			sp.editor.View(),
		)
	}
	// Describe the target as what it actually is. Only a Quack remote has a
	// transport to report; a local file or a lake has neither scheme nor TLS.
	uri := srv.URI
	var badge string
	switch srv.Type {
	case ConnQuack:
		if uri == "" {
			uri = "quack://" + srv.Host + fmt.Sprintf(":%d", srv.Port)
		}
		if srv.TLS {
			badge = greenStyle.Render("● HTTPS")
		} else {
			badge = amberStyle.Render("● HTTP")
		}
	case ConnLocal:
		badge = mutedStyle.Render("● file")
	case ConnDuckLake:
		badge = mutedStyle.Render("● ducklake")
	}

	serverLine := "  " +
		mutedStyle.Render("target  ") +
		labelStyle.Render(srv.Name) +
		mutedStyle.Render("  "+truncate(uri, 60)+"  ") +
		badge

	histLine := ""
	if len(sp.history) > 0 {
		histLine = "   " + mutedStyle.Render(fmt.Sprintf("history %d", len(sp.history)))
		if sp.historyIdx >= 0 {
			histLine += mutedStyle.Render(fmt.Sprintf("  [%d/%d]", sp.historyIdx+1, len(sp.history)))
		}
	}

	return lipgloss.JoinVertical(lipgloss.Left,
		serverLine+histLine,
		"",
		sp.editor.View(),
	)
}

func (sp Scratchpad) ViewResultsStatus() string {
	if sp.exportPrompt {
		return "  " + labelStyle.Render("export as:") +
			"  " + keyBadge("c") + " CSV" +
			"  " + keyBadge("p") + " Parquet" +
			"  " + keyBadge("esc") + " cancel"
	}
	if sp.exportMsg != "" {
		st := greenStyle
		if strings.HasPrefix(sp.exportMsg, "✕") {
			st = redStyle
		}
		return "  " + st.Render(sp.exportMsg)
	}
	if sp.running {
		return "  " + amberStyle.Render("⟳ running…") +
			mutedStyle.Render("  ctrl+c or esc to interrupt  ·  deadline "+QueryTimeout().String())
	}
	if sp.result == nil {
		return mutedStyle.Render("  no results yet — press ctrl+r to run")
	}
	if sp.result.Err != "" {
		return "  " + redStyle.Render("✕ error  ") + brightStyle.Render(sp.result.Err)
	}

	rowWord := "rows"
	if len(sp.result.Rows) == 1 {
		rowWord = "row"
	}

	// Where the result came from. There is no "mock" case any more: the
	// fabricated executor is gone, so a result is either live or an error.
	var badge string
	switch sp.result.Method {
	case "cli":
		badge = greenStyle.Render("● LIVE/cli")
	case "http":
		badge = greenStyle.Render("● LIVE/http")
	default:
		badge = mutedStyle.Render("◌ " + sp.result.Method)
	}

	// When the selected connection can't run queries, say which prerequisite is
	// missing rather than leaving the user to guess.
	var cliHint string
	if sp.clients != nil && sp.serverIdx < len(sp.clients) && sp.clients[sp.serverIdx] != nil {
		c := sp.clients[sp.serverIdx]
		switch {
		case !c.GetState().Online:
			cliHint = mutedStyle.Render("  ·  connection offline")
		case !c.HasCLI() && c.Config.Type != ConnQuack:
			cliHint = mutedStyle.Render("  ·  install the duckdb CLI for live queries")
		}
	}

	elapsed := fmt.Sprintf("%dms", sp.result.ElapsedMs)
	ts := sp.result.Timestamp.Format("15:04:05")

	// Scroll position indicator — shows up only when the result is taller
	// than the viewport (otherwise it's noise).
	var scrollIndicator string
	if sp.resultsVP.TotalLineCount() > sp.resultsVP.Height {
		pct := int(sp.resultsVP.ScrollPercent() * 100)
		scrollIndicator = mutedStyle.Render(fmt.Sprintf("  ·  scroll %d%%", pct))
	}

	return "  " + badge +
		mutedStyle.Render("  ·  ") +
		greenStyle.Render(fmt.Sprintf("%d %s", len(sp.result.Rows), rowWord)) +
		mutedStyle.Render("  ·  "+elapsed+"  ·  "+ts+"  ·  ") +
		mutedStyle.Render(truncate(firstLine(sp.result.Query), 40)) +
		scrollIndicator +
		cliHint
}

func (sp Scratchpad) ViewResults() string {
	if sp.result == nil {
		return sp.viewEmptyResults()
	}
	return sp.resultsVP.View()
}

func (sp Scratchpad) viewEmptyResults() string {
	placeholder := lipgloss.JoinVertical(lipgloss.Center,
		"",
		mutedStyle.Render("─────────────────────────────────────"),
		mutedStyle.Render("  results appear here after running  "),
		"",
		mutedStyle.Render("  example queries:"),
		"",
		amberStyle.Render("  SELECT * FROM analytics.orders LIMIT 10;"),
		amberStyle.Render("  SELECT * FROM analytics.customers LIMIT 20;"),
		amberStyle.Render("  SELECT COUNT(*) FROM raw.logs;"),
		amberStyle.Render("  SHOW TABLES;"),
		amberStyle.Render("  DESCRIBE analytics.orders;"),
		"",
		mutedStyle.Render("─────────────────────────────────────"),
	)

	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(colorPanelBorder).
		PaddingLeft(1).PaddingRight(1).
		Width(sp.width - 4).
		Height(sp.resultsVP.Height).
		Render(placeholder)
}

func (sp Scratchpad) ViewFooter() string {
	keys := strings.Join([]string{
		keyBadge("ctrl+r") + " run",
		keyBadge("ctrl+c") + " interrupt",
		keyBadge("ctrl+p/n") + " history",
		keyBadge("ctrl+e") + " export",
		keyBadge("ctrl+b/f") + " page  " + keyBadge("ctrl+u/d") + " ½page  " + keyBadge("alt+↑↓") + " line",
		keyBadge("ctrl+l") + " clear",
		keyBadge("tab") + " target",
		keyBadge("esc") + " back",
	}, "  ")
	return footerStyle.Render(keys)
}

// ── result table renderer ─────────────────────────────────────────────────

func renderResultTable(r QueryResult, maxWidth int) string {
	if r.Err != "" {
		return redStyle.Render("Error: "+r.Err) + "\n\n" +
			mutedStyle.Render(r.Query)
	}
	if len(r.Columns) == 0 {
		return mutedStyle.Render("(query returned no columns)")
	}
	if len(r.Rows) == 0 {
		return mutedStyle.Render("(0 rows)")
	}

	// Column widths in terminal cells, so wide characters line up.
	widths := make([]int, len(r.Columns))
	for i, c := range r.Columns {
		widths[i] = ansi.StringWidth(c)
	}
	for _, row := range r.Rows {
		for j, cell := range row {
			if j < len(widths) {
				if w := ansi.StringWidth(cell); w > widths[j] {
					widths[j] = w
				}
			}
		}
	}

	// Cap total width: trim trailing columns if necessary, and count how many
	// were dropped. Dropping them silently meant a wide result looked complete
	// while columns were missing off the right-hand edge.
	sep := "  │  "
	sepW := ansi.StringWidth(sep)
	allColumns := len(r.Columns)
	total := sum(widths) + (len(widths)-1)*sepW
	for total > maxWidth && len(widths) > 1 {
		widths = widths[:len(widths)-1]
		r.Columns = r.Columns[:len(r.Columns)-1]
		total = sum(widths) + (len(widths)-1)*sepW
	}
	dropped := allColumns - len(r.Columns)

	var sb strings.Builder

	// Header
	for i, col := range r.Columns {
		sb.WriteString(labelStyle.Render(padRight(col, widths[i])))
		if i < len(r.Columns)-1 {
			sb.WriteString(mutedStyle.Render(sep))
		}
	}
	sb.WriteString("\n")

	// Separator
	for i, w := range widths {
		sb.WriteString(mutedStyle.Render(strings.Repeat("─", w)))
		if i < len(widths)-1 {
			sb.WriteString(mutedStyle.Render("──┼──"))
		}
	}
	sb.WriteString("\n")

	// Rows
	for _, row := range r.Rows {
		for j, w := range widths {
			cell := ""
			if j < len(row) {
				cell = row[j]
			}
			sb.WriteString(brightStyle.Render(padRight(truncate(cell, w), w)))
			if j < len(widths)-1 {
				sb.WriteString(mutedStyle.Render(sep))
			}
		}
		sb.WriteString("\n")
	}

	if dropped > 0 {
		noun := "columns"
		if dropped == 1 {
			noun = "column"
		}
		sb.WriteString("\n")
		sb.WriteString(amberStyle.Render(fmt.Sprintf("+ %d more %s", dropped, noun)))
		sb.WriteString(mutedStyle.Render(" — too wide for this terminal; SELECT fewer columns to see them"))
		sb.WriteString("\n")
	}

	return sb.String()
}

// ── offline result ────────────────────────────────────────────────────────
//
// Earlier versions had a mock executor that returned fake demo rows when no
// real connection was available. That was confusing — the scratchpad returned
// results and none of them were real. What replaced it is this: say the
// connection is offline and point at the README. (It was still called
// mockExecute, and took a ServerInfo it ignored, until the last of the mock
// scaffolding went.)

func offlineResult(query string) QueryResult {
	return QueryResult{
		Query:     query,
		Timestamp: time.Now(),
		Method:    "offline",
		Err:       "no online connection — start a Quack server, point at a .duckdb file, or attach a DuckLake (see README \"Getting started\")",
	}
}

// padRight pads s with spaces to w terminal cells.
//
// Width is measured in cells, not bytes: a CJK character occupies two cells and
// three bytes, and a styled string carries escape sequences that occupy none.
// Measuring bytes made every column containing either one misalign.
func padRight(s string, w int) string {
	gap := w - ansi.StringWidth(s)
	if gap <= 0 {
		return s
	}
	return s + strings.Repeat(" ", gap)
}

// truncate shortens s to at most n terminal cells, marking the cut with an
// ellipsis when there's room for one. n <= 0 means "no room at all" and yields
// the empty string — callers derive n from panel widths, which go negative on
// narrow terminals, and returning the untruncated string there blew the layout
// apart.
//
// Cutting is ANSI- and width-aware: slicing bytes used to emit invalid UTF-8
// for multibyte content and could sever an escape sequence mid-way, leaving the
// rest of the line coloured by whatever the fragment happened to say.
func truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if ansi.StringWidth(s) <= n {
		return s
	}
	if n <= 1 {
		return ansi.Truncate(s, n, "")
	}
	return ansi.Truncate(s, n, "…")
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "\n"); i >= 0 {
		return s[:i]
	}
	return s
}

func sum(ns []int) int {
	t := 0
	for _, n := range ns {
		t += n
	}
	return t
}

// IsEmpty reports whether the result has no data rows worth exporting.
func (r *QueryResult) IsEmpty() bool {
	return r == nil || (len(r.Columns) == 0 && len(r.Rows) == 0) || r.Err != ""
}

// exportCSV writes the in-memory result rows to ~/.duckdb/exports/pintail-{ts}.csv.
// Pure in-process; doesn't require duckdb CLI.
func exportCSV(r QueryResult) (string, error) {
	path, err := exportPath("csv")
	if err != nil {
		return "", err
	}
	f, err := os.Create(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	if err := w.Write(r.Columns); err != nil {
		return "", err
	}
	for _, row := range r.Rows {
		if err := w.Write(row); err != nil {
			return "", err
		}
	}
	w.Flush()
	return path, w.Error()
}

// exportParquet uses the duckdb CLI to re-execute the query and write the
// output as Parquet via COPY (...) TO. This is the easiest way to get a
// proper Parquet file from a Go process without CGo.
func exportParquet(c *QuackClient, sql string) (string, error) {
	if c == nil || !c.HasCLI() {
		return "", fmt.Errorf("duckdb CLI not available")
	}
	if c.GetState().Online == false {
		return "", fmt.Errorf("connection offline")
	}
	path, err := exportPath("parquet")
	if err != nil {
		return "", err
	}

	// Strip trailing semicolons; the inner SQL goes inside COPY (...) TO.
	inner := strings.TrimRight(strings.TrimSpace(sql), ";")
	copySQL := fmt.Sprintf("COPY (%s) TO '%s' (FORMAT PARQUET);", inner, sqlQuote(path))

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, c.cliPath, c.cliArgs(copySQL)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("%s", strings.TrimSpace(string(out)))
	}
	return path, nil
}

// exportPath constructs a timestamped path under ~/.duckdb/exports/ and
// ensures the directory exists.
func exportPath(ext string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "/tmp"
	}
	dir := filepath.Join(home, ".duckdb", "exports")
	if err := os.MkdirAll(dir, 0750); err != nil {
		return "", err
	}
	name := fmt.Sprintf("pintail-%s.%s", time.Now().Format("20060102-150405"), ext)
	return filepath.Join(dir, name), nil
}
