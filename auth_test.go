package main

import (
	"strings"
	"testing"
)

func TestAuthEditorTogglesArePreserved(t *testing.T) {
	tokens := []Token{
		buildToken("etl", "analytics", "SELECT", "never"),
		buildToken("admin", "*", "SELECT, INSERT", "never"),
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
	a := NewAuthEditor([]Token{buildToken("t", "*", "SELECT", "never")}, nil)
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
	local := NewQuackClient(ServerConfig{Name: "local-dev", Type: ConnLocal, Path: "/tmp/x.duckdb"}, nil, nil)
	quackA := NewQuackClient(ServerConfig{Name: "quack-a", Type: ConnQuack, Host: "a", Port: 9494}, nil, nil)
	quackB := NewQuackClient(ServerConfig{Name: "quack-b", Type: ConnQuack, Host: "b", Port: 9494}, nil, nil)

	// A local connection cannot hold a token policy, so it must not be chosen
	// as the default target just for being first.
	a := NewAuthEditor([]Token{buildToken("t", "*", "SELECT", "never")},
		[]*QuackClient{local, quackA, quackB})

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
	noneEditor := NewAuthEditor([]Token{buildToken("t", "*", "SELECT", "never")}, []*QuackClient{local})
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
	a := NewAuthEditor([]Token{buildToken("t", "*", "SELECT", "never")}, nil)

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

// The generated statement is not stock DuckDB syntax; the screen has to say so.
func TestAuthEditorFlagsNonStandardSQL(t *testing.T) {
	a := NewAuthEditor([]Token{buildToken("t", "analytics", "SELECT", "never")}, nil)
	grid := a.ViewPermGrid(80)

	if !strings.Contains(grid, "ALTER SECRET") {
		t.Error("the generated SQL should still be shown")
	}
	if !strings.Contains(grid, "not stock DuckDB syntax") {
		t.Error("the panel should warn that ALTER SECRET is not stock DuckDB syntax")
	}
}

// Permission edits have to reach the tokens, which is what the root model does
// on esc. Before, they lived only in the editor and vanished.
func TestModelWritesAuthEditsBackToTokens(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	tok := buildToken("etl", "analytics", "SELECT", "never")
	if err := SaveTokens([]Token{tok}); err != nil {
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
	saved := LoadTokens()
	if len(saved) != 1 {
		t.Fatalf("expected one persisted token, got %d", len(saved))
	}
	if len(saved[0].Permissions) != 2 || saved[0].Permissions[1] != "INSERT" {
		t.Errorf("persisted permissions = %v, want [SELECT INSERT]", saved[0].Permissions)
	}

	// Reopening the editor reflects the saved state.
	reopened := NewAuthEditor(LoadTokens(), nil)
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
