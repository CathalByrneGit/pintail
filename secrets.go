package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// ── secret form ───────────────────────────────────────────────────────────
//
// Mirrors the connection-manager form pattern: a type selector at the top
// cycles between s3/r2/gcs/azure, and the field set below adapts.

type secretForm struct {
	sectype  SecretType
	focusIdx int // -1 = type selector focused; ≥0 = visible-field index
	editing  int // -1 = adding new; ≥0 = editing secrets[i]

	name      string
	keyID     string
	secret    string
	region    string
	accountID string
	connStr   string
	scope     string
}

type secField struct {
	label       string
	value       *string
	placeholder string
	hint        string
}

func newSecretForm() *secretForm {
	return &secretForm{sectype: SecretS3, focusIdx: -1, editing: -1}
}

func secretFormFromExisting(s StorageSecret, idx int) *secretForm {
	return &secretForm{
		sectype:   s.Type,
		focusIdx:  0,
		editing:   idx,
		name:      s.Name,
		keyID:     s.KeyID,
		secret:    s.Secret,
		region:    s.Region,
		accountID: s.AccountID,
		connStr:   s.ConnStr,
		scope:     s.Scope,
	}
}

func (f *secretForm) visibleFields() []secField {
	switch f.sectype {
	case SecretR2:
		return []secField{
			{"Name", &f.name, "e.g. lake_r2", "logical name for this secret"},
			{"Key ID", &f.keyID, "r2 access key id", ""},
			{"Secret", &f.secret, "r2 secret access key", "stored verbatim — file perms are your responsibility"},
			{"Account ID", &f.accountID, "cloudflare account id", ""},
			{"Scope", &f.scope, "r2://bucket/path  (optional)", "restricts which paths this secret applies to"},
		}
	case SecretGCS:
		return []secField{
			{"Name", &f.name, "e.g. analytics_gcs", "logical name for this secret"},
			{"Key ID", &f.keyID, "HMAC key", ""},
			{"Secret", &f.secret, "HMAC secret", "stored verbatim — file perms are your responsibility"},
			{"Scope", &f.scope, "gs://bucket  (optional)", "restricts which paths this secret applies to"},
		}
	case SecretAzure:
		return []secField{
			{"Name", &f.name, "e.g. azure_lake", "logical name for this secret"},
			{"Conn string", &f.connStr, "DefaultEndpointsProtocol=https;…", "Azure storage account connection string"},
			{"Scope", &f.scope, "azure://container  (optional)", "restricts which paths this secret applies to"},
		}
	default: // SecretS3
		return []secField{
			{"Name", &f.name, "e.g. lake_s3", "logical name for this secret"},
			{"Key ID", &f.keyID, "AKIA…", "AWS access key id (or compatible)"},
			{"Secret", &f.secret, "secret access key", "stored verbatim — file perms are your responsibility"},
			{"Region", &f.region, "us-east-1  (optional)", ""},
			{"Scope", &f.scope, "s3://bucket/path  (optional)", "restricts which paths this secret applies to"},
		}
	}
}

func (f *secretForm) toSecret() StorageSecret {
	return StorageSecret{
		Name:      strings.TrimSpace(f.name),
		Type:      f.sectype,
		KeyID:     f.keyID,
		Secret:    f.secret,
		Region:    strings.TrimSpace(f.region),
		AccountID: strings.TrimSpace(f.accountID),
		ConnStr:   f.connStr,
		Scope:     strings.TrimSpace(f.scope),
		CreatedAt: time.Now(),
	}
}

func (f *secretForm) valid() bool {
	if f.name == "" {
		return false
	}
	switch f.sectype {
	case SecretAzure:
		return f.connStr != ""
	default:
		return f.keyID != "" && f.secret != ""
	}
}

// ── update dispatch (secrets mode) ────────────────────────────────────────

func (tm TokenManager) updateSecretsMode(msg tea.KeyMsg) (TokenManager, tea.Cmd) {
	// Form is open — route to it
	if tm.secretForm != nil {
		return tm.updateSecretForm(msg)
	}
	// Delete confirmation dialog
	if tm.secretDelConfirm {
		return tm.updateSecretDelConfirm(msg)
	}

	switch msg.String() {
	case "up", "k":
		if tm.secretCursor > 0 {
			tm.secretCursor--
		}
	case "down", "j":
		if tm.secretCursor < len(tm.secrets)-1 {
			tm.secretCursor++
		}
	case "n":
		tm.secretForm = newSecretForm()
	case "d":
		if len(tm.secrets) > 0 {
			tm.secretDelConfirm = true
		}
	case "v":
		tm.showSecret = !tm.showSecret
	case "e":
		if s := tm.selectedSecret(); s != nil {
			if path, err := exportSecretSQL(*s); err == nil {
				tm.exportedPath = path
				tm.successMsg = "exported → " + path
				tm.successTTL = 60
			} else {
				tm.successMsg = "✕ export failed: " + err.Error()
				tm.successTTL = 60
			}
		}
	}
	return tm, nil
}

func (tm TokenManager) updateSecretForm(msg tea.KeyMsg) (TokenManager, tea.Cmd) {
	f := tm.secretForm
	visible := f.visibleFields()

	switch msg.String() {
	case "esc":
		tm.secretForm = nil
		return tm, nil

	case "tab", "down":
		if f.focusIdx < len(visible)-1 {
			f.focusIdx++
		}
		return tm, nil

	case "shift+tab", "up":
		if f.focusIdx > -1 {
			f.focusIdx--
		}
		return tm, nil

	case "left":
		if f.focusIdx == -1 {
			for i := 0; i < len(AllSecretTypes)-1; i++ {
				f.sectype = f.sectype.Next()
			}
		}
		return tm, nil

	case "right", " ":
		if f.focusIdx == -1 {
			f.sectype = f.sectype.Next()
			return tm, nil
		}
		if f.focusIdx >= 0 && f.focusIdx < len(visible) {
			fld := visible[f.focusIdx]
			*fld.value += " "
		}
		return tm, nil

	case "enter":
		if f.focusIdx == -1 {
			f.focusIdx = 0
			return tm, nil
		}
		if f.focusIdx < len(visible)-1 {
			f.focusIdx++
			return tm, nil
		}
		if !f.valid() {
			return tm, nil
		}
		sec := f.toSecret()
		if f.editing >= 0 && f.editing < len(tm.secrets) {
			// preserve creation time on edit
			sec.CreatedAt = tm.secrets[f.editing].CreatedAt
			tm.secrets[f.editing] = sec
			tm.successMsg = "updated " + sec.Name
		} else {
			tm.secrets = append(tm.secrets, sec)
			tm.successMsg = "created " + sec.Name
		}
		tm.successTTL = 60
		_ = SaveStorageSecrets(tm.secrets)
		tm.secretForm = nil
		return tm, nil

	case "backspace":
		if f.focusIdx >= 0 && f.focusIdx < len(visible) {
			fld := visible[f.focusIdx]
			if len(*fld.value) > 0 {
				*fld.value = (*fld.value)[:len(*fld.value)-1]
			}
		}
		return tm, nil

	default:
		if f.focusIdx >= 0 && f.focusIdx < len(visible) && len(msg.String()) == 1 {
			fld := visible[f.focusIdx]
			*fld.value += msg.String()
		}
	}
	return tm, nil
}

func (tm TokenManager) updateSecretDelConfirm(msg tea.KeyMsg) (TokenManager, tea.Cmd) {
	switch msg.String() {
	case "y", "Y":
		if tm.secretCursor < len(tm.secrets) {
			deleted := tm.secrets[tm.secretCursor].Name
			tm.secrets = append(tm.secrets[:tm.secretCursor], tm.secrets[tm.secretCursor+1:]...)
			_ = SaveStorageSecrets(tm.secrets)
			if tm.secretCursor >= len(tm.secrets) && tm.secretCursor > 0 {
				tm.secretCursor--
			}
			tm.successMsg = "deleted " + deleted
			tm.successTTL = 60
		}
		tm.secretDelConfirm = false
	case "n", "N", "esc":
		tm.secretDelConfirm = false
	}
	return tm, nil
}

func (tm *TokenManager) selectedSecret() *StorageSecret {
	if len(tm.secrets) == 0 || tm.secretCursor >= len(tm.secrets) {
		return nil
	}
	return &tm.secrets[tm.secretCursor]
}

// ── views ─────────────────────────────────────────────────────────────────

func (tm TokenManager) ViewSecretList(width, height int) string {
	header := lipgloss.JoinHorizontal(lipgloss.Top,
		mutedStyle.Render("  Quack tokens   "),
		lipgloss.NewStyle().
			Background(colorDuckYellow).Foreground(colorDarkBg).Bold(true).
			Padding(0, 1).Render("Storage secrets"),
	)

	if len(tm.secrets) == 0 {
		return lipgloss.JoinVertical(lipgloss.Left,
			header,
			"",
			labelStyle.Render("  STORAGE SECRETS"),
			"",
			mutedStyle.Render("  no storage secrets yet — press [n] to create one"),
			"",
			mutedStyle.Render("  these are credentials used by DuckLake configs (and any"),
			mutedStyle.Render("  local connection pointing at a remote .duckdb file) to"),
			mutedStyle.Render("  read object storage. Quack connections don't use them"),
			mutedStyle.Render("  — the Quack server has its own credentials."),
		)
	}

	lines := []string{
		header,
		"",
		labelStyle.Render("  STORAGE SECRETS  ") +
			mutedStyle.Render(fmt.Sprintf("(%d total)", len(tm.secrets))),
		"",
		mutedStyle.Render("  NAME                  TYPE   SCOPE                          STATUS"),
		mutedStyle.Render("  ────                  ────   ─────                          ──────"),
	}
	for i, s := range tm.secrets {
		cursor := "  "
		name := brightStyle.Render(padRight(s.Name, 22))
		if i == tm.secretCursor {
			cursor = amberStyle.Render("▶ ")
			name = labelStyle.Render(padRight(s.Name, 22))
		}
		typeCell := mutedStyle.Render(padRight(string(s.Type), 7))
		scope := s.Scope
		if scope == "" {
			scope = mutedStyle.Render("(no scope)")
		}
		scope = padRight(truncate(scope, 30), 30)
		status := greenStyle.Render("● active")
		lines = append(lines, cursor+name+typeCell+scope+"   "+status)
	}
	return strings.Join(lines, "\n")
}

func (tm TokenManager) ViewSecretDetail(width int) string {
	s := tm.selectedSecret()
	if s == nil {
		return mutedStyle.Render("  no secret selected")
	}

	mask := func(v string) string {
		if v == "" {
			return mutedStyle.Render("(empty)")
		}
		if tm.showSecret {
			return brightStyle.Render(v)
		}
		if len(v) <= 6 {
			return mutedStyle.Render("••••••")
		}
		return mutedStyle.Render(v[:3]+"…"+v[len(v)-3:]) +
			" " + mutedStyle.Render("(v to reveal)")
	}

	rows := []string{
		labelStyle.Render("SECRET DETAIL"),
		"",
		row("name", brightStyle.Render(s.Name)),
		row("type", amberStyle.Render(string(s.Type))),
	}
	switch s.Type {
	case SecretAzure:
		rows = append(rows, row("connstr", mask(s.ConnStr)))
	default:
		rows = append(rows, row("key id", mask(s.KeyID)), row("secret", mask(s.Secret)))
		if s.Type == SecretS3 && s.Region != "" {
			rows = append(rows, row("region", brightStyle.Render(s.Region)))
		}
		if s.Type == SecretR2 {
			rows = append(rows, row("account", brightStyle.Render(s.AccountID)))
		}
	}
	if s.Scope != "" {
		rows = append(rows, row("scope", brightStyle.Render(s.Scope)))
	}
	if !s.CreatedAt.IsZero() {
		rows = append(rows, row("created", mutedStyle.Render(fmtRelative(s.CreatedAt))))
	}

	rows = append(rows,
		"",
		mutedStyle.Render(strings.Repeat("─", width-4)),
		"",
		labelStyle.Render("SQL"),
		"",
		renderCodeBlock(s.ToSQL(), width-4),
	)
	return strings.Join(rows, "\n")
}

func (tm TokenManager) ViewSecretForm(width, height int) string {
	f := tm.secretForm
	visible := f.visibleFields()

	title := "NEW STORAGE SECRET"
	if f.editing >= 0 {
		title = "EDIT STORAGE SECRET"
	}

	// Type selector
	var chips []string
	for _, t := range AllSecretTypes {
		chip := mutedStyle.Render(" " + string(t) + " ")
		if t == f.sectype {
			chip = lipgloss.NewStyle().
				Foreground(colorDarkBg).Background(colorDuckYellow).Bold(true).
				Padding(0, 1).Render(string(t))
		}
		chips = append(chips, chip)
	}
	cursor := "  "
	if f.focusIdx == -1 {
		cursor = amberStyle.Render("▶ ")
	}
	typeLine := cursor + mutedStyle.Render(padRight("Type", 12)) +
		strings.Join(chips, mutedStyle.Render("  "))

	// Field rows
	var fieldLines []string
	for i, fld := range visible {
		cur := "  "
		if i == f.focusIdx {
			cur = amberStyle.Render("▶ ")
		}
		val := *fld.value
		if i == f.focusIdx {
			val += amberStyle.Render("█")
		}
		display := brightStyle.Render(val)
		if *fld.value == "" {
			display = mutedStyle.Render(fld.placeholder)
		}
		fieldLines = append(fieldLines,
			cur+mutedStyle.Render(padRight(fld.label, 12))+display,
			"    "+mutedStyle.Render(fld.hint),
		)
	}

	hint := mutedStyle.Render("  [↑↓/tab] field  [←→/space] cycle type  [enter] advance/save  [esc] cancel")
	if !f.valid() {
		hint += "  " + redStyle.Render("· required fields missing")
	}

	content := lipgloss.JoinVertical(lipgloss.Left,
		labelStyle.Render(title), "",
		typeLine, "",
		strings.Join(fieldLines, "\n"), "",
		hint,
	)
	return activePanelStyle.Width(width).Render(content)
}

// ── secret SQL export ─────────────────────────────────────────────────────

func exportSecretSQL(s StorageSecret) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "/tmp"
	}
	dir := filepath.Join(home, ".duckdb")
	if err := os.MkdirAll(dir, 0750); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "pintail_storage_secrets.sql")

	content := fmt.Sprintf(
		"-- Pintail storage secret export — %s\n-- secret: %s (type=%s)\n\n%s\n",
		time.Now().Format(time.RFC3339),
		s.Name, s.Type, s.ToSQL(),
	)
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		return "", err
	}
	return path, nil
}
