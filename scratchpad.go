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

	clients []*QuackClient // may be nil entries or nil slice for mock-only mode
	running bool           // true while an async query is in flight
	isMock  bool           // true when last result came from the mock executor

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
	Method    string // "cli" | "http" | "mock"
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
		if msg.errStr != "" {
			sp.result = &QueryResult{Err: msg.errStr, Query: "", Timestamp: time.Now(), Method: "error"}
		} else {
			sp.result = msg.result
			sp.isMock = msg.isMock
		}
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
			sp.isMock = false
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

	sp.running = true

	srv := sp.servers[sp.serverIdx]

	// Resolve the right client for the selected server
	var client *QuackClient
	if sp.clients != nil && sp.serverIdx < len(sp.clients) {
		client = sp.clients[sp.serverIdx]
	}

	// Async: run in a goroutine, result comes back as queryResultMsg
	if client != nil {
		return sp, client.QueryAsync(sql, srv)
	}

	// No client at all — return an offline result rather than fabricating.
	return sp, func() tea.Msg {
		r := mockExecute(sql, srv)
		r.Method = "offline"
		return queryResultMsg{result: &r, isMock: false}
	}
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
	srv := sp.servers[sp.serverIdx]
	scheme := "quack://"
	badge := amberStyle.Render("● HTTP")
	if srv.TLS {
		scheme = "quacks://"
		badge = greenStyle.Render("● HTTPS")
	}
	serverLine := "  " +
		mutedStyle.Render("target  ") +
		labelStyle.Render(srv.Name) +
		mutedStyle.Render("  "+scheme+srv.Host+fmt.Sprintf(":%d", srv.Port)+"  ") +
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
			mutedStyle.Render("  query in flight")
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

	// Source badge
	var badge string
	switch sp.result.Method {
	case "cli":
		badge = greenStyle.Render("● LIVE/cli")
	case "http":
		badge = greenStyle.Render("● LIVE/http")
	case "mock":
		badge = amberStyle.Render("◌ MOCK")
	default:
		badge = mutedStyle.Render("◌ mock")
	}

	// CLI availability hint when offline
	var cliHint string
	if sp.isMock {
		if sp.clients != nil && sp.serverIdx < len(sp.clients) && sp.clients[sp.serverIdx] != nil {
			if !sp.clients[sp.serverIdx].HasCLI() {
				cliHint = mutedStyle.Render("  ·  install duckdb CLI for live queries")
			} else {
				cliHint = mutedStyle.Render("  ·  server offline")
			}
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

	// Calculate column widths
	widths := make([]int, len(r.Columns))
	for i, c := range r.Columns {
		widths[i] = len(c)
	}
	for _, row := range r.Rows {
		for j, cell := range row {
			if j < len(widths) && len(cell) > widths[j] {
				widths[j] = len(cell)
			}
		}
	}

	// Cap total width: trim last columns if necessary
	sep := "  │  "
	total := sum(widths) + (len(widths)-1)*len(sep)
	for total > maxWidth && len(widths) > 1 {
		widths = widths[:len(widths)-1]
		r.Columns = r.Columns[:len(r.Columns)-1]
		total = sum(widths) + (len(widths)-1)*len(sep)
	}

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

	return sb.String()
}

// ── offline stub (no mock data) ─────────────────────────────────────────
//
// Earlier versions of Pintail had a fabricated mock executor that returned
// fake demo rows for queries like "SELECT * FROM analytics.orders" when no
// real connection was available. That was confusing — the dashboard looked
// populated, the scratchpad returned results, but none of it was real.
// The honest behaviour is to surface the offline state and direct the user
// at the README "Getting started" section.

func mockExecute(query string, srv ServerInfo) QueryResult {
	return QueryResult{
		Query:     query,
		Timestamp: time.Now(),
		Err:       "no online connection — start a Quack server, point at a .duckdb file, or attach a DuckLake (see README \"Getting started\")",
	}
}

func padRight(s string, w int) string {
	if len(s) >= w {
		return s
	}
	return s + strings.Repeat(" ", w-len(s))
}

func truncate(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	if n <= 3 {
		return s[:n]
	}
	return s[:n-1] + "…"
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
	copySQL := fmt.Sprintf("COPY (%s) TO '%s' (FORMAT PARQUET);", inner, path)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var args []string
	if c.Config.Type == ConnLocal {
		args = []string{c.Config.Path, "-c", copySQL}
	} else {
		args = []string{"-c", c.attachPrefix() + copySQL}
	}
	cmd := exec.CommandContext(ctx, c.cliPath, args...)
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
