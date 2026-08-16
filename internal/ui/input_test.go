package ui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/CathalByrneGit/pintail/internal/quack"
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
		configs: []quack.ServerConfig{{Name: "existing", Type: quack.ConnQuack, Host: "h", Port: 9494}},
		addForm: newAddServerForm(),
	}
	m.clients = quack.InitClients(m.configs, nil)
	m.addForm.focusIdx = 0 // Name field
	m.addForm.name = "prod"

	next, _ := m.updateAddServer(key("right"))
	got := next.(Model).addForm

	if got.name != "prod" {
		t.Errorf("name = %q, want it untouched by the arrow key", got.name)
	}
	if got.connType != quack.ConnQuack {
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
	if got := next.(Model).addForm; got.connType != quack.ConnLocal {
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
	if tm.secretForm.sectype != quack.SecretS3 {
		t.Errorf("sectype = %q, want it unchanged while a field is focused", tm.secretForm.sectype)
	}

	tm, _ = tm.updateSecretForm(key(" "))
	if tm.secretForm.name != "lake_s3 " {
		t.Errorf("name = %q, want space to still be typeable", tm.secretForm.name)
	}

	tm.secretForm.focusIdx = -1
	tm, _ = tm.updateSecretForm(key("right"))
	if tm.secretForm.sectype != quack.SecretR2 {
		t.Errorf("sectype = %q, want → on the type row to cycle to r2", tm.secretForm.sectype)
	}
}

// "e" was bound to save-file ahead of the text-input fallback, so no field on
// the TLS screen could contain the letter — including the Domain field, whose
// own placeholder is quack.example.com.
func TestTLSScreenAcceptsTheLetterE(t *testing.T) {
	g := NewTLSGenerator([]quack.ServerConfig{{Name: "q", Host: "localhost", Port: 9494}})
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

	g := NewTLSGenerator([]quack.ServerConfig{{Name: "q", Host: "localhost", Port: 9494}})
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
	g := NewTLSGenerator([]quack.ServerConfig{{Name: "q", Host: "backend", Port: 9494}})

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

// The Quack server is HTTP/1.1 (cpp-httplib, no HTTP/2 anywhere), so a
// generated proxy config must not force HTTP/2 upstream — doing so breaks every
// request through the proxy. All three generators used to.
func TestGeneratedProxyConfigsDoNotForceHTTP2Upstream(t *testing.T) {
	g := NewTLSGenerator([]quack.ServerConfig{{Name: "q", Host: "backend", Port: 9494}})
	g.fields[0].value = "quack.example.com"

	tests := []struct {
		proxy    proxyKind
		wantIn   []string
		wantNone []string
	}{
		{
			proxy:    proxyCaddy,
			wantIn:   []string{"versions 1.1", "reverse_proxy backend:9494"},
			wantNone: []string{"h2c"},
		},
		{
			proxy:  proxyNginx,
			wantIn: []string{"proxy_http_version 1.1", `proxy_set_header   Connection ""`},
			// No websocket upgrade in the Quack protocol, and no HTTP/2 upstream.
			wantNone: []string{"$http_upgrade", "HTTP/2 upstream support"},
		},
		{
			proxy:    proxyEnvoy,
			wantIn:   []string{"http_protocol_options: {}"},
			wantNone: []string{"http2_protocol_options"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.proxy.String(), func(t *testing.T) {
			g.proxy = tc.proxy
			cfg := g.generateConfig()
			for _, want := range tc.wantIn {
				if !strings.Contains(cfg, want) {
					t.Errorf("%v config missing %q:\n%s", tc.proxy, want, cfg)
				}
			}
			for _, unwanted := range tc.wantNone {
				if strings.Contains(cfg, unwanted) {
					t.Errorf("%v config should not contain %q:\n%s", tc.proxy, unwanted, cfg)
				}
			}
		})
	}
}

// The Quack reverse-proxy guide documents four settings without which a proxied
// server misbehaves: large request bodies (PREPARE carries SQL, APPEND carries
// DataChunks), unbuffered responses (results stream back as repeated FETCHes),
// long timeouts (a query can sit between FETCHes for minutes), and upstream
// keep-alive (the server keeps connection state on the persistent connection).
func TestGeneratedProxyConfigsCarryTheDocumentedSettings(t *testing.T) {
	g := NewTLSGenerator([]quack.ServerConfig{{Name: "q", Host: "127.0.0.1", Port: 9494}})
	g.fields[0].value = "quack.example.com"

	tests := []struct {
		proxy  proxyKind
		wantIn []string
	}{
		{
			proxy: proxyCaddy,
			wantIn: []string{
				"flush_interval -1", // unbuffered streaming
				"max_size 256MB",    // large bodies
				"keepalive 30s",     // upstream keep-alive
			},
		},
		{
			proxy: proxyNginx,
			wantIn: []string{
				"client_max_body_size 256M",
				"proxy_buffering off",
				"proxy_read_timeout  600s",
				`proxy_set_header   Connection ""`,
			},
		},
		{
			proxy: proxyEnvoy,
			wantIn: []string{
				"timeout: 600s", // Envoy defaults to 15s
				"per_connection_buffer_limit_bytes: 268435456", // 1 MiB default is too small
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.proxy.String(), func(t *testing.T) {
			g.proxy = tc.proxy
			cfg := g.generateConfig()
			for _, want := range tc.wantIn {
				if !strings.Contains(cfg, want) {
					t.Errorf("%v config missing %q:\n%s", tc.proxy, want, cfg)
				}
			}
		})
	}
}
