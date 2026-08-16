package ui

import (
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// ── domain types ──────────────────────────────────────────────────────────

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
