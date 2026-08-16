package ui

import "github.com/charmbracelet/lipgloss"

// ── colour palette ────────────────────────────────────────────────────────

var (
	colorDuckYellow  = lipgloss.Color("#F5A623")
	colorGreen       = lipgloss.Color("#2ECC71")
	colorAmber       = lipgloss.Color("#F39C12")
	colorRed         = lipgloss.Color("#E74C3C")
	colorMuted       = lipgloss.Color("#6C7A89")
	colorBrightWhite = lipgloss.Color("#ECF0F1")
	colorPanelBorder = lipgloss.Color("#2C3E50")
	colorDarkBg      = lipgloss.Color("#0D1117")
)

// ── text styles ───────────────────────────────────────────────────────────

var (
	titleStyle  = lipgloss.NewStyle().Foreground(colorDuckYellow).Bold(true)
	labelStyle  = lipgloss.NewStyle().Foreground(colorDuckYellow).Bold(true)
	mutedStyle  = lipgloss.NewStyle().Foreground(colorMuted)
	brightStyle = lipgloss.NewStyle().Foreground(colorBrightWhite)
	greenStyle  = lipgloss.NewStyle().Foreground(colorGreen).Bold(true)
	amberStyle  = lipgloss.NewStyle().Foreground(colorAmber).Bold(true)
	redStyle    = lipgloss.NewStyle().Foreground(colorRed).Bold(true)
)

// ── layout styles ─────────────────────────────────────────────────────────

var (
	headerBarStyle = lipgloss.NewStyle().
			Background(colorDarkBg).
			Foreground(colorBrightWhite).
			Padding(0, 1)

	panelStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(colorPanelBorder).
			PaddingLeft(1).PaddingRight(1)

	activePanelStyle = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(colorDuckYellow).
				PaddingLeft(1).PaddingRight(1)

	footerStyle = lipgloss.NewStyle().
			Foreground(colorMuted).
			PaddingLeft(1)
)

// keyBadge renders a keyboard shortcut as a dark-on-yellow pill.
func keyBadge(k string) string {
	return lipgloss.NewStyle().
		Foreground(colorDarkBg).
		Background(colorDuckYellow).
		Padding(0, 1).
		Render(k)
}
