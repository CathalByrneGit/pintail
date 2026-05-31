package main

import (
	"context"
	"encoding/csv"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
			sp.resultsVP, _ = sp.resultsVP.Update(msg)
			return sp, nil

		case "pgdown", "ctrl+f":
			sp.resultsVP, _ = sp.resultsVP.Update(msg)
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

	// No client at all — pure mock, still async for consistent UX
	return sp, func() tea.Msg {
		r := mockExecute(sql, srv)
		r.Method = "mock"
		return queryResultMsg{result: &r, isMock: true}
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

	return "  " + badge +
		mutedStyle.Render("  ·  ") +
		greenStyle.Render(fmt.Sprintf("%d %s", len(sp.result.Rows), rowWord)) +
		mutedStyle.Render("  ·  "+elapsed+"  ·  "+ts+"  ·  ") +
		mutedStyle.Render(truncate(firstLine(sp.result.Query), 40)) +
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
		keyBadge("pgup/dn") + " scroll",
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

// ── mock executor ─────────────────────────────────────────────────────────

func mockExecute(query string, srv ServerInfo) QueryResult {
	r := QueryResult{
		Query:     query,
		Timestamp: time.Now(),
		ElapsedMs: rand.Intn(80) + 4,
	}

	q := strings.ToLower(strings.TrimSpace(query))
	q = strings.TrimRight(q, ";")

	switch {
	case strings.HasPrefix(q, "show tables"):
		r.Columns = []string{"schema", "name", "format", "rows"}
		for _, schema := range mockCatalog {
			for _, tbl := range schema.Tables {
				r.Rows = append(r.Rows, []string{
					schema.Name, tbl.Name, tbl.Format, fmtRows(tbl.Rows),
				})
			}
		}

	case strings.HasPrefix(q, "describe "):
		tableName := strings.TrimPrefix(q, "describe ")
		tableName = strings.TrimPrefix(tableName, "analytics.")
		tableName = strings.TrimPrefix(tableName, "raw.")
		r.Columns = []string{"column_name", "column_type", "null", "key", "default"}
		r.Rows = schemaForTable(tableName)
		if len(r.Rows) == 0 {
			r.Err = fmt.Sprintf("table not found: %s", tableName)
		}

	case strings.Contains(q, "count(*)") || strings.Contains(q, "count( * )"):
		r.Columns = []string{"count(*)"}
		n := rand.Intn(9_000_000) + 100_000
		r.Rows = [][]string{{fmtInt(int64(n))}}
		r.ElapsedMs += rand.Intn(200) // aggregations cost more

	case strings.Contains(q, "orders"):
		limit := extractLimit(q, 10)
		r.Columns = []string{"order_id", "customer_id", "amount", "status", "order_date"}
		r.Rows = mockOrderRows(limit)

	case strings.Contains(q, "customers"):
		limit := extractLimit(q, 10)
		r.Columns = []string{"customer_id", "name", "email", "country", "created_at"}
		r.Rows = mockCustomerRows(limit)

	case strings.Contains(q, "events"):
		limit := extractLimit(q, 10)
		r.Columns = []string{"event_id", "session_id", "event_type", "page", "ts"}
		r.Rows = mockEventRows(limit)

	case strings.Contains(q, "logs"):
		limit := extractLimit(q, 10)
		r.Columns = []string{"ts", "level", "service", "message"}
		r.Rows = mockLogRows(limit)
		r.ElapsedMs += rand.Intn(30) // logs table is huge

	case strings.Contains(q, "metrics"):
		limit := extractLimit(q, 10)
		r.Columns = []string{"ts", "metric", "value", "host", "env"}
		r.Rows = mockMetricRows(limit)

	case q == "select 1" || q == "select 1 as test":
		r.Columns = []string{"1"}
		r.Rows = [][]string{{"1"}}
		r.ElapsedMs = 1

	case strings.Contains(q, "quack_query"):
		r.Err = "quack_query() is available via a live Quack server — connect a real server to execute"

	default:
		// Generic: pretend we ran something
		if strings.HasPrefix(q, "select") {
			r.Columns = []string{"result"}
			r.Rows = [][]string{{"(mock executor — no table matched query pattern)"}}
		} else if strings.HasPrefix(q, "insert") || strings.HasPrefix(q, "update") || strings.HasPrefix(q, "delete") {
			r.Columns = []string{"rows_affected"}
			n := rand.Intn(500) + 1
			r.Rows = [][]string{{strconv.Itoa(n)}}
		} else {
			r.Err = "unrecognized statement (mock executor supports SELECT, SHOW TABLES, DESCRIBE)"
		}
	}

	return r
}

// ── mock row generators ───────────────────────────────────────────────────

var orderStatuses = []string{"shipped", "pending", "processing", "delivered", "cancelled"}
var countries = []string{"US", "UK", "DE", "FR", "CA", "AU", "JP", "BR"}
var eventTypes = []string{"page_view", "click", "form_submit", "purchase", "scroll", "search"}
var logLevels = []string{"INFO", "INFO", "INFO", "WARN", "ERROR", "DEBUG"}
var services = []string{"api-server", "auth-service", "data-ingest", "query-engine", "catalog-svc"}

func mockOrderRows(n int) [][]string {
	base := 10_000 + rand.Intn(5000)
	rows := make([][]string, n)
	for i := range rows {
		rows[i] = []string{
			fmt.Sprintf("%d", base+i),
			fmt.Sprintf("c_%04d", rand.Intn(9999)),
			fmt.Sprintf("%.2f", float64(rand.Intn(49900)+100)/100),
			orderStatuses[rand.Intn(len(orderStatuses))],
			randDate(90),
		}
	}
	return rows
}

func mockCustomerRows(n int) [][]string {
	firstNames := []string{"Alice", "Bob", "Carol", "Dan", "Eve", "Frank", "Grace", "Hiro"}
	lastNames := []string{"Johnson", "Smith", "Patel", "Wang", "Müller", "Tanaka", "Okafor"}
	rows := make([][]string, n)
	for i := range rows {
		fn := firstNames[rand.Intn(len(firstNames))]
		ln := lastNames[rand.Intn(len(lastNames))]
		email := strings.ToLower(fn) + "@example.com"
		rows[i] = []string{
			fmt.Sprintf("c_%04d", rand.Intn(9999)),
			fn + " " + ln,
			email,
			countries[rand.Intn(len(countries))],
			randDate(730),
		}
	}
	return rows
}

func mockEventRows(n int) [][]string {
	pages := []string{"/home", "/products", "/cart", "/checkout", "/account", "/search"}
	rows := make([][]string, n)
	for i := range rows {
		rows[i] = []string{
			fmt.Sprintf("e_%06d", rand.Intn(999999)),
			fmt.Sprintf("s_%s", randHex(6)),
			eventTypes[rand.Intn(len(eventTypes))],
			pages[rand.Intn(len(pages))],
			randTimestamp(7),
		}
	}
	return rows
}

func mockLogRows(n int) [][]string {
	msgs := []string{
		"Request processed in %dms",
		"Cache miss for key %s",
		"Query plan selected: %s",
		"Connection pool exhausted",
		"Retrying after transient error",
		"Health check passed",
	}
	rows := make([][]string, n)
	for i := range rows {
		tmpl := msgs[rand.Intn(len(msgs))]
		var msg string
		switch {
		case strings.Contains(tmpl, "%d"):
			msg = fmt.Sprintf(tmpl, rand.Intn(500)+1)
		case strings.Contains(tmpl, "%s"):
			msg = fmt.Sprintf(tmpl, randHex(4))
		default:
			msg = tmpl
		}
		rows[i] = []string{
			randTimestamp(1),
			logLevels[rand.Intn(len(logLevels))],
			services[rand.Intn(len(services))],
			msg,
		}
	}
	return rows
}

func mockMetricRows(n int) [][]string {
	metrics := []string{"cpu_usage", "mem_bytes", "req_latency_ms", "error_rate", "qps"}
	envs := []string{"prod", "staging"}
	rows := make([][]string, n)
	for i := range rows {
		rows[i] = []string{
			randTimestamp(1),
			metrics[rand.Intn(len(metrics))],
			fmt.Sprintf("%.4f", rand.Float64()*100),
			fmt.Sprintf("node-%02d", rand.Intn(12)+1),
			envs[rand.Intn(len(envs))],
		}
	}
	return rows
}

func schemaForTable(name string) [][]string {
	schemas := map[string][][]string{
		"orders": {
			{"order_id", "INTEGER", "NO", "PRI", ""},
			{"customer_id", "VARCHAR", "NO", "FK", ""},
			{"amount", "DECIMAL(10,2)", "NO", "", ""},
			{"status", "VARCHAR", "YES", "", "pending"},
			{"order_date", "DATE", "NO", "", ""},
		},
		"customers": {
			{"customer_id", "VARCHAR", "NO", "PRI", ""},
			{"name", "VARCHAR", "NO", "", ""},
			{"email", "VARCHAR", "YES", "", ""},
			{"country", "VARCHAR", "YES", "", ""},
			{"created_at", "TIMESTAMP", "NO", "", "now()"},
		},
		"events": {
			{"event_id", "VARCHAR", "NO", "PRI", ""},
			{"session_id", "VARCHAR", "NO", "", ""},
			{"event_type", "VARCHAR", "NO", "", ""},
			{"page", "VARCHAR", "YES", "", ""},
			{"ts", "TIMESTAMP", "NO", "", ""},
		},
		"logs": {
			{"ts", "TIMESTAMP", "NO", "", ""},
			{"level", "VARCHAR", "NO", "", "INFO"},
			{"service", "VARCHAR", "NO", "", ""},
			{"message", "TEXT", "YES", "", ""},
		},
		"metrics": {
			{"ts", "TIMESTAMP", "NO", "", ""},
			{"metric", "VARCHAR", "NO", "", ""},
			{"value", "DOUBLE", "NO", "", ""},
			{"host", "VARCHAR", "NO", "", ""},
			{"env", "VARCHAR", "NO", "", "prod"},
		},
	}
	return schemas[name]
}

// ── util ──────────────────────────────────────────────────────────────────

func extractLimit(q string, def int) int {
	idx := strings.Index(q, "limit ")
	if idx < 0 {
		return def
	}
	var n int
	if _, err := fmt.Sscanf(strings.TrimSpace(q[idx+6:]), "%d", &n); err == nil && n > 0 {
		if n > 500 {
			n = 500
		}
		return n
	}
	return def
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

func fmtInt(n int64) string {
	s := strconv.FormatInt(n, 10)
	var out []byte
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, byte(c))
	}
	return string(out)
}

func randDate(daysBack int) string {
	d := time.Now().AddDate(0, 0, -rand.Intn(daysBack))
	return d.Format("2006-01-02")
}

func randTimestamp(daysBack int) string {
	d := time.Now().Add(-time.Duration(rand.Intn(daysBack*86400)) * time.Second)
	return d.Format("2006-01-02 15:04:05")
}

func randHex(n int) string {
	const chars = "0123456789abcdef"
	out := make([]byte, n)
	for i := range out {
		out[i] = chars[rand.Intn(len(chars))]
	}
	return string(out)
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
