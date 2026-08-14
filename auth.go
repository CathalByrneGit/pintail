package main

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// ── types ─────────────────────────────────────────────────────────────────

// Permission is a toggleable SQL operation right.
type Permission struct {
	Op      string // SELECT, INSERT, UPDATE, DELETE, CREATE, DROP
	Allowed bool
}

// PolicyEntry is a token's editable permission profile.
type PolicyEntry struct {
	TokenName string
	Scope     []string
	Perms     []Permission
	Active    bool
}

// AuthEditor holds the state for the auth policy editor screen.
type AuthEditor struct {
	policies   []PolicyEntry
	cursor     int // selected policy (left panel)
	permCursor int // selected permission row (right panel)
	focus      int // 0 = policy list, 1 = perm list

	applyMsg string
	applyTTL int

	clients []*QuackClient
}

// ── constructor ───────────────────────────────────────────────────────────

func NewAuthEditor(tokens []Token, clients []*QuackClient) AuthEditor {
	policies := make([]PolicyEntry, len(tokens))
	for i, t := range tokens {
		perms := buildPerms(t.Permissions)
		policies[i] = PolicyEntry{
			TokenName: t.Name,
			Scope:     t.Scope,
			Perms:     perms,
			Active:    t.Active,
		}
	}
	return AuthEditor{policies: policies, clients: clients}
}

func buildPerms(granted []string) []Permission {
	all := []string{"SELECT", "INSERT", "UPDATE", "DELETE", "CREATE", "DROP", "ALL"}
	out := make([]Permission, len(all))
	grantedSet := make(map[string]bool)
	for _, p := range granted {
		grantedSet[strings.ToUpper(p)] = true
	}
	for i, op := range all {
		out[i] = Permission{
			Op:      op,
			Allowed: grantedSet[op] || grantedSet["*"] || grantedSet["ALL"],
		}
	}
	return out
}

// ── Update ────────────────────────────────────────────────────────────────

func (a AuthEditor) Update(msg tea.Msg) (AuthEditor, tea.Cmd) {
	switch msg := msg.(type) {
	case tickMsg:
		if a.applyTTL > 0 {
			a.applyTTL--
			if a.applyTTL == 0 {
				a.applyMsg = ""
			}
		}

	case tea.KeyMsg:
		switch msg.String() {

		case "tab":
			a.focus = 1 - a.focus // toggle between policy list and perm list

		case "up", "k":
			if a.focus == 0 {
				if a.cursor > 0 {
					a.cursor--
					a.permCursor = 0
				}
			} else {
				if a.permCursor > 0 {
					a.permCursor--
				}
			}

		case "down", "j":
			if a.focus == 0 {
				if a.cursor < len(a.policies)-1 {
					a.cursor++
					a.permCursor = 0
				}
			} else {
				if a.cursor < len(a.policies) {
					p := a.policies[a.cursor]
					if a.permCursor < len(p.Perms)-1 {
						a.permCursor++
					}
				}
			}

		case " ", "enter":
			// Toggle the focused permission
			if a.focus == 1 && a.cursor < len(a.policies) {
				p := &a.policies[a.cursor]
				if a.permCursor < len(p.Perms) {
					p.Perms[a.permCursor].Allowed = !p.Perms[a.permCursor].Allowed
					// "ALL" toggle: flip all others
					if p.Perms[a.permCursor].Op == "ALL" {
						v := p.Perms[a.permCursor].Allowed
						for i := range p.Perms {
							p.Perms[i].Allowed = v
						}
					}
				}
			}

		case "a":
			// Apply — generate SQL and send via CLI if available
			if a.cursor < len(a.policies) {
				sql := a.applySQL(a.policies[a.cursor])
				a.applyMsg = "Generated: " + truncate(sql, 60)
				a.applyTTL = 6
				// If we have an online client, ship it
				if len(a.clients) > 0 {
					c := a.clients[0]
					if c.GetState().Online && c.HasCLI() {
						return a, c.QueryAsync(sql, c.Config.ToServerInfo())
					}
				}
			}
		}
	}
	return a, nil
}

// ── View helpers ──────────────────────────────────────────────────────────

func (a AuthEditor) ViewPolicyList(width, height int) string {
	var lines []string
	lines = append(lines, labelStyle.Render("TOKENS"), "")

	for i, p := range a.policies {
		cursor := "  "
		nameStyle := brightStyle
		dot := greenStyle.Render("●")

		if !p.Active {
			dot = redStyle.Render("✕")
			nameStyle = lipgloss.NewStyle().Foreground(colorMuted).Strikethrough(true)
		}
		if i == a.cursor {
			cursor = amberStyle.Render("▶ ")
			nameStyle = labelStyle
		}

		granted := 0
		for _, perm := range p.Perms {
			if perm.Allowed && perm.Op != "ALL" {
				granted++
			}
		}
		total := len(p.Perms) - 1 // exclude ALL row

		line := cursor + dot + " " + nameStyle.Render(p.TokenName) + "\n" +
			"    " + mutedStyle.Render(fmt.Sprintf("%d/%d ops  scope: %s",
			granted, total, strings.Join(p.Scope, ", ")))
		lines = append(lines, line)
	}

	return strings.Join(lines, "\n")
}

func (a AuthEditor) ViewPermGrid(width int) string {
	if a.cursor >= len(a.policies) {
		return mutedStyle.Render("no policy selected")
	}
	p := a.policies[a.cursor]

	var lines []string
	lines = append(lines,
		labelStyle.Render("PERMISSIONS"),
		"  "+mutedStyle.Render(p.TokenName),
		"  "+mutedStyle.Render("scope: "+strings.Join(p.Scope, ", ")),
		"",
	)

	for i, perm := range p.Perms {
		cursor := "   "
		if a.focus == 1 && i == a.permCursor {
			cursor = amberStyle.Render(" ▶ ")
		}

		// Toggle indicator
		var box string
		if perm.Allowed {
			box = greenStyle.Render("  ✓  ")
		} else {
			box = redStyle.Render("  ✕  ")
		}

		// Separator before ALL row
		if perm.Op == "ALL" {
			lines = append(lines, "  "+mutedStyle.Render(strings.Repeat("─", 20)))
		}

		line := cursor + box + brightStyle.Render(padRight(perm.Op, 8)) +
			mutedStyle.Render(permDesc(perm.Op))
		lines = append(lines, line)
	}

	lines = append(lines, "")
	lines = append(lines,
		mutedStyle.Render(hrule(width-6)),
		"",
		labelStyle.Render("GENERATED SQL"),
		"",
	)

	sql := a.applySQL(p)
	lines = append(lines, renderCodeBlock(sql, width-4))

	if a.applyMsg != "" {
		lines = append(lines, "", "  "+greenStyle.Render("✓ "+a.applyMsg))
	}

	return strings.Join(lines, "\n")
}

func (a AuthEditor) ViewFooter() string {
	var keys []string
	if a.focus == 0 {
		keys = []string{
			keyBadge("↑↓") + " select token",
			keyBadge("tab") + " edit perms",
			keyBadge("esc") + " back",
		}
	} else {
		keys = []string{
			keyBadge("↑↓") + " select perm",
			keyBadge("space") + " toggle",
			keyBadge("a") + " apply",
			keyBadge("tab") + " token list",
			keyBadge("esc") + " back",
		}
	}
	return footerStyle.Render(strings.Join(keys, "   "))
}

// ── SQL generator ─────────────────────────────────────────────────────────

func (a AuthEditor) applySQL(p PolicyEntry) string {
	var ops []string
	for _, perm := range p.Perms {
		if perm.Allowed && perm.Op != "ALL" {
			ops = append(ops, perm.Op)
		}
	}
	if len(ops) == 0 {
		ops = []string{"NONE"}
	}

	scope := "*"
	if len(p.Scope) > 0 && p.Scope[0] != "*" {
		scope = strings.Join(p.Scope, ", ")
	}

	return fmt.Sprintf(
		"-- Update permissions for token: %s\nALTER SECRET %s\n  SET PERMISSIONS (%s)\n  FOR SCOPE '%s';",
		p.TokenName,
		p.TokenName,
		strings.Join(ops, ", "),
		scope,
	)
}

// ── helpers ───────────────────────────────────────────────────────────────

func permDesc(op string) string {
	switch op {
	case "SELECT":
		return "read rows"
	case "INSERT":
		return "write new rows"
	case "UPDATE":
		return "modify existing rows"
	case "DELETE":
		return "remove rows"
	case "CREATE":
		return "create tables / schemas"
	case "DROP":
		return "delete tables / schemas"
	case "ALL":
		return "grant or revoke everything above"
	}
	return ""
}
