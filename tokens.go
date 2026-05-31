package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// ── types ─────────────────────────────────────────────────────────────────

// Token represents a scoped Quack authentication credential.
type Token struct {
	ID          string
	Name        string
	Value       string   // full token value; never logged
	Scope       []string // catalogs this token can access ("*" = global)
	Permissions []string // SQL operations allowed
	CreatedAt   time.Time
	ExpiresAt   *time.Time // nil = never
	LastUsed    time.Time
	Active      bool
}

// TokenManager holds all state for the token management view.
// tmMode toggles the token manager between the two kinds of secrets it
// manages: Quack auth tokens and storage credentials.
type tmMode int

const (
	tmModeTokens tmMode = iota
	tmModeSecrets
)

type TokenManager struct {
	mode tmMode

	// — Quack token state —
	tokens    []Token
	cursor    int
	showValue bool // whether the selected token's value is revealed

	form          *tokenForm
	rotateConfirm bool
	revokeConfirm bool

	// — Storage secret state (parallel to the token state above) —
	secrets          []StorageSecret
	secretCursor     int
	showSecret       bool // whether the selected secret's key material is revealed
	secretForm       *secretForm
	secretDelConfirm bool

	// — shared —
	successMsg   string
	successTTL   int    // ticks until message clears
	exportedPath string // last exported file path
}

// tokenForm is the inline new-token creation form.
type tokenForm struct {
	fields   []textinput.Model
	focusIdx int
}

// ── constructors ──────────────────────────────────────────────────────────

func NewTokenManager() TokenManager {
	return TokenManager{
		tokens:  mockTokens(),
		secrets: LoadStorageSecrets(),
	}
}

func newTokenForm() *tokenForm {
	labels := []string{"Name", "Scope", "Permissions", "Expires"}
	placeholders := []string{
		"e.g. etl_pipeline_prod",
		"analytics, raw  (or * for global)",
		"SELECT, INSERT  (or * for all)",
		"never  (or YYYY-MM-DD)",
	}
	defaults := []string{"", "", "SELECT", "never"}

	fields := make([]textinput.Model, len(labels))
	for i := range fields {
		ti := textinput.New()
		ti.Placeholder = placeholders[i]
		ti.SetValue(defaults[i])
		ti.CharLimit = 80
		ti.PromptStyle = labelStyle
		ti.TextStyle = brightStyle
		ti.Prompt = labels[i] + "  "
		fields[i] = ti
	}
	fields[0].Focus()

	return &tokenForm{fields: fields, focusIdx: 0}
}

// ── Update ────────────────────────────────────────────────────────────────

func (tm TokenManager) Update(msg tea.Msg) (TokenManager, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.KeyMsg:
		// Secrets-mode dispatch (its own list, form, dialogs).
		// Skip the toggle when any form/dialog is open in either mode.
		anyModalOpen := tm.form != nil || tm.secretForm != nil ||
			tm.rotateConfirm || tm.revokeConfirm || tm.secretDelConfirm
		if msg.String() == "tab" && !anyModalOpen {
			if tm.mode == tmModeTokens {
				tm.mode = tmModeSecrets
			} else {
				tm.mode = tmModeTokens
			}
			tm.exportedPath = ""
			tm.successMsg = ""
			return tm, nil
		}
		if tm.mode == tmModeSecrets {
			return tm.updateSecretsMode(msg)
		}

		// Form is open — route all keys there
		if tm.form != nil {
			return tm.updateForm(msg)
		}
		// Rotate confirmation dialog
		if tm.rotateConfirm {
			return tm.updateRotateConfirm(msg)
		}
		// Revoke confirmation dialog
		if tm.revokeConfirm {
			return tm.updateRevokeConfirm(msg)
		}
		// Normal navigation
		switch msg.String() {
		case "up", "k":
			if tm.cursor > 0 {
				tm.cursor--
				tm.showValue = false
			}
		case "down", "j":
			if tm.cursor < len(tm.tokens)-1 {
				tm.cursor++
				tm.showValue = false
			}
		case "n":
			tm.form = newTokenForm()
		case "r":
			if sel := tm.selectedToken(); sel != nil && sel.Active {
				tm.rotateConfirm = true
			}
		case "d":
			if sel := tm.selectedToken(); sel != nil && sel.Active {
				tm.revokeConfirm = true
			}
		case "v":
			tm.showValue = !tm.showValue
		case "e":
			if sel := tm.selectedToken(); sel != nil {
				path, err := exportTokenSQL(*sel)
				if err == nil {
					tm.exportedPath = path
					tm.successMsg = "Exported → " + path
					tm.successTTL = 5
				} else {
					tm.successMsg = "Export failed: " + err.Error()
					tm.successTTL = 4
				}
			}
		}

	case tickMsg:
		if tm.successTTL > 0 {
			tm.successTTL--
			if tm.successTTL == 0 {
				tm.successMsg = ""
			}
		}
	}

	return tm, nil
}

func (tm TokenManager) updateForm(msg tea.KeyMsg) (TokenManager, tea.Cmd) {
	switch msg.String() {
	case "esc":
		tm.form = nil
		return tm, nil

	case "tab", "shift+tab", "down", "up":
		f := tm.form
		// Blur current, advance focus
		f.fields[f.focusIdx].Blur()
		if msg.String() == "tab" || msg.String() == "down" {
			f.focusIdx = (f.focusIdx + 1) % len(f.fields)
		} else {
			f.focusIdx = (f.focusIdx - 1 + len(f.fields)) % len(f.fields)
		}
		cmd := f.fields[f.focusIdx].Focus()
		return tm, cmd

	case "enter":
		f := tm.form
		if f.focusIdx < len(f.fields)-1 {
			// Advance to next field
			f.fields[f.focusIdx].Blur()
			f.focusIdx++
			cmd := f.fields[f.focusIdx].Focus()
			return tm, cmd
		}
		// Last field → commit new token
		tm.tokens = append(tm.tokens, buildToken(
			f.fields[0].Value(),
			f.fields[1].Value(),
			f.fields[2].Value(),
			f.fields[3].Value(),
		))
		tm.cursor = len(tm.tokens) - 1
		tm.form = nil
		tm.successMsg = "Token created"
		tm.successTTL = 4
		return tm, nil
	}

	// Forward keypress to focused input
	var cmd tea.Cmd
	f := tm.form
	f.fields[f.focusIdx], cmd = f.fields[f.focusIdx].Update(msg)
	return tm, cmd
}

func (tm TokenManager) updateRotateConfirm(msg tea.KeyMsg) (TokenManager, tea.Cmd) {
	switch msg.String() {
	case "y", "Y":
		for i := range tm.tokens {
			if i == tm.cursor {
				tm.tokens[i].Value = generateTokenValue()
				tm.tokens[i].CreatedAt = time.Now()
				tm.showValue = true // reveal the new value immediately
			}
		}
		tm.rotateConfirm = false
		tm.successMsg = "Token rotated — new value shown below"
		tm.successTTL = 5
	case "n", "N", "esc":
		tm.rotateConfirm = false
	}
	return tm, nil
}

func (tm TokenManager) updateRevokeConfirm(msg tea.KeyMsg) (TokenManager, tea.Cmd) {
	switch msg.String() {
	case "y", "Y":
		for i := range tm.tokens {
			if i == tm.cursor {
				tm.tokens[i].Active = false
			}
		}
		tm.revokeConfirm = false
		tm.successMsg = "Token revoked"
		tm.successTTL = 4
	case "n", "N", "esc":
		tm.revokeConfirm = false
	}
	return tm, nil
}

// ── View helpers ──────────────────────────────────────────────────────────

func (tm TokenManager) ViewTokenList(width, height int) string {
	var lines []string
	lines = append(lines, labelStyle.Render("TOKENS"), "")

	for i, t := range tm.tokens {
		cursor := "  "
		style := mutedStyle
		dot := mutedStyle.Render("●")

		if t.Active {
			dot = greenStyle.Render("●")
			style = brightStyle
		} else {
			dot = redStyle.Render("✕")
			style = lipgloss.NewStyle().Foreground(colorRed).Strikethrough(true)
		}
		if i == tm.cursor {
			cursor = amberStyle.Render("▶ ")
			if t.Active {
				style = lipgloss.NewStyle().Foreground(colorDuckYellow).Bold(true)
			}
		}

		scopeStr := strings.Join(t.Scope, ", ")
		if len(scopeStr) > width-18 {
			scopeStr = scopeStr[:width-21] + "…"
		}

		line := cursor + dot + " " + style.Render(t.Name) +
			"\n    " + mutedStyle.Render(scopeStr)
		lines = append(lines, line)
	}

	// Stats
	active := 0
	for _, t := range tm.tokens {
		if t.Active {
			active++
		}
	}
	lines = append(lines, "")
	lines = append(lines, mutedStyle.Render(strings.Repeat("─", width-6)))
	lines = append(lines,
		mutedStyle.Render("active   ")+greenStyle.Render(fmt.Sprintf("%d", active)),
		mutedStyle.Render("revoked  ")+redStyle.Render(fmt.Sprintf("%d", len(tm.tokens)-active)),
	)

	return strings.Join(lines, "\n")
}

func (tm TokenManager) ViewTokenDetail(width int) string {
	t := tm.selectedToken()
	if t == nil {
		return mutedStyle.Render("No token selected")
	}

	var lines []string

	// Name + status badge
	statusBadge := greenStyle.Render(" ACTIVE ")
	if !t.Active {
		statusBadge = redStyle.Render(" REVOKED ")
	}
	lines = append(lines,
		labelStyle.Render("TOKEN DETAIL"),
		"",
		brightStyle.Bold(true).Render(t.Name)+"  "+statusBadge,
		"",
	)

	// Token value (masked or revealed)
	valDisplay := maskToken(t.Value)
	revealHint := mutedStyle.Render("  [v] reveal")
	if tm.showValue {
		valDisplay = lipgloss.NewStyle().Foreground(colorAmber).Render(t.Value)
		revealHint = mutedStyle.Render("  [v] hide")
	}
	lines = append(lines,
		row("Token", valDisplay+revealHint),
		row("Scope", brightStyle.Render(strings.Join(t.Scope, ", "))),
		row("Perms", brightStyle.Render(strings.Join(t.Permissions, ", "))),
		row("Created", mutedStyle.Render(t.CreatedAt.Format("2006-01-02"))),
		row("Expires", mutedStyle.Render(fmtExpiry(t.ExpiresAt))),
		row("Last use", mutedStyle.Render(fmtRelative(t.LastUsed))),
		"",
	)

	// Generated SQL block
	sql := tokenSQL(*t)
	lines = append(lines,
		labelStyle.Render("SQL to apply"),
		renderCodeBlock(sql, width-4),
	)

	return strings.Join(lines, "\n")
}

func (tm TokenManager) ViewForm(width, height int) string {
	f := tm.form
	if f == nil {
		return ""
	}

	innerW := min(width-8, 56)

	var fieldLines []string
	for i, fi := range f.fields {
		indicator := "  "
		if i == f.focusIdx {
			indicator = amberStyle.Render("▶ ")
		}
		fieldLines = append(fieldLines, indicator+fi.View())
		if i < len(f.fields)-1 {
			fieldLines = append(fieldLines, "")
		}
	}

	progress := fmt.Sprintf("  field %d/%d", f.focusIdx+1, len(f.fields))

	hint := mutedStyle.Render("  [tab]/[↑↓] next field  [enter] advance / confirm  [esc] cancel") +
		mutedStyle.Render(progress)

	content := lipgloss.JoinVertical(lipgloss.Left,
		labelStyle.Render("NEW TOKEN"),
		"",
		strings.Join(fieldLines, "\n"),
		"",
		hint,
	)

	return activePanelStyle.Width(innerW).Render(content)
}

func (tm TokenManager) ViewConfirmDialog(action, detail string) string {
	content := lipgloss.JoinVertical(lipgloss.Left,
		amberStyle.Render("⚠  "+strings.ToUpper(action)),
		"",
		brightStyle.Render(detail),
		"",
		keyBadge("y")+" confirm   "+keyBadge("n")+" cancel",
	)
	return activePanelStyle.Width(48).Render(content)
}

func (tm TokenManager) ViewFooter() string {
	var keys []string

	// In secrets mode, the verbs differ slightly (no rotate — credentials come
	// from the cloud provider, not generated by us).
	if tm.mode == tmModeSecrets {
		if tm.secretForm != nil {
			keys = []string{
				keyBadge("tab") + " next field",
				keyBadge("←→") + " cycle type",
				keyBadge("enter") + " save",
				keyBadge("esc") + " cancel",
			}
		} else {
			keys = []string{
				keyBadge("tab") + " ◀ Quack tokens",
				keyBadge("n") + " new",
				keyBadge("d") + " delete",
				keyBadge("v") + " reveal",
				keyBadge("e") + " export",
				keyBadge("esc") + " back",
			}
		}
		line := footerStyle.Render(strings.Join(keys, "   "))
		if tm.successMsg != "" {
			line += "   " + greenStyle.Render("✓ "+tm.successMsg)
		}
		return line
	}

	if tm.form != nil {
		keys = []string{
			keyBadge("tab") + " next field",
			keyBadge("enter") + " confirm",
			keyBadge("esc") + " cancel",
		}
	} else {
		keys = []string{
			keyBadge("tab") + " Storage secrets ▶",
			keyBadge("n") + " new",
			keyBadge("r") + " rotate",
			keyBadge("d") + " revoke",
			keyBadge("v") + " reveal",
			keyBadge("e") + " export",
			keyBadge("esc") + " back",
		}
	}
	line := footerStyle.Render(strings.Join(keys, "   "))
	if tm.successMsg != "" {
		line += "   " + greenStyle.Render("✓ "+tm.successMsg)
	}
	return line
}

// ── helpers ───────────────────────────────────────────────────────────────

func (tm *TokenManager) selectedToken() *Token {
	if len(tm.tokens) == 0 || tm.cursor >= len(tm.tokens) {
		return nil
	}
	return &tm.tokens[tm.cursor]
}

func buildToken(name, scope, perms, expiry string) Token {
	scopeParts := splitTrim(scope)
	permParts := splitTrim(perms)
	if len(scopeParts) == 0 {
		scopeParts = []string{"*"}
	}
	if len(permParts) == 0 {
		permParts = []string{"SELECT"}
	}
	if name == "" {
		name = "token_" + time.Now().Format("0102150405")
	}

	var exp *time.Time
	if expiry != "" && expiry != "never" {
		t, err := time.Parse("2006-01-02", expiry)
		if err == nil {
			exp = &t
		}
	}

	return Token{
		ID:          generateID(),
		Name:        name,
		Value:       generateTokenValue(),
		Scope:       scopeParts,
		Permissions: permParts,
		CreatedAt:   time.Now(),
		ExpiresAt:   exp,
		LastUsed:    time.Time{},
		Active:      true,
	}
}

func generateTokenValue() string {
	b := make([]byte, 24)
	rand.Read(b)
	return "qk_" + hex.EncodeToString(b)
}

func generateID() string {
	b := make([]byte, 4)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func tokenSQL(t Token) string {
	scope := strings.Join(t.Scope, ", ")
	val := t.Value
	if len(val) > 16 {
		val = val[:16] + "…"
	}
	perms := strings.Join(t.Permissions, " | ")
	var sb strings.Builder
	sb.WriteString("CREATE SECRET (\n")
	sb.WriteString("  TYPE quack,\n")
	sb.WriteString(fmt.Sprintf("  TOKEN '%s',\n", val))
	sb.WriteString(fmt.Sprintf("  SCOPE '%s'\n", scope))
	sb.WriteString(");\n")
	sb.WriteString(fmt.Sprintf("-- permissions: %s", perms))
	return sb.String()
}

func exportTokenSQL(t Token) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "/tmp"
	}
	dir := filepath.Join(home, ".duckdb")
	os.MkdirAll(dir, 0700)
	path := filepath.Join(dir, "pintail_tokens.sql")

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return "", err
	}
	defer f.Close()

	fmt.Fprintf(f, "-- Token: %s  exported: %s\n", t.Name, time.Now().Format(time.RFC3339))
	fmt.Fprintln(f, tokenSQL(t))
	fmt.Fprintln(f)
	return path, nil
}

func maskToken(v string) string {
	if len(v) <= 7 {
		return strings.Repeat("•", len(v))
	}
	return v[:7] + strings.Repeat("•", 24)
}

func fmtExpiry(t *time.Time) string {
	if t == nil {
		return "never"
	}
	if time.Now().After(*t) {
		return "EXPIRED " + t.Format("2006-01-02")
	}
	return t.Format("2006-01-02")
}

func fmtRelative(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

func row(label, value string) string {
	pad := 10 - len(label)
	if pad < 1 {
		pad = 1
	}
	return mutedStyle.Render(label+strings.Repeat(" ", pad)) + value
}

func renderCodeBlock(code string, width int) string {
	blockStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(colorPanelBorder).
		Foreground(colorAmber).
		PaddingLeft(1).PaddingRight(1)
	return blockStyle.Width(width).Render(code)
}

func splitTrim(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ── mock data ─────────────────────────────────────────────────────────────

func mockTokens() []Token {
	// Returns nothing by default — users create real tokens via the
	// token manager (press [n] in the tokens screen). See the README
	// "Getting started" section for how to bootstrap a working setup.
	return nil
}
