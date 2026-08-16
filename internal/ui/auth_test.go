package ui

import (
	"strings"
	"testing"

	"github.com/CathalByrneGit/pintail/internal/quack"
)

func TestAuthEditorTogglesArePreserved(t *testing.T) {
	tokens := []quack.Token{
		quack.BuildToken("etl", "analytics", "SELECT", "never"),
		quack.BuildToken("admin", "*", "SELECT, INSERT", "never"),
	}
	a := NewAuthEditor(tokens, nil)

	if a.Dirty() {
		t.Error("a freshly opened editor should not be dirty")
	}
	if got := a.Permissions()["etl"]; len(got) != 1 || got[0] != "SELECT" {
		t.Fatalf("initial etl permissions = %v, want [SELECT]", got)
	}

	// Focus the permission grid and grant INSERT (index 1) to etl.
	a.focus = 1
	a.permCursor = 1
	a, _ = a.Update(key(" "))

	if !a.Dirty() {
		t.Error("toggling a permission should mark the editor dirty")
	}
	got := a.Permissions()["etl"]
	if len(got) != 2 || got[0] != "SELECT" || got[1] != "INSERT" {
		t.Errorf("etl permissions = %v, want [SELECT INSERT]", got)
	}

	// The other token is untouched.
	if other := a.Permissions()["admin"]; len(other) != 2 {
		t.Errorf("admin permissions = %v, want them unchanged", other)
	}
}

func TestAuthEditorAllToggleGrantsEverything(t *testing.T) {
	a := NewAuthEditor([]quack.Token{quack.BuildToken("t", "*", "SELECT", "never")}, nil)
	a.focus = 1

	// "ALL" is the last row.
	perms := a.policies[0].Perms
	a.permCursor = len(perms) - 1
	if perms[a.permCursor].Op != "ALL" {
		t.Fatalf("last row is %q, want ALL", perms[a.permCursor].Op)
	}

	a, _ = a.Update(key(" "))
	granted := a.Permissions()["t"]
	if len(granted) != len(perms)-1 {
		t.Errorf("granted %v, want every operation except the ALL row itself", granted)
	}

	// Toggling ALL off clears them.
	a, _ = a.Update(key(" "))
	if granted := a.Permissions()["t"]; len(granted) != 0 {
		t.Errorf("granted %v, want none", granted)
	}
}

// Apply used to be sent to clients[0] whatever it was, and its result was
// routed to the scratchpad, so this screen never reported an outcome.
func TestAuthEditorApplyTargeting(t *testing.T) {
	local := quack.NewQuackClient(quack.ServerConfig{Name: "local-dev", Type: quack.ConnLocal, Path: "/tmp/x.duckdb"}, nil, nil)
	quackA := quack.NewQuackClient(quack.ServerConfig{Name: "quack-a", Type: quack.ConnQuack, Host: "a", Port: 9494}, nil, nil)
	quackB := quack.NewQuackClient(quack.ServerConfig{Name: "quack-b", Type: quack.ConnQuack, Host: "b", Port: 9494}, nil, nil)

	// A local connection cannot hold a token policy, so it must not be chosen
	// as the default target just for being first.
	a := NewAuthEditor([]quack.Token{quack.BuildToken("t", "*", "SELECT", "never")},
		[]*quack.QuackClient{local, quackA, quackB})

	target := a.targetClient()
	if target == nil || target.Config.Name != "quack-a" {
		t.Fatalf("default target = %v, want quack-a", target)
	}

	a.cycleTarget()
	if got := a.targetClient(); got.Config.Name != "quack-b" {
		t.Errorf("after cycling, target = %q, want quack-b", got.Config.Name)
	}
	a.cycleTarget()
	if got := a.targetClient(); got.Config.Name != "quack-a" {
		t.Errorf("cycling should skip the local connection and wrap, got %q", got.Config.Name)
	}

	// With no token-auth backend at all there is no target, and apply says so
	// instead of sending the statement somewhere arbitrary.
	noneEditor := NewAuthEditor([]quack.Token{quack.BuildToken("t", "*", "SELECT", "never")}, []*quack.QuackClient{local})
	if noneEditor.targetClient() != nil {
		t.Error("a local-only setup should have no apply target")
	}
	noneEditor.focus = 1
	noneEditor, cmd := noneEditor.Update(key("a"))
	if cmd != nil {
		t.Error("apply should not run a command when there is no target")
	}
	if !noneEditor.applyIsErr || !strings.Contains(noneEditor.applyMsg, "no Quack connection") {
		t.Errorf("applyMsg = %q (isErr=%v), want a no-target explanation",
			noneEditor.applyMsg, noneEditor.applyIsErr)
	}
}

func TestAuthEditorReportsApplyOutcome(t *testing.T) {
	a := NewAuthEditor([]quack.Token{quack.BuildToken("t", "*", "SELECT", "never")}, nil)

	a, _ = a.Update(authApplyResultMsg{target: "quack-a"})
	if a.applyIsErr || !strings.Contains(a.applyMsg, "applied to quack-a") {
		t.Errorf("success message = %q (isErr=%v)", a.applyMsg, a.applyIsErr)
	}

	a, _ = a.Update(authApplyResultMsg{target: "quack-a", err: "Parser Error: syntax error at or near \"SECRET\"\nmore detail"})
	if !a.applyIsErr {
		t.Error("a failed apply must be marked as an error")
	}
	if !strings.Contains(a.applyMsg, "Parser Error") {
		t.Errorf("failure message = %q, want the backend error in it", a.applyMsg)
	}
	if strings.Contains(a.applyMsg, "more detail") {
		t.Errorf("failure message should be a single line, got %q", a.applyMsg)
	}
}

// The generated policy has to be the mechanism Quack actually enforces — an
// authorization callback macro plus the setting that points at it — not the
// ALTER SECRET statement this screen used to emit, which DuckDB cannot parse.
func TestAuthEditorGeneratesTheRealAuthorizationHook(t *testing.T) {
	tok := quack.BuildToken("etl", "analytics", "SELECT, INSERT", "never")
	a := NewAuthEditor([]quack.Token{tok}, nil)

	sql := a.applySQL(a.policies[0])

	for _, want := range []string{
		"CREATE OR REPLACE MACRO pintail_authz(sid, query)",
		"regexp_matches(upper(trim(query))",
		"SET GLOBAL quack_authorization_function = 'pintail_authz';",
		"SELECT", "INSERT",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("generated SQL missing %q:\n%s", want, sql)
		}
	}
	if strings.Contains(sql, "ALTER SECRET") {
		t.Errorf("generated SQL still uses ALTER SECRET, which DuckDB cannot parse:\n%s", sql)
	}
	// DuckDB's FROM-first syntax is a read, and so are the other read shapes the
	// Quack docs group with SELECT.
	for _, want := range []string{"FROM", "WITH", "EXPLAIN", "DESCRIBE", "SHOW"} {
		if !strings.Contains(sql, want) {
			t.Errorf("SELECT should admit %q as a read shape:\n%s", want, sql)
		}
	}
	// Operations that were not granted must not appear in the pattern.
	for _, unwanted := range []string{"DROP", "DELETE"} {
		if strings.Contains(sql, "|"+unwanted) || strings.Contains(sql, "("+unwanted) {
			t.Errorf("ungranted %q leaked into the pattern:\n%s", unwanted, sql)
		}
	}
	// The limits are stated, not hidden.
	if !strings.Contains(sql, "per SERVER") {
		t.Errorf("the SQL should say the hook is global to the server:\n%s", sql)
	}
	if !strings.Contains(sql, "not airtight") {
		t.Errorf("the SQL should carry the prefix-matching caveat:\n%s", sql)
	}
}

func TestAuthEditorDenyAllPolicy(t *testing.T) {
	tok := quack.BuildToken("locked", "*", "", "never")
	a := NewAuthEditor([]quack.Token{tok}, nil)
	// Clear everything, including the SELECT default buildToken applies.
	for i := range a.policies[0].Perms {
		a.policies[0].Perms[i].Allowed = false
	}

	sql := a.applySQL(a.policies[0])
	if !strings.Contains(sql, "AS false;") {
		t.Errorf("a policy with nothing granted should deny every query:\n%s", sql)
	}
	if strings.Contains(sql, "regexp_matches") {
		t.Errorf("deny-all needs no pattern:\n%s", sql)
	}
}

// The panel has to say that the hook is server-wide, since the screen presents
// per-token toggles and Quack provides no per-token isolation.
func TestAuthEditorStatesTheHookIsServerWide(t *testing.T) {
	a := NewAuthEditor([]quack.Token{quack.BuildToken("t", "analytics", "SELECT", "never")}, nil)
	grid := a.ViewPermGrid(80)

	if !strings.Contains(grid, "quack_authorization_function") {
		t.Errorf("the generated SQL should be shown:\n%s", grid)
	}
	if !strings.Contains(grid, "one authorization hook per server") {
		t.Errorf("the panel should say the hook is server-wide:\n%s", grid)
	}
}

// Permission edits have to reach the tokens, which is what the root model does
// on esc. Before, they lived only in the editor and vanished.
func TestModelWritesAuthEditsBackToTokens(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	tok := quack.BuildToken("etl", "analytics", "SELECT", "never")
	if err := quack.SaveTokens([]quack.Token{tok}); err != nil {
		t.Fatal(err)
	}

	m := Model{tokenMgr: NewTokenManager()}
	m.authEditor = NewAuthEditor(m.tokenMgr.tokens, nil)

	// Grant INSERT.
	m.authEditor.focus = 1
	m.authEditor.permCursor = 1
	m.authEditor, _ = m.authEditor.Update(key(" "))

	m.applyAuthEdits()

	if got := m.tokenMgr.tokens[0].Permissions; len(got) != 2 || got[1] != "INSERT" {
		t.Errorf("in-memory token permissions = %v, want [SELECT INSERT]", got)
	}
	saved := quack.LoadTokens()
	if len(saved) != 1 {
		t.Fatalf("expected one persisted token, got %d", len(saved))
	}
	if len(saved[0].Permissions) != 2 || saved[0].Permissions[1] != "INSERT" {
		t.Errorf("persisted permissions = %v, want [SELECT INSERT]", saved[0].Permissions)
	}

	// Reopening the editor reflects the saved state.
	reopened := NewAuthEditor(quack.LoadTokens(), nil)
	if got := reopened.Permissions()["etl"]; len(got) != 2 {
		t.Errorf("reopened editor permissions = %v, want two ops", got)
	}
}

func TestSameStrings(t *testing.T) {
	tests := []struct {
		a, b []string
		want bool
	}{
		{nil, nil, true},
		{[]string{}, nil, true},
		{[]string{"SELECT"}, []string{"SELECT"}, true},
		{[]string{"SELECT"}, []string{"INSERT"}, false},
		{[]string{"SELECT"}, []string{"SELECT", "INSERT"}, false},
	}
	for _, tc := range tests {
		if got := sameStrings(tc.a, tc.b); got != tc.want {
			t.Errorf("sameStrings(%v, %v) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

// Quack runs exactly one authorization callback per server, so an apply
// overwrites whatever is installed. Replacing another tool's — or a
// hand-deployed — access control without asking is not Pintail's call, so a
// foreign hook aborts the apply and is reported.
func TestAuthEditorRefusesToOverwriteAForeignHook(t *testing.T) {
	a := NewAuthEditor([]quack.Token{quack.BuildToken("t", "*", "SELECT", "never")}, nil)

	a, _ = a.Update(authApplyResultMsg{target: "quack-a", conflict: "acme_authz"})
	if !a.applyIsErr {
		t.Error("a conflict must be reported as an error")
	}
	for _, want := range []string{"acme_authz", "not overwriting", "[R]"} {
		if !strings.Contains(a.applyMsg, want) {
			t.Errorf("conflict message %q is missing %q", a.applyMsg, want)
		}
	}
}

// The default is a named allow-all callback, not an empty setting, so treating
// "" as the only unset value would make every fresh server look like it already
// had a policy and block the first apply.
func TestAuthzDefaultIsNotEmpty(t *testing.T) {
	if authzDefault != "quack_nop_authorization" {
		t.Errorf("authzDefault = %q; Quack ships quack_nop_authorization as the default hook",
			authzDefault)
	}
	if authzSetting != "quack_authorization_function" {
		t.Errorf("authzSetting = %q, want quack_authorization_function", authzSetting)
	}
}

// Quack hands the whole query string to the hook, and Pintail's management
// script starts with CREATE. A policy denying CREATE therefore denies the next
// apply, which makes it effectively one-way — so it needs saying up front and
// confirming before it goes out.
func TestApplyConfirmsAPolicyThatLocksPintailOut(t *testing.T) {
	srv := onlineQuackClient("q")

	// SELECT only: CREATE denied, so this is the one-way case.
	a := NewAuthEditor([]quack.Token{quack.BuildToken("t", "*", "SELECT", "never")}, []*quack.QuackClient{srv})
	a.focus = 1

	if !policyLocksOutPintail(a.policies[0]) {
		t.Fatal("a SELECT-only policy denies CREATE and should be flagged")
	}

	// The warning is on screen before any key is pressed.
	grid := a.ViewPermGrid(100)
	if !strings.Contains(grid, "one-way") {
		t.Errorf("the perm grid does not warn that the policy is one-way:\n%s", grid)
	}

	// First [a] arms the confirmation and sends nothing.
	a, cmd := a.Update(key("a"))
	if cmd != nil {
		t.Error("the first apply of a one-way policy must not send anything")
	}
	if !a.confirmApply {
		t.Error("confirmation was not armed")
	}
	if !strings.Contains(a.applyMsg, "confirm") {
		t.Errorf("applyMsg = %q, want it to ask for confirmation", a.applyMsg)
	}

	// A different key cancels rather than leaving the prompt armed.
	cancelled, _ := a.Update(key("j"))
	if cancelled.confirmApply {
		t.Error("an unrelated key should abandon the pending confirmation")
	}

	// Second [a] goes through.
	a, cmd = a.Update(key("a"))
	if cmd == nil {
		t.Error("the confirmed apply should send the policy")
	}
	if a.confirmApply {
		t.Error("confirmation should be cleared once the apply is sent")
	}
}

// A policy that allows CREATE can be replaced by a later apply, so it needs no
// confirmation and must not be labelled one-way.
func TestApplyDoesNotConfirmAReversiblePolicy(t *testing.T) {
	srv := onlineQuackClient("q")

	a := NewAuthEditor([]quack.Token{quack.BuildToken("t", "*", "SELECT, CREATE", "never")}, []*quack.QuackClient{srv})
	a.focus = 1

	if policyLocksOutPintail(a.policies[0]) {
		t.Fatal("a policy allowing CREATE is reversible and must not be flagged")
	}
	if got := a.ViewPermGrid(100); strings.Contains(got, "one-way") {
		t.Errorf("a reversible policy should not be labelled one-way:\n%s", got)
	}

	a, cmd := a.Update(key("a"))
	if cmd == nil {
		t.Error("a reversible policy should apply on the first keypress")
	}
	if a.confirmApply {
		t.Error("no confirmation should be armed for a reversible policy")
	}
}

// The statement-prefix caveat has to be visible on the screen where the toggles
// are, not only inside the generated SQL further down the panel.
func TestPermGridStatesItIsNotASecurityBoundary(t *testing.T) {
	a := NewAuthEditor([]quack.Token{quack.BuildToken("t", "*", "SELECT", "never")}, nil)
	got := a.ViewPermGrid(100)

	for _, want := range []string{"not a security boundary", "prefix", "READ_ONLY"} {
		if !strings.Contains(got, want) {
			t.Errorf("perm grid is missing %q:\n%s", want, got)
		}
	}
}

// [R] is the documented way back from a policy that denies CREATE, and it has
// to be reachable — including being advertised in the footer.
func TestResetHookIsAvailable(t *testing.T) {
	srv := onlineQuackClient("q")

	a := NewAuthEditor([]quack.Token{quack.BuildToken("t", "*", "SELECT", "never")}, []*quack.QuackClient{srv})
	a.focus = 1

	if got := a.ViewFooter(); !strings.Contains(got, "reset hook") {
		t.Errorf("footer does not offer the reset: %s", got)
	}

	a, cmd := a.Update(key("R"))
	if cmd == nil {
		t.Fatal("[R] should issue a reset")
	}
	if !strings.Contains(a.applyMsg, authzDefault) {
		t.Errorf("applyMsg = %q, want it to name the default hook", a.applyMsg)
	}
}

func TestHookIsForeign(t *testing.T) {
	tests := []struct {
		current string
		want    bool
	}{
		// Quack's shipped default means nothing has been installed. Reading it
		// as an existing policy would block the first apply on every server.
		{"quack_nop_authorization", false},
		{"", false},
		// Our own macro is ours to replace.
		{"pintail_authz", false},
		// Anything else belongs to another tool or a hand-built deployment.
		{"acme_authz", true},
		{"authz_no", true},
	}
	for _, tc := range tests {
		if got := hookIsForeign(tc.current); got != tc.want {
			t.Errorf("hookIsForeign(%q) = %v, want %v", tc.current, got, tc.want)
		}
	}
}

// onlineQuackClient is a client that reports itself online with a CLI present,
// so a test about which keystrokes send what does not quietly skip on a machine
// with no duckdb installed — the earlier guards in apply would short-circuit
// before the behaviour under test.
func onlineQuackClient(name string) *quack.QuackClient {
	return quack.NewQuackClient(
		quack.ServerConfig{Name: name, Type: quack.ConnQuack, Host: "h", Port: 9494}, nil, nil,
		quack.WithState(quack.ConnState{Online: true}),
		// Never executed: these tests assert on which keystrokes send what, not
		// on results, and must not skip on a machine without duckdb installed.
		quack.WithCLI("/nonexistent/duckdb"),
	)
}
