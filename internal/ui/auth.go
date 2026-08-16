package ui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/CathalByrneGit/pintail/internal/quack"
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

	// confirmApply is armed when an apply would install a policy that denies
	// Pintail's own management statements, and requires a second [a] to go
	// through.
	confirmApply bool

	// dirty records whether any permission has been toggled since the editor
	// was opened, so the screen can say that edits are pending and the root
	// model knows to write them back to the tokens.
	dirty bool

	clients []*quack.QuackClient
	// targetIdx selects which connection an applied policy is sent to. Apply
	// used to always use clients[0] regardless of the selected token.
	targetIdx int
}

// ── constructor ───────────────────────────────────────────────────────────

func NewAuthEditor(tokens []quack.Token, clients []*quack.QuackClient) AuthEditor {
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
		if c.Config.Supports(quack.CapTokenAuth) {
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
func (a AuthEditor) targetClient() *quack.QuackClient {
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
			p := a.policies[a.cursor]
			c := a.targetClient()
			switch {
			case c == nil:
				a.setApplyMsg("no Quack connection to apply to — SQL shown below to copy", true)
			case !c.HasCLI():
				a.setApplyMsg("duckdb CLI not found in PATH — SQL shown below to copy", true)
			case !c.GetState().Online:
				a.setApplyMsg(c.Config.Name+" is offline — SQL shown below to copy", true)
			case policyLocksOutPintail(p) && !a.confirmApply:
				// One more keystroke, deliberately. Every statement Pintail
				// sends the server arrives as one string that the hook itself
				// vets, and Pintail's management script begins with CREATE — so
				// a policy that denies CREATE denies the next apply too. The
				// server then cannot be edited from here at all.
				a.confirmApply = true
				a.setApplyMsg("this policy denies CREATE, so Pintail cannot change it again — press [a] to confirm, any other key to cancel", true)
			default:
				a.confirmApply = false
				a.setApplyMsg("applying to "+c.Config.Name+"…", false)
				return a, applyPolicyCmd(c, a.applySQL(p))
			}

		// Restore the server's default allow-all callback. Worth a key of its
		// own: after a policy that denies CREATE this is the only management
		// statement that still gets through.
		case "R":
			c := a.targetClient()
			switch {
			case c == nil:
				a.setApplyMsg("no Quack connection to reset", true)
			case !c.HasCLI():
				a.setApplyMsg("duckdb CLI not found in PATH", true)
			case !c.GetState().Online:
				a.setApplyMsg(c.Config.Name+" is offline", true)
			default:
				a.setApplyMsg("resetting "+c.Config.Name+" to "+authzDefault+"…", false)
				return a, resetPolicyCmd(c)
			}
		}

		// Any key other than a second [a] abandons a pending confirmation, so a
		// stray keypress cannot leave the prompt armed.
		if msg.String() != "a" {
			a.confirmApply = false
		}

	// Result of an apply. This is a distinct message from the scratchpad's
	// queryResultMsg: routing it through that type sent the outcome to the
	// scratchpad, so this screen never reported success or failure and the
	// scratchpad's own result was overwritten.
	case authApplyResultMsg:
		switch {
		case msg.conflict != "":
			a.setApplyMsg(fmt.Sprintf(
				"%s already uses the hook %q — not overwriting it; press [R] to reset to %s first",
				msg.target, msg.conflict, authzDefault), true)
		case msg.err != "":
			a.setApplyMsg("apply failed: "+firstLine(msg.err), true)
		default:
			a.setApplyMsg("applied to "+msg.target, false)
		}
	}
	return a, nil
}

// hookIsForeign reports whether the hook currently installed on a server
// belongs to somebody else, and so must not be overwritten by an apply.
//
// Quack's default is a named allow-all callback rather than an empty setting, so
// authzDefault counts as "nothing installed" — treating only "" that way would
// make every fresh server look occupied and block the first apply.
func hookIsForeign(current string) bool {
	switch current {
	case "", authzDefault, authzMacroName:
		return false
	}
	return true
}

// policyLocksOutPintail reports whether applying this policy would deny the
// statements Pintail needs to manage the policy afterwards.
//
// Quack hands the authorization callback the whole query string, and Pintail's
// management script is `CREATE OR REPLACE MACRO … ; SET GLOBAL …`, so the hook
// sees a string beginning with CREATE. Deny CREATE and the next apply is
// rejected by the policy currently in force — only RESET GLOBAL still works.
func policyLocksOutPintail(p PolicyEntry) bool {
	for _, perm := range p.Perms {
		if perm.Op == "CREATE" && perm.Allowed {
			return false
		}
	}
	return true
}

// authApplyResultMsg carries the outcome of a policy apply back to this screen
// rather than to the scratchpad.
type authApplyResultMsg struct {
	target string
	err    string
	// conflict names the hook already installed on the server when it is
	// somebody else's. The apply is abandoned in that case rather than
	// overwriting it.
	conflict string
}

// authzSetting is the server setting that names the authorization callback.
const authzSetting = "quack_authorization_function"

// authzDefault is the value the setting holds when nobody has installed a hook.
// Quack ships an allow-all callback rather than an empty setting, so "unset" is
// this name and not "".
const authzDefault = "quack_nop_authorization"

// applyPolicyCmd installs the generated policy on the server.
//
// The macro and the setting have to exist in the server process — that is where
// the authorization hook runs — so this goes through quack_query() rather than
// our own attached session, which would have created the macro locally and left
// the server's policy untouched.
//
// The setting is read first. Quack has exactly one authorization callback per
// server, so applying a policy overwrites whatever was there — including a hook
// some other tool or a hand-written deployment installed. Silently replacing
// another system's access control is not ours to do, so a foreign hook aborts
// the apply and is reported instead.
func applyPolicyCmd(c *quack.QuackClient, sql string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		current, err := c.ServerSetting(ctx, authzSetting)
		if err != nil {
			return authApplyResultMsg{target: c.Config.Name,
				err: fmt.Sprintf("could not read %s: %s", authzSetting, err)}
		}
		if hookIsForeign(current) {
			return authApplyResultMsg{target: c.Config.Name, conflict: current}
		}

		if err := c.RunServerSQL(ctx, sql); err != nil {
			return authApplyResultMsg{target: c.Config.Name, err: err.Error()}
		}
		return authApplyResultMsg{target: c.Config.Name}
	}
}

// resetPolicyCmd restores the server's default allow-all callback.
//
// This is the documented way back: RESET GLOBAL returns the setting to
// quack_nop_authorization. It matters because an applied policy that does not
// permit CREATE denies Pintail's own management statements, and then this is the
// last request Pintail can still make successfully.
func resetPolicyCmd(c *quack.QuackClient) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := c.RunServerSQL(ctx, "RESET GLOBAL "+authzSetting); err != nil {
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
		if a.clients[next].Config.Supports(quack.CapTokenAuth) {
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
		// This belongs on the screen, not only in a comment inside the generated
		// SQL further down. What the toggles compile to is a regexp over the
		// statement text, and an admin reading a grid of SELECT/INSERT/DELETE
		// checkboxes will otherwise reasonably assume real privilege enforcement.
		"  "+amberStyle.Render("⚠ not a security boundary")+
			mutedStyle.Render("  these toggles compile to a"),
		"  "+mutedStyle.Render("statement-prefix match; WITH x AS (…) INSERT … passes as a read."),
		"  "+mutedStyle.Render("Attach the database READ_ONLY for enforcement that holds."),
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

	// The hook is global to the server. Presenting per-token toggles without
	// saying so would imply an isolation Quack does not provide.
	lines = append(lines,
		mutedStyle.Render("  Quack runs one authorization hook per server, so applying this"),
		mutedStyle.Render("  sets the policy for every token — see the comments in the SQL."))

	// A policy that denies CREATE denies Pintail's own management script, which
	// the hook vets like any other query. Say so before [a], not after.
	if policyLocksOutPintail(p) {
		lines = append(lines,
			"",
			"  "+redStyle.Render("⚠ one-way: ")+
				mutedStyle.Render("this denies CREATE, so Pintail cannot apply another"),
			"  "+mutedStyle.Render("policy afterwards. Recovery is [R] (RESET GLOBAL) or local access"),
			"  "+mutedStyle.Render("to the server process. Applying asks for confirmation."))
	}

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
			keyBadge("R") + " reset hook",
			keyBadge("T") + " target",
			keyBadge("tab") + " token list",
			keyBadge("esc") + " save & back",
		}
	}
	return footerStyle.Render(strings.Join(keys, "   "))
}

// ── SQL generator ─────────────────────────────────────────────────────────

// authzMacroName is the macro the generated policy defines on the server.
const authzMacroName = "pintail_authz"

// statementPrefixes maps a toggled operation to the statement keywords that
// begin such a query. SELECT covers DuckDB's FROM-first syntax and the read-only
// shapes the Quack docs group with it.
func statementPrefixes(op string) []string {
	switch op {
	case "SELECT":
		return []string{"SELECT", "FROM", "WITH", "EXPLAIN", "DESCRIBE", "SHOW"}
	default:
		return []string{op}
	}
}

// applySQL renders the policy as what Quack actually enforces.
//
// Quack has no per-token grant table and no ALTER SECRET statement — the
// previous version of this generator emitted one, which is a parser error. What
// exists is a single authorization callback per server: a function named by the
// quack_authorization_function setting, invoked as
// `SELECT <fn>(<connection_id>, <query>)` before every query, admitting it on
// TRUE. The default returns TRUE for everything.
//
// So the toggles compile into a macro over the statement text. Two limits are
// stated in the output rather than hidden: the hook is global to the server, and
// prefix matching is not a robust filter.
func (a AuthEditor) applySQL(p PolicyEntry) string {
	var ops, prefixes []string
	for _, perm := range p.Perms {
		if perm.Allowed && perm.Op != "ALL" {
			ops = append(ops, perm.Op)
			prefixes = append(prefixes, statementPrefixes(perm.Op)...)
		}
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "-- Authorization policy derived from token: %s\n", p.TokenName)
	if len(ops) == 0 {
		sb.WriteString("-- Nothing is allowed: this denies every query.\n")
	} else {
		fmt.Fprintf(&sb, "-- Allowed: %s\n", strings.Join(ops, ", "))
	}
	sb.WriteString("--\n")
	sb.WriteString("-- Quack's authorization hook is per SERVER, not per token. To scope a\n")
	sb.WriteString("-- policy to one token, add an authentication hook that records\n")
	sb.WriteString("-- connection_id → user, then look it up here via the sid argument.\n")

	if len(ops) == 0 {
		fmt.Fprintf(&sb, "CREATE OR REPLACE MACRO %s(sid, query) AS false;\n", authzMacroName)
	} else {
		sb.WriteString("-- Prefix matching is illustrative, not airtight: a query like\n")
		sb.WriteString("-- WITH x AS (SELECT 1) INSERT INTO t SELECT * FROM x starts with WITH\n")
		sb.WriteString("-- yet still writes. For real read-only enforcement, attach the database\n")
		sb.WriteString("-- read-only or inspect the parsed statement type instead.\n")
		fmt.Fprintf(&sb, "CREATE OR REPLACE MACRO %s(sid, query) AS\n", authzMacroName)
		fmt.Fprintf(&sb, "    regexp_matches(upper(trim(query)), '^(%s)\\b');\n",
			strings.Join(prefixes, "|"))
	}
	fmt.Fprintf(&sb, "SET GLOBAL quack_authorization_function = '%s';", authzMacroName)
	return sb.String()
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
