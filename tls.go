package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
)

// ── types ─────────────────────────────────────────────────────────────────

type proxyKind int

const (
	proxyCaddy proxyKind = iota
	proxyNginx
	proxyEnvoy
	proxyKindCount
)

func (p proxyKind) String() string {
	return [...]string{"Caddy", "Nginx", "Envoy"}[p]
}

func (p proxyKind) FileExt() string {
	return [...]string{"Caddyfile", "nginx.conf", "envoy.yaml"}[p]
}

type tlsField struct {
	label       string
	value       string
	placeholder string
	hint        string
}

// TLSGenerator holds all state for the TLS config generator screen.
type TLSGenerator struct {
	fields   []tlsField
	focusIdx int
	proxy    proxyKind
	configVP viewport.Model

	savedPath string
	savedAt   time.Time
	saveErr   string
}

// ── constructor ───────────────────────────────────────────────────────────

func NewTLSGenerator(configs []ServerConfig) TLSGenerator {
	upstream := "localhost:9494"
	if len(configs) > 0 {
		upstream = fmt.Sprintf("%s:%d", configs[0].Host, configs[0].Port)
	}

	vp := viewport.New(60, 20)
	vp.Style = mutedStyle

	g := TLSGenerator{
		fields: []tlsField{
			{
				label: "Domain", value: "",
				placeholder: "quack.example.com",
				hint:        "public hostname — Caddy auto-provisions a cert",
			},
			{
				label: "Upstream", value: upstream,
				placeholder: "localhost:9494",
				hint:        "host:port of the Quack server process",
			},
			{
				label: "Cert file", value: "/etc/ssl/certs/quack.crt",
				placeholder: "/etc/ssl/certs/quack.crt",
				hint:        "nginx / envoy only",
			},
			{
				label: "Key file", value: "/etc/ssl/private/quack.key",
				placeholder: "/etc/ssl/private/quack.key",
				hint:        "nginx / envoy only",
			},
		},
		proxy:    proxyCaddy,
		configVP: vp,
	}
	g.rebuildViewport(80)
	return g
}

// ── Update ────────────────────────────────────────────────────────────────

func (g TLSGenerator) Update(msg tea.Msg) (TLSGenerator, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "tab":
			g.proxy = (g.proxy + 1) % proxyKindCount
			g.rebuildViewport(80)

		case "up", "shift+tab":
			if g.focusIdx > 0 {
				g.focusIdx--
			}

		case "down":
			if g.focusIdx < len(g.fields)-1 {
				g.focusIdx++
			}

		case "enter":
			if g.focusIdx < len(g.fields)-1 {
				g.focusIdx++
			}

		case "backspace":
			f := &g.fields[g.focusIdx]
			if len(f.value) > 0 {
				f.value = f.value[:len(f.value)-1]
				g.rebuildViewport(80)
			}

		case "pgup", "ctrl+b":
			g.configVP, _ = g.configVP.Update(msg)

		case "pgdown", "ctrl+f":
			g.configVP, _ = g.configVP.Update(msg)

		// Save is ctrl+s, not "e": every field on this screen is free text, and
		// binding a bare letter made it impossible to type — "quack.example.com"
		// could not be entered into the Domain field at all.
		case "ctrl+s":
			path, err := g.saveToFile()
			if err != nil {
				// Silently ignoring this made a failed write look identical to
				// a successful one.
				g.saveErr = err.Error()
				g.savedPath = ""
				g.savedAt = time.Now()
				break
			}
			g.saveErr = ""
			g.savedPath = path
			g.savedAt = time.Now()

		default:
			if len(msg.String()) == 1 {
				g.fields[g.focusIdx].value += msg.String()
				g.rebuildViewport(80)
			}
		}
	}
	return g, nil
}

func (g *TLSGenerator) SetWidth(w int) {
	g.configVP.Width = w - 4
	g.rebuildViewport(w)
}

func (g *TLSGenerator) rebuildViewport(w int) {
	content := g.generateConfig()
	g.configVP.SetContent(content)
}

// ── View helpers ──────────────────────────────────────────────────────────

func (g TLSGenerator) ViewForm(width int) string {
	var lines []string
	lines = append(lines, labelStyle.Render("SETTINGS"), "")

	for i, f := range g.fields {
		cursor := "  "
		if i == g.focusIdx {
			cursor = amberStyle.Render("▶ ")
		}

		// Dim cert/key fields when Caddy is selected (not needed)
		val := f.value
		label := f.label
		hint := f.hint

		if (i == 2 || i == 3) && g.proxy == proxyCaddy {
			label = mutedStyle.Render(label)
			hint = mutedStyle.Render("(not needed for Caddy)")
			if val == "" {
				val = mutedStyle.Render(f.placeholder)
			} else {
				val = mutedStyle.Render(val)
			}
		} else {
			label = mutedStyle.Render(label)
			if val == "" {
				val = mutedStyle.Render(f.placeholder)
			} else {
				val = brightStyle.Render(val)
			}
			if i == g.focusIdx {
				val += amberStyle.Render("█")
			}
		}

		lines = append(lines,
			cursor+label,
			"    "+val,
			"    "+mutedStyle.Render(hint),
			"",
		)
	}

	// Proxy selector
	lines = append(lines, mutedStyle.Render(hrule(width-6)), "")
	lines = append(lines, labelStyle.Render("PROXY TYPE")+"  "+mutedStyle.Render("[tab] to cycle"))
	lines = append(lines, "")

	for k := proxyKind(0); k < proxyKindCount; k++ {
		sel := "  "
		style := mutedStyle
		if k == g.proxy {
			sel = amberStyle.Render("▶ ")
			style = labelStyle
		}
		lines = append(lines, sel+style.Render(k.String()))
	}

	return strings.Join(lines, "\n")
}

func (g TLSGenerator) ViewConfig() string {
	return g.configVP.View()
}

func (g TLSGenerator) ViewStatusBar() string {
	if g.saveErr != "" && time.Since(g.savedAt) < 8*time.Second {
		return "  " + redStyle.Render("✕ save failed: ") + brightStyle.Render(g.saveErr)
	}
	if g.savedPath != "" && time.Since(g.savedAt) < 5*time.Second {
		return "  " + greenStyle.Render("✓ saved → "+g.savedPath)
	}
	return "  " + mutedStyle.Render("proxy: ") + labelStyle.Render(g.proxy.String()) +
		"   " + mutedStyle.Render(fmt.Sprintf("file: %s", g.proxy.FileExt()))
}

func (g TLSGenerator) ViewFooter() string {
	keys := strings.Join([]string{
		keyBadge("↑↓") + " field",
		keyBadge("tab") + " proxy",
		keyBadge("pgup/dn") + " scroll",
		keyBadge("ctrl+s") + " save file",
		keyBadge("esc") + " back",
	}, "   ")
	return footerStyle.Render(keys)
}

// ── config generators ─────────────────────────────────────────────────────

func (g TLSGenerator) generateConfig() string {
	domain := g.fields[0].value
	if domain == "" {
		domain = g.fields[0].placeholder
	}
	upstream := g.fields[1].value
	if upstream == "" {
		upstream = g.fields[1].placeholder
	}
	certFile := g.fields[2].value
	keyFile := g.fields[3].value

	switch g.proxy {
	case proxyCaddy:
		return generateCaddy(domain, upstream)
	case proxyNginx:
		return generateNginx(domain, upstream, certFile, keyFile)
	case proxyEnvoy:
		upstreamHost, upstreamPort := splitHostPort(upstream)
		return generateEnvoy(domain, upstreamHost, upstreamPort, certFile, keyFile)
	}
	return ""
}

func generateCaddy(domain, upstream string) string {
	return fmt.Sprintf(`# Pintail — Caddyfile
# Caddy handles TLS automatically (Let's Encrypt or ZeroSSL).
#
# The Quack server speaks plain HTTP only — HTTP/1.1 with keep-alive, no HTTP/2 —
# so the upstream is pinned to 1.1. Forcing cleartext HTTP/2 upstream here would
# break every request through the proxy.

%s {
    reverse_proxy %s {
        # Quack streams results back as repeated FETCH responses. Buffering
        # them in Caddy defeats the streaming and inflates memory; -1 flushes
        # immediately (the equivalent of nginx proxy_buffering off).
        flush_interval -1

        # Quack keeps server-side connection state alive on the persistent
        # HTTP connection, so keep-alive upstream is correctness, not just speed.
        transport http {
            versions 1.1
            keepalive 30s
        }
    }

    # PREPARE carries SQL and APPEND carries inserted DataChunks, so request
    # bodies are far larger than the default cap allows.
    request_body {
        max_size 256MB
    }

    # Optional: tighten TLS
    tls {
        protocols tls1.2 tls1.3
    }
}

# Redirect plain HTTP → HTTPS (Caddy does this automatically,
# but explicit for clarity)
http://%s {
    redir https://{host}{uri} permanent
}
`, domain, upstream, domain)
}

func generateNginx(domain, upstream, certFile, keyFile string) string {
	return fmt.Sprintf(`# Pintail — Nginx config
# The Quack server speaks HTTP/1.1 with keep-alive, so proxy_pass with
# proxy_http_version 1.1 is all that is needed upstream. "http2 on" below is
# client-facing only and unrelated to the upstream protocol.

upstream quack_backend {
    server %s;
    keepalive 32;
}

server {
    listen 443 ssl;
    http2  on;
    server_name %s;

    ssl_certificate     %s;
    ssl_certificate_key %s;
    ssl_protocols       TLSv1.2 TLSv1.3;
    ssl_ciphers         ECDHE-ECDSA-AES128-GCM-SHA256:ECDHE-RSA-AES128-GCM-SHA256:ECDHE-ECDSA-AES256-GCM-SHA384;
    ssl_prefer_server_ciphers off;
    ssl_session_cache   shared:SSL:10m;
    ssl_session_timeout 1d;

    # PREPARE carries SQL and APPEND carries inserted DataChunks; the 1 MiB
    # nginx default fails mid-INSERT.
    client_max_body_size 256M;

    # HSTS (optional — enable after verifying TLS works)
    # add_header Strict-Transport-Security "max-age=63072000" always;

    location / {
        proxy_pass         http://quack_backend;
        proxy_http_version 1.1;

        # Keep-alive to the upstream: an empty Connection header stops nginx
        # sending "close" on every request. Quack has no websocket upgrade, so
        # no Upgrade header is forwarded.
        proxy_set_header   Connection "";
        proxy_set_header   Host       $host;
        proxy_set_header   X-Real-IP  $remote_addr;

        # Forward the Quack auth token as-is
        proxy_pass_header  Authorization;

        # Quack streams results via repeated FETCH responses; buffering through
        # nginx defeats the streaming and inflates memory.
        proxy_buffering off;

        # A long-running query can sit on the wire between FETCHes for minutes,
        # well past nginx's 60s default.
        proxy_read_timeout  600s;
        proxy_send_timeout  600s;
        proxy_connect_timeout 5s;
    }
}

server {
    listen 80;
    server_name %s;
    return 301 https://$host$request_uri;
}
`, upstream, domain, certFile, keyFile, domain)
}

func generateEnvoy(domain, upstreamHost, upstreamPort, certFile, keyFile string) string {
	return fmt.Sprintf(`# Pintail — Envoy config (static bootstrap)
# Envoy version >= 1.20 recommended.
# Save as envoy.yaml and run:
#   envoy -c envoy.yaml

static_resources:

  listeners:
    - name: quack_tls_listener
      address:
        socket_address: { address: 0.0.0.0, port_value: 443 }
      per_connection_buffer_limit_bytes: 268435456
      filter_chains:
        - transport_socket:
            name: envoy.transport_sockets.tls
            typed_config:
              "@type": type.googleapis.com/envoy.extensions.transport_sockets.tls.v3.DownstreamTlsContext
              common_tls_context:
                tls_certificates:
                  - certificate_chain: { filename: "%s" }
                    private_key:       { filename: "%s" }
                tls_params:
                  tls_minimum_protocol_version: TLSv1_2
          filters:
            - name: envoy.filters.network.http_connection_manager
              typed_config:
                "@type": type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager
                stat_prefix: quack_ingress
                codec_type:  HTTP2
                route_config:
                  name: quack_route
                  virtual_hosts:
                    - name: quack_service
                      domains: ["%s"]
                      routes:
                        - match: { prefix: "/" }
                          route:
                            cluster: quack_cluster
                            # Envoy's default route timeout is 15s, which cuts
                            # off a query still streaming FETCH responses.
                            timeout: 600s
                http_filters:
                  - name: envoy.filters.http.router
                    typed_config:
                      "@type": type.googleapis.com/envoy.extensions.filters.http.router.v3.Router

  clusters:
    - name: quack_cluster
      type: LOGICAL_DNS
      dns_lookup_family: V4_ONLY
      lb_policy: ROUND_ROBIN
      # PREPARE / APPEND bodies are much larger than the 1 MiB default buffer.
      per_connection_buffer_limit_bytes: 268435456
      # HTTP/1.1 upstream: the Quack server is cpp-httplib and does not speak
      # HTTP/2, so the explicit config selects the HTTP/1.1 protocol options.
      # Selecting the HTTP/2 ones here would break every request.
      typed_extension_protocol_options:
        envoy.extensions.upstreams.http.v3.HttpProtocolOptions:
          "@type": type.googleapis.com/envoy.extensions.upstreams.http.v3.HttpProtocolOptions
          explicit_http_config:
            http_protocol_options: {}
      load_assignment:
        cluster_name: quack_cluster
        endpoints:
          - lb_endpoints:
              - endpoint:
                  address:
                    socket_address:
                      address:    %s
                      port_value: %s

admin:
  address:
    socket_address: { address: 127.0.0.1, port_value: 9901 }
`, certFile, keyFile, domain, upstreamHost, upstreamPort)
}

// ── file export ───────────────────────────────────────────────────────────

func (g TLSGenerator) saveToFile() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot locate home directory: %w", err)
	}
	dir := filepath.Join(home, ".duckdb", "tls")
	if err := os.MkdirAll(dir, 0750); err != nil {
		return "", err
	}

	name := fmt.Sprintf("pintail-%s", g.proxy.FileExt())
	path := filepath.Join(dir, name)

	content := g.generateConfig()
	if err := os.WriteFile(path, []byte(content), 0640); err != nil {
		return "", err
	}
	return path, nil
}

// ── helpers ───────────────────────────────────────────────────────────────

func splitHostPort(addr string) (string, string) {
	idx := strings.LastIndex(addr, ":")
	if idx < 0 {
		return addr, "9494"
	}
	return addr[:idx], addr[idx+1:]
}
