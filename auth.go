package main

import (
	"context"
	"fmt"
	"strings"
	"time"

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

	applyMsg   string
	applyIsErr bool
	applyTTL   int

	// dirty records whether any permission has been toggled since the editor
	// was opened, so the screen can say that edits are pending and the root
	// model knows to write them back to the tokens.
	dirty bool

	clients []*QuackClient
	// targetIdx selects which connection an applied policy is sent to. Apply
	// used to always use clients[0] regardless of the selected token.
	targetIdx int
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

	// Default the apply target to the first connection that can actually accept
	// a token policy — a Quack server. Anything else has no notion of one.
	target := -1
	for i, c := range clients {
		if c.Config.Supports(CapTokenAuth) {
			target = i
			break
		}
	}
	return AuthEditor{policies: policies, clients: clients, targetIdx: target}
}

// Permissions returns the granted operation list for each policy, keyed by
// token name, so the caller can write toggles back to the tokens they came
// from. Edits used to live only in this screen and were discarded on esc.
func (a AuthEditor) Permissions() map[string][]string {
	out := make(map[string][]string, len(a.policies))
	for _, p := range a.policies {
		var ops []string
		for _, perm := range p.Perms {
			if perm.Allowed && perm.Op != "ALL" {
				ops = append(ops, perm.Op)
			}
		}
		out[p.TokenName] = ops
	}
	return out
}

// Dirty reports whether any permission was toggled.
func (a AuthEditor) Dirty() bool { return a.dirty }

// targetClient returns the connection an apply would be sent to, or nil.
func (a AuthEditor) targetClient() *QuackClient {
	if a.targetIdx < 0 || a.targetIdx >= len(a.clients) {
		return nil
	}
	return a.clients[a.targetIdx]
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
					a.dirty = true
					// "ALL" toggle: flip all others
					if p.Perms[a.permCursor].Op == "ALL" {
						v := p.Perms[a.permCursor].Allowed
						for i := range p.Perms {
							p.Perms[i].Allowed = v
						}
					}
				}
			}

		// Cycle which connection an apply would target. Silently applying to
		// clients[0] meant a policy could land on a server that had nothing to
		// do with the selected token.
		case "T":
			a.cycleTarget()

		case "a":
			// Apply — generate the SQL and send it to the chosen target.
			if a.cursor >= len(a.policies) {
				return a, nil
			}
			sql := a.applySQL(a.policies[a.cursor])
			c := a.targetClient()
			switch {
			case c == nil:
				a.setApplyMsg("no Quack connection to apply to — SQL shown below to copy", true)
			case !c.HasCLI():
				a.setApplyMsg("duckdb CLI not found in PATH — SQL shown below to copy", true)
			case !c.GetState().Online:
				a.setApplyMsg(c.Config.Name+" is offline — SQL shown below to copy", true)
			default:
				a.setApplyMsg("applying to "+c.Config.Name+"…", false)
				return a, applyPolicyCmd(c, sql)
			}
		}

	// Result of an apply. This is a distinct message from the scratchpad's
	// queryResultMsg: routing it through that type sent the outcome to the
	// scratchpad, so this screen never reported success or failure and the
	// scratchpad's own result was overwritten.
	case authApplyResultMsg:
		if msg.err != "" {
			a.setApplyMsg("apply failed: "+firstLine(msg.err), true)
		} else {
			a.setApplyMsg("applied to "+msg.target, false)
		}
	}
	return a, nil
}

// authApplyResultMsg carries the outcome of an ALTER SECRET apply back to this
// screen rather than to the scratchpad.
type authApplyResultMsg struct {
	target string
	err    string
}

// applyPolicyCmd runs the generated policy SQL against one connection.
func applyPolicyCmd(c *QuackClient, sql string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if _, err := c.queryCLI(ctx, sql); err != nil {
			return authApplyResultMsg{target: c.Config.Name, err: err.Error()}
		}
		return authApplyResultMsg{target: c.Config.Name}
	}
}

func (a *AuthEditor) setApplyMsg(msg string, isErr bool) {
	a.applyMsg = msg
	a.applyIsErr = isErr
	a.applyTTL = 8
}

// cycleTarget moves to the next connection that supports token auth.
func (a *AuthEditor) cycleTarget() {
	if len(a.clients) == 0 {
		return
	}
	for i := 1; i <= len(a.clients); i++ {
		next := (a.targetIdx + i) % len(a.clients)
		if a.clients[next].Config.Supports(CapTokenAuth) {
			a.targetIdx = next
			return
		}
	}
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

	// ALTER SECRET is not a statement stock DuckDB accepts (it is a parser
	// error as of 1.5.5) — whether it works depends entirely on what the Quack
	// server adds. Saying so beats letting an apply fail mysteriously.
	lines = append(lines,
		mutedStyle.Render("  ALTER SECRET is not stock DuckDB syntax — applying it"),
		mutedStyle.Render("  requires a Quack server that implements it."))

	// Where an apply would go, so [a] is never a surprise.
	target := mutedStyle.Render("  apply target  ") + redStyle.Render("none configured")
	if c := a.targetClient(); c != nil {
		state := "offline"
		style := redStyle
		if c.GetState().Online {
			state, style = "online", greenStyle
		}
		target = mutedStyle.Render("  apply target  ") +
			brightStyle.Render(c.Config.Name) + "  " + style.Render(state)
	}
	lines = append(lines, "", target)

	if a.dirty {
		lines = append(lines, "  "+amberStyle.Render("● unsaved permission edits")+
			mutedStyle.Render("  saved to the token list on [esc]"))
	}

	if a.applyMsg != "" {
		if a.applyIsErr {
			lines = append(lines, "", "  "+redStyle.Render("✕ "+a.applyMsg))
		} else {
			lines = append(lines, "", "  "+greenStyle.Render("✓ "+a.applyMsg))
		}
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
			keyBadge("T") + " target",
			keyBadge("tab") + " token list",
			keyBadge("esc") + " save & back",
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
