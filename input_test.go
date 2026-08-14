package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func key(s string) tea.KeyMsg {
	switch s {
	case "right":
		return tea.KeyMsg{Type: tea.KeyRight}
	case "left":
		return tea.KeyMsg{Type: tea.KeyLeft}
	case "tab":
		return tea.KeyMsg{Type: tea.KeyTab}
	case "ctrl+s":
		return tea.KeyMsg{Type: tea.KeyCtrlS}
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	default:
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
	}
}

// The arrow key used to be handled together with space, so pressing → while
// editing any field in either hand-rolled form appended a space to the value.
func TestArrowRightDoesNotTypeIntoConnectionForm(t *testing.T) {
	m := Model{
		configs: []ServerConfig{{Name: "existing", Type: ConnQuack, Host: "h", Port: 9494}},
		addForm: newAddServerForm(),
	}
	m.clients = InitClients(m.configs, nil)
	m.addForm.focusIdx = 0 // Name field
	m.addForm.name = "prod"

	next, _ := m.updateAddServer(key("right"))
	got := next.(Model).addForm

	if got.name != "prod" {
		t.Errorf("name = %q, want it untouched by the arrow key", got.name)
	}
	if got.connType != ConnQuack {
		t.Errorf("connType = %q, want the type unchanged while a field is focused", got.connType)
	}

	// Space still types, and → still cycles the type while the type row is
	// focused: neither behaviour was supposed to change.
	next, _ = next.(Model).updateAddServer(key(" "))
	if got := next.(Model).addForm; got.name != "prod " {
		t.Errorf("name = %q, want space to still be typeable", got.name)
	}

	onType := next.(Model)
	onType.addForm.focusIdx = -1
	next, _ = onType.updateAddServer(key("right"))
	if got := next.(Model).addForm; got.connType != ConnLocal {
		t.Errorf("connType = %q, want → on the type row to cycle to local", got.connType)
	}
}

func TestArrowRightDoesNotTypeIntoSecretForm(t *testing.T) {
	tm := TokenManager{mode: tmModeSecrets, secretForm: newSecretForm()}
	tm.secretForm.focusIdx = 0 // Name field
	tm.secretForm.name = "lake_s3"

	tm, _ = tm.updateSecretForm(key("right"))
	if tm.secretForm.name != "lake_s3" {
		t.Errorf("name = %q, want it untouched by the arrow key", tm.secretForm.name)
	}
	if tm.secretForm.sectype != SecretS3 {
		t.Errorf("sectype = %q, want it unchanged while a field is focused", tm.secretForm.sectype)
	}

	tm, _ = tm.updateSecretForm(key(" "))
	if tm.secretForm.name != "lake_s3 " {
		t.Errorf("name = %q, want space to still be typeable", tm.secretForm.name)
	}

	tm.secretForm.focusIdx = -1
	tm, _ = tm.updateSecretForm(key("right"))
	if tm.secretForm.sectype != SecretR2 {
		t.Errorf("sectype = %q, want → on the type row to cycle to r2", tm.secretForm.sectype)
	}
}

// "e" was bound to save-file ahead of the text-input fallback, so no field on
// the TLS screen could contain the letter — including the Domain field, whose
// own placeholder is quack.example.com.
func TestTLSScreenAcceptsTheLetterE(t *testing.T) {
	g := NewTLSGenerator([]ServerConfig{{Name: "q", Host: "localhost", Port: 9494}})
	g.focusIdx = 0 // Domain

	for _, r := range "quack.example.com" {
		g, _ = g.Update(key(string(r)))
	}

	if want := "quack.example.com"; g.fields[0].value != want {
		t.Errorf("Domain = %q, want %q", g.fields[0].value, want)
	}
	if !strings.Contains(g.generateConfig(), "quack.example.com") {
		t.Error("generated config does not include the typed domain")
	}
}

func TestTLSScreenSaveAndReportFailure(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	g := NewTLSGenerator([]ServerConfig{{Name: "q", Host: "localhost", Port: 9494}})
	g.focusIdx = 0
	for _, r := range "quack.example.com" {
		g, _ = g.Update(key(string(r)))
	}

	g, _ = g.Update(key("ctrl+s"))
	if g.saveErr != "" {
		t.Fatalf("unexpected save error: %s", g.saveErr)
	}
	want := filepath.Join(home, ".duckdb", "tls", "pintail-Caddyfile")
	if g.savedPath != want {
		t.Fatalf("savedPath = %q, want %q", g.savedPath, want)
	}
	body, err := os.ReadFile(want)
	if err != nil {
		t.Fatalf("reading saved config: %v", err)
	}
	if !strings.Contains(string(body), "quack.example.com") {
		t.Error("saved Caddyfile does not contain the configured domain")
	}
	if !strings.Contains(g.ViewStatusBar(), "saved") {
		t.Errorf("status bar should confirm the save, got %q", g.ViewStatusBar())
	}

	// A write that cannot succeed must say so rather than looking like a no-op.
	blocked := filepath.Join(home, "blocked")
	if err := os.WriteFile(blocked, nil, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", blocked) // a file, so MkdirAll under it must fail

	g, _ = g.Update(key("ctrl+s"))
	if g.saveErr == "" {
		t.Error("want a save error when the target directory cannot be created")
	}
	if g.savedPath != "" {
		t.Errorf("savedPath = %q, want it cleared after a failure", g.savedPath)
	}
	if !strings.Contains(g.ViewStatusBar(), "save failed") {
		t.Errorf("status bar should report the failure, got %q", g.ViewStatusBar())
	}
}

func TestTLSProxyCycleRegeneratesConfig(t *testing.T) {
	g := NewTLSGenerator([]ServerConfig{{Name: "q", Host: "backend", Port: 9494}})

	tests := []struct {
		proxy    proxyKind
		wantFile string
		wantIn   string
	}{
		{proxyCaddy, "Caddyfile", "reverse_proxy backend:9494"},
		{proxyNginx, "nginx.conf", "upstream quack_backend"},
		{proxyEnvoy, "envoy.yaml", "static_resources"},
	}
	for _, tc := range tests {
		t.Run(tc.wantFile, func(t *testing.T) {
			if g.proxy != tc.proxy {
				t.Fatalf("proxy = %v, want %v", g.proxy, tc.proxy)
			}
			if g.proxy.FileExt() != tc.wantFile {
				t.Errorf("FileExt = %q, want %q", g.proxy.FileExt(), tc.wantFile)
			}
			if cfg := g.generateConfig(); !strings.Contains(cfg, tc.wantIn) {
				t.Errorf("config for %v does not contain %q", tc.proxy, tc.wantIn)
			}
			g, _ = g.Update(key("tab"))
		})
	}
}
