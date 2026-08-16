package ui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/CathalByrneGit/pintail/internal/quack"
)

// ── types ─────────────────────────────────────────────────────────────────

// Token represents a scoped Quack authentication credential. Persisted under
// "tokens" in ~/.duckdb/pintail.json, alongside the storage secrets and with
// the same plaintext caveat.
type tmMode int

const (
	tmModeTokens tmMode = iota
	tmModeSecrets
)

type TokenManager struct {
	mode tmMode

	// — Quack token state —
	tokens    []quack.Token
	cursor    int
	showValue bool // whether the selected token's value is revealed

	form          *tokenForm
	rotateConfirm bool
	revokeConfirm bool

	// — Storage secret state (parallel to the token state above) —
	secrets          []quack.StorageSecret
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
		tokens:  quack.LoadTokens(),
		secrets: quack.LoadStorageSecrets(),
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
		tm.tokens = append(tm.tokens, quack.BuildToken(
			f.fields[0].Value(),
			f.fields[1].Value(),
			f.fields[2].Value(),
			f.fields[3].Value(),
		))
		tm.cursor = len(tm.tokens) - 1
		tm.form = nil
		tm.persist("Token created")
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
				tm.tokens[i].Value = quack.GenerateTokenValue()
				tm.tokens[i].CreatedAt = time.Now()
				tm.showValue = true // reveal the new value immediately
			}
		}
		tm.rotateConfirm = false
		tm.persist("Token rotated — new value shown below")
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
		tm.persist("Token revoked")
	case "n", "N", "esc":
		tm.revokeConfirm = false
	}
	return tm, nil
}

// ── View helpers ──────────────────────────────────────────────────────────

func (tm TokenManager) ViewTokenList(width, height int) string {
	// Tab nav header — same pattern as ViewSecretList so both modes look
	// consistent and the user can see they're toggleable.
	header := lipgloss.JoinHorizontal(lipgloss.Top,
		lipgloss.NewStyle().
			Background(colorDuckYellow).Foreground(colorDarkBg).Bold(true).
			Padding(0, 1).Render("Quack tokens"),
		mutedStyle.Render("   Storage secrets  "),
	)

	var lines []string
	lines = append(lines, header, "")
	lines = append(lines, labelStyle.Render("  TOKENS  ")+
		mutedStyle.Render(fmt.Sprintf("(%d total)", len(tm.tokens))))
	lines = append(lines, "")

	if len(tm.tokens) == 0 {
		lines = append(lines,
			mutedStyle.Render("  ◌ no tokens yet"),
			"",
			mutedStyle.Render("  press [n] to create one"),
			"",
			mutedStyle.Render("  tokens are bearer credentials a Quack server"),
			mutedStyle.Render("  accepts on inbound connections — once created"),
			mutedStyle.Render("  here, export to SQL with [e] and run on the"),
			mutedStyle.Render("  server."),
		)
		return strings.Join(lines, "\n")
	}

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

		// Room left for the scope line after the cursor, dot and padding.
		// On a narrow terminal this goes negative — truncate handles that by
		// dropping the scope rather than slicing past the start of the string.
		scopeStr := truncate(strings.Join(t.Scope, ", "), width-18)

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
	lines = append(lines, mutedStyle.Render(hrule(width-6)))
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

	// Generated SQL block — the value is elided here unless [v] is toggled,
	// matching the masking of the Token row above.
	sql := tokenSQL(*t, tm.showValue)
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

func (tm *TokenManager) selectedToken() *quack.Token {
	if len(tm.tokens) == 0 || tm.cursor >= len(tm.tokens) {
		return nil
	}
	return &tm.tokens[tm.cursor]
}

// tokenSQL renders the CREATE SECRET statement for a token.
//
// `full` decides whether the token value is written verbatim. Exports must be
// runnable, so they pass true; the on-screen block passes the reveal state, so
// the value stays elided until the operator asks for it with [v]. Rendering
// the elided form into the export file made every exported credential
// unusable — the reason this parameter exists rather than being a constant.
func tokenSQL(t quack.Token, full bool) string {
	scope := strings.Join(t.Scope, ", ")
	val := t.Value
	if !full {
		val = truncate(val, 16)
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

func exportTokenSQL(t quack.Token) (string, error) {
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
	fmt.Fprintln(f, tokenSQL(t, true))
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

// persist writes the token list to the config file and turns any failure into
// the status message, so a save that did not happen is visible rather than
// assumed. Tokens are credentials: losing one silently is the worst outcome
// here, which is why every mutating action calls this.
func (tm *TokenManager) persist(okMsg string) {
	if err := quack.SaveTokens(tm.tokens); err != nil {
		tm.successMsg = "save failed: " + err.Error()
		tm.successTTL = 8
		return
	}
	tm.successMsg = okMsg
	tm.successTTL = 4
}
