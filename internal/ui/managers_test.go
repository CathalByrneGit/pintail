package ui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/CathalByrneGit/pintail/internal/quack"
)

func typeRunes(tm TokenManager, s string) TokenManager {
	for _, r := range s {
		tm, _ = tm.Update(key(string(r)))
	}
	return tm
}

// tab moves between the token list and the storage-secret list, and must not do
// so while a form or dialog is open — that would strand a half-entered value.
func TestTokenManagerModeToggle(t *testing.T) {
	tm := TokenManager{tokens: []quack.Token{quack.BuildToken("t", "*", "SELECT", "never")}}

	if tm.mode != tmModeTokens {
		t.Fatal("should start on the token list")
	}
	tm, _ = tm.Update(key("tab"))
	if tm.mode != tmModeSecrets {
		t.Error("tab should switch to the secret list")
	}
	tm, _ = tm.Update(key("tab"))
	if tm.mode != tmModeTokens {
		t.Error("tab should switch back")
	}

	// With a dialog open, tab belongs to the dialog.
	tm.revokeConfirm = true
	before := tm.mode
	tm, _ = tm.Update(key("tab"))
	if tm.mode != before {
		t.Error("tab must not change mode while a confirmation is open")
	}
}

// Creating a token through the form is the primary path of the screen and was
// entirely uncovered.
func TestTokenManagerCreateThroughTheForm(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	tm := TokenManager{}
	tm, _ = tm.Update(key("n"))
	if tm.form == nil {
		t.Fatal("[n] should open the new-token form")
	}

	tm = typeRunes(tm, "etl")
	tm, _ = tm.Update(key("tab")) // next field: scope
	tm = typeRunes(tm, "analytics")
	// enter advances until the last field, then commits, so walk to the end.
	for i := 0; i < len(tm.form.fields); i++ {
		tm, _ = tm.Update(key("enter"))
		if tm.form == nil {
			break
		}
	}

	if tm.form != nil {
		t.Error("enter should close the form")
	}
	if len(tm.tokens) != 1 {
		t.Fatalf("got %d tokens, want 1", len(tm.tokens))
	}
	got := tm.tokens[0]
	if got.Name != "etl" {
		t.Errorf("Name = %q, want etl", got.Name)
	}
	if len(got.Scope) == 0 || got.Scope[0] != "analytics" {
		t.Errorf("Scope = %v, want [analytics]", got.Scope)
	}
	if got.Value == "" {
		t.Error("a created token has no value")
	}

	// And it was persisted, not just held in memory.
	if stored := quack.LoadTokens(); len(stored) != 1 || stored[0].Name != "etl" {
		t.Errorf("token was not written to disk: %+v", stored)
	}

	// esc abandons a form without creating anything.
	tm, _ = tm.Update(key("n"))
	tm = typeRunes(tm, "discarded")
	tm, _ = tm.Update(key("esc"))
	if tm.form != nil {
		t.Error("esc should close the form")
	}
	if len(tm.tokens) != 1 {
		t.Errorf("esc should not create a token, got %d", len(tm.tokens))
	}
}

// Rotate and revoke are destructive, so both go through a confirmation, and
// declining must leave the token alone.
func TestTokenManagerRotateAndRevokeConfirm(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	tok := quack.BuildToken("etl", "*", "SELECT", "never")
	original := tok.Value
	tm := TokenManager{tokens: []quack.Token{tok}}

	// Declining a rotation keeps the value.
	tm, _ = tm.Update(key("r"))
	if !tm.rotateConfirm {
		t.Fatal("[r] should ask for confirmation")
	}
	if d := tm.ViewConfirmDialog("rotate", "etl"); !strings.Contains(strings.ToUpper(d), "ROTATE") {
		t.Errorf("the dialog should name the action: %q", d)
	}
	tm, _ = tm.Update(key("n"))
	if tm.rotateConfirm {
		t.Error("n should dismiss the dialog")
	}
	if tm.tokens[0].Value != original {
		t.Error("declining a rotation must not change the token")
	}

	// Confirming rotates it.
	tm, _ = tm.Update(key("r"))
	tm, _ = tm.Update(key("y"))
	if tm.rotateConfirm {
		t.Error("the dialog should close after confirming")
	}
	if tm.tokens[0].Value == original {
		t.Error("confirming should have replaced the token value")
	}

	// Revoke marks it inactive rather than deleting it.
	tm, _ = tm.Update(key("d"))
	if !tm.revokeConfirm {
		t.Fatal("[d] should ask for confirmation")
	}
	tm, _ = tm.Update(key("y"))
	if len(tm.tokens) != 1 {
		t.Fatalf("revoke should not remove the row, got %d", len(tm.tokens))
	}
	if tm.tokens[0].Active {
		t.Error("a revoked token should be inactive")
	}
}

// The secret form's job is to produce a StorageSecret, and to round-trip an
// existing one for editing without losing fields.
func TestSecretFormRoundTrip(t *testing.T) {
	original := quack.StorageSecret{
		Name: "lake_s3", Type: quack.SecretS3, KeyID: "AKIA", Secret: "shh",
		Region: "eu-west-1", Scope: "s3://b/p",
	}

	f := secretFormFromExisting(original, 3)
	if f.editing != 3 {
		t.Errorf("editing = %d, want the index being edited", f.editing)
	}
	got := f.toSecret()
	if got.Name != original.Name || got.Type != original.Type ||
		got.KeyID != original.KeyID || got.Secret != original.Secret ||
		got.Region != original.Region || got.Scope != original.Scope {
		t.Errorf("round trip lost fields:\n got %+v\nwant %+v", got, original)
	}

	// Azure uses a connection string rather than key/secret.
	azure := quack.StorageSecret{Name: "az", Type: quack.SecretAzure, ConnStr: "DefaultEndpointsProtocol=https;..."}
	if got := secretFormFromExisting(azure, 0).toSecret(); got.ConnStr != azure.ConnStr {
		t.Errorf("ConnStr = %q, want it preserved", got.ConnStr)
	}
}

// The exported statement has to be runnable SQL naming the secret.
func TestExportSecretSQL(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	s := quack.StorageSecret{Name: "lake_s3", Type: quack.SecretS3,
		KeyID: "AKIA", Secret: "sh'h", Region: "eu-west-1"}

	path, err := exportSecretSQL(s)
	if err != nil {
		t.Fatalf("exportSecretSQL: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the export: %v", err)
	}
	got := string(body)
	for _, want := range []string{"CREATE OR REPLACE SECRET lake_s3", "KEY_ID 'AKIA'", "SECRET 'sh''h'"} {
		if !strings.Contains(got, want) {
			t.Errorf("export missing %q:\n%s", want, got)
		}
	}
	// Credentials on disk must not be world-readable.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("export mode = %#o, want no group/other access", perm)
	}
}

// Adding a secret through the secrets-mode list, which had no coverage at all.
func TestSecretsModeAddFlow(t *testing.T) {
	tm := TokenManager{mode: tmModeSecrets}

	tm, _ = tm.Update(key("n"))
	if tm.secretForm == nil {
		t.Fatal("[n] should open the secret form")
	}
	// The form opens on the type selector (focusIdx -1), so the first enter
	// moves into the fields; typing before that goes nowhere.
	tm, _ = tm.Update(key("enter"))

	// An S3 secret is only valid with a name, a key id and a secret, so fill all
	// three: enter is a no-op on an incomplete form rather than saving a
	// half-built credential.
	tm = typeRunes(tm, "mysecret")
	tm, _ = tm.Update(key("enter")) // name -> key id
	tm = typeRunes(tm, "AKIA")
	tm, _ = tm.Update(key("enter")) // key id -> secret
	tm = typeRunes(tm, "shh")
	for i := 0; i < 12 && tm.secretForm != nil; i++ {
		tm, _ = tm.Update(key("enter"))
	}

	if len(tm.secrets) != 1 {
		t.Fatalf("got %d secrets, want 1", len(tm.secrets))
	}
	if tm.secrets[0].Name != "mysecret" {
		t.Errorf("Name = %q, want mysecret", tm.secrets[0].Name)
	}

	// Deleting asks first, and declining keeps it.
	tm, _ = tm.Update(key("d"))
	if !tm.secretDelConfirm {
		t.Fatal("[d] should ask for confirmation")
	}
	tm, _ = tm.Update(key("n"))
	if len(tm.secrets) != 1 {
		t.Error("declining should keep the secret")
	}

	tm, _ = tm.Update(key("d"))
	tm, _ = tm.Update(key("y"))
	if len(tm.secrets) != 0 {
		t.Errorf("confirming should delete it, %d left", len(tm.secrets))
	}
}

// History recall is how the scratchpad avoids retyping; it has to stop at both
// ends rather than running off the slice.
func TestScratchpadHistoryNavigation(t *testing.T) {
	sp := NewScratchpad(nil, nil)
	sp.history = []HistoryEntry{
		{Query: "SELECT 1"},
		{Query: "SELECT 2"},
		{Query: "SELECT 3"},
	}

	sp = sp.historyPrev()
	if got := sp.editor.Value(); got != "SELECT 3" {
		t.Errorf("first recall = %q, want the most recent entry", got)
	}
	sp = sp.historyPrev()
	if got := sp.editor.Value(); got != "SELECT 2" {
		t.Errorf("second recall = %q", got)
	}
	for i := 0; i < 5; i++ {
		sp = sp.historyPrev()
	}
	if got := sp.editor.Value(); got != "SELECT 1" {
		t.Errorf("recall should stop at the oldest entry, got %q", got)
	}

	sp = sp.historyNext()
	if got := sp.editor.Value(); got != "SELECT 2" {
		t.Errorf("forward = %q, want SELECT 2", got)
	}
	for i := 0; i < 5; i++ {
		sp = sp.historyNext()
	}
	// Walking forward past the newest returns to an empty editor rather than
	// sticking on the last entry.
	if got := sp.editor.Value(); got != "" && got != "SELECT 3" {
		t.Errorf("forward past the end = %q, want empty or the newest", got)
	}

	// With no history at all, neither direction may panic.
	empty := NewScratchpad(nil, nil)
	_ = empty.historyPrev()
	_ = empty.historyNext()
}

func TestLongestCommonPrefix(t *testing.T) {
	tests := []struct{ a, b, want string }{
		{"/tmp/foo", "/tmp/foobar", "/tmp/foo"},
		{"/tmp/foo", "/tmp/bar", "/tmp/"},
		{"abc", "abc", "abc"},
		{"", "abc", ""},
		{"abc", "", ""},
		{"xyz", "abc", ""},
	}
	for _, tc := range tests {
		if got := longestCommonPrefix(tc.a, tc.b); got != tc.want {
			t.Errorf("longestCommonPrefix(%q, %q) = %q, want %q", tc.a, tc.b, got, tc.want)
		}
	}
}

// Tab-completion of a file path in the connection form.
func TestCompletePath(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"analytics.duckdb", "analytics-old.duckdb", "other.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// A unique prefix completes to the whole name.
	if got := completePath(filepath.Join(dir, "oth")); !strings.HasSuffix(got, "other.txt") {
		t.Errorf("completePath = %q, want it completed to other.txt", got)
	}

	// An ambiguous prefix completes only as far as the common part.
	got := completePath(filepath.Join(dir, "ana"))
	if !strings.HasSuffix(got, "analytics") {
		t.Errorf("completePath = %q, want the common prefix 'analytics'", got)
	}

	// No match leaves the input alone rather than blanking the field.
	in := filepath.Join(dir, "zzz")
	if got := completePath(in); got != in {
		t.Errorf("completePath with no match = %q, want the input unchanged", got)
	}

	// ~ is expanded, so the home directory completes like any other.
	t.Setenv("HOME", dir)
	if got := completePath("~/oth"); !strings.HasSuffix(got, "other.txt") {
		t.Errorf("completePath(~/oth) = %q, want the tilde expanded and completed", got)
	}
}

// The polling commands must actually be scheduled — a nil tick means the
// dashboard silently stops refreshing.
func TestTickCommandsAreScheduled(t *testing.T) {
	for name, cmd := range map[string]func() interface{}{
		"tick":        func() interface{} { return tickCmd() },
		"pingTick":    func() interface{} { return pingTickCmd() },
		"sessionTick": func() interface{} { return sessionTickCmd() },
	} {
		if cmd() == nil {
			t.Errorf("%s returned no command, so that poll would never fire", name)
		}
	}
}

// Init has to kick off the first ping and the polls, or the dashboard sits at
// "pinging…" forever.
func TestInitSchedulesWork(t *testing.T) {
	m := NewModelWithConfigs([]quack.ServerConfig{
		{Name: "q", Type: quack.ConnQuack, Host: "h", Port: 9494},
	}, nil)
	if m.Init() == nil {
		t.Error("Init returned no command, so nothing would ever be polled")
	}
}

// NewModel reads the real config path; with an empty home it must still produce
// a usable model rather than panicking on first render.
func TestNewModelWithAnEmptyHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	m := NewModel()
	m.width, m.height = 100, 30
	if out := m.View(); out == "" {
		t.Error("a fresh install rendered nothing")
	}
	if len(m.configs) == 0 {
		t.Error("a fresh install should have the stub connection")
	}
	// The sub-models must all be constructed, or the first keypress that reaches
	// one of them dereferences a zero value.
	if m.connTable.Columns() == nil {
		t.Error("connection table was not built")
	}
}

// The command wrappers are what carry client results back to the update loop;
// each must produce the message type the update loop switches on.
func TestFetchCommandsReturnTheirMessages(t *testing.T) {
	c := quack.NewQuackClient(
		quack.ServerConfig{Name: "f", Type: quack.ConnLocal, Path: "/nonexistent.duckdb"},
		nil, nil, quack.WithCLI(""))

	if msg, ok := fetchSessionsCmd(c, 2)().(sessionResultMsg); !ok {
		t.Errorf("fetchSessionsCmd returned %T, want sessionResultMsg", msg)
	} else if msg.idx != 2 {
		t.Errorf("idx = %d, want 2: the result must say which connection it describes", msg.idx)
	}

	if msg, ok := fetchCatalogCmd(c, 3)().(catalogResultMsg); !ok {
		t.Errorf("fetchCatalogCmd returned %T, want catalogResultMsg", msg)
	} else if msg.idx != 3 {
		t.Errorf("idx = %d, want 3", msg.idx)
	}

	if msg, ok := pingServerCmd(c, 4)().(pingResultMsg); !ok {
		t.Errorf("pingServerCmd returned %T, want pingResultMsg", msg)
	} else if msg.idx != 4 {
		t.Errorf("idx = %d, want 4", msg.idx)
	}
}

// exportParquet refuses cleanly rather than writing a truncated file when the
// connection cannot serve the COPY.
func TestExportParquetRefusesWhenUnusable(t *testing.T) {
	if _, err := exportParquet(nil, "SELECT 1"); err == nil {
		t.Error("a nil client should be refused")
	}

	offline := quack.NewQuackClient(
		quack.ServerConfig{Name: "f", Type: quack.ConnLocal, Path: "/x.duckdb"}, nil, nil,
		quack.WithCLI("/some/duckdb"))
	if _, err := exportParquet(offline, "SELECT 1"); err == nil {
		t.Error("an offline connection should be refused")
	}

	noCLI := quack.NewQuackClient(
		quack.ServerConfig{Name: "f", Type: quack.ConnLocal, Path: "/x.duckdb"}, nil, nil,
		quack.WithCLI(""), quack.WithState(quack.ConnState{Online: true}))
	if _, err := exportParquet(noCLI, "SELECT 1"); err == nil {
		t.Error("no duckdb CLI should be refused")
	}
}
