package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// ── view enum ─────────────────────────────────────────────────────────────

type appView int

const (
	viewDashboard appView = iota
	viewTokens
	viewScratchpad
	viewAddServer
	viewTLS
	viewAuth
	viewSnapshots
)

type panel int

const (
	panelConnections panel = iota
	panelCatalog
)

// ── tickers ───────────────────────────────────────────────────────────────
//
// Three independent cadences keep subprocess spawning under control:
//   • pingTick    (5s)  — cheap TCP reachability check, no subprocess
//   • sessionTick (15s) — refresh live sessions for ONLINE servers only
//   • catalog is fetched once on each offline→online transition, not on a timer

type pingTickMsg time.Time
type sessionTickMsg time.Time

func pingTickCmd() tea.Cmd {
	return tea.Tick(5*time.Second, func(t time.Time) tea.Msg {
		return pingTickMsg(t)
	})
}

func sessionTickCmd() tea.Cmd {
	return tea.Tick(15*time.Second, func(t time.Time) tea.Msg {
		return sessionTickMsg(t)
	})
}

// ── add-server form ───────────────────────────────────────────────────────
//
// The form has a Type selector at the top (cycled with space / ←→) which
// decides which input fields appear below. All possible field values are
// kept on the struct so cycling Type doesn't lose what the user already typed.

type addServerForm struct {
	connType ConnType
	focusIdx int // -1 = type selector focused; -2 = connection list focused; ≥0 = field index
	// editingIdx >= 0 means we're editing m.configs[editingIdx] instead of adding
	editingIdx int

	// All possible values; only some matter per connType.
	name        string
	host        string
	port        string
	token       string
	tls         string // "y"/"n"
	path        string
	catalogPath string
	catalogRef  string
	storagePath string
	secretRef   string // storage_secret_ref — applies to local + ducklake

	// Index of currently-selected row in the connection list, for edit/delete.
	listCursor int

	// Why the last save attempt was refused, shown under the form. Enter used
	// to do nothing at all when the form wasn't saveable, which looked like a
	// broken key.
	errMsg string
}

type formField struct {
	label       string
	value       *string
	placeholder string
	hint        string
}

// ── path-completion helpers ───────────────────────────────────────────────
//
// These power the tab-to-complete behaviour on path-style fields in the
// add-connection form. Shell-style: tab completes to the longest common
// prefix among matching files; once the value matches an existing path
// (no further completion possible), tab falls through to the normal
// "advance to next field" behaviour.

// isPathField reports whether a form-field label represents a filesystem
// path that should support tab completion. Kept as a small allowlist
// rather than a struct flag to avoid threading new state through the
// visibleFields() return type.
func isPathField(label string) bool {
	switch label {
	case "Path", "Catalog path", "Storage":
		return true
	}
	return false
}

// completePath expands ~ and returns either the same partial path (no
// matches), the unique match (single match), or the longest common prefix
// of all matches. The returned value is what should replace the field's
// current contents.
func completePath(partial string) string {
	expanded := partial
	if strings.HasPrefix(expanded, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			expanded = filepath.Join(home, expanded[2:])
		}
	}
	matches, err := filepath.Glob(expanded + "*")
	if err != nil || len(matches) == 0 {
		return partial
	}
	if len(matches) == 1 {
		// Append a trailing slash for directories to make the next tab
		// drill in. Shell convention.
		if info, err := os.Stat(matches[0]); err == nil && info.IsDir() {
			return matches[0] + string(filepath.Separator)
		}
		return matches[0]
	}
	// Longest common prefix among multiple matches.
	lcp := matches[0]
	for _, m := range matches[1:] {
		lcp = longestCommonPrefix(lcp, m)
	}
	return lcp
}

func longestCommonPrefix(a, b string) string {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return a[:i]
		}
	}
	return a[:n]
}

func newAddServerForm() *addServerForm {
	return &addServerForm{
		connType:   ConnQuack,
		focusIdx:   -1,
		editingIdx: -1,
		port:       "9494",
		tls:        "n",
	}
}

// formFromConfig builds a form pre-populated for editing an existing config.
func formFromConfig(cfg ServerConfig, idx int) *addServerForm {
	f := &addServerForm{
		connType:    cfg.Type,
		focusIdx:    0,
		editingIdx:  idx,
		name:        cfg.Name,
		host:        cfg.Host,
		port:        fmt.Sprintf("%d", cfg.Port),
		token:       cfg.Token,
		path:        cfg.Path,
		catalogPath: cfg.CatalogPath,
		catalogRef:  cfg.CatalogRef,
		storagePath: cfg.StoragePath,
		secretRef:   cfg.StorageSecretRef,
	}
	if cfg.Port == 0 {
		f.port = "9494"
	}
	if cfg.TLS {
		f.tls = "y"
	} else {
		f.tls = "n"
	}
	if f.connType == "" {
		f.connType = ConnQuack
	}
	return f
}

// visibleFields returns the editable fields for the currently-selected type.
//
// The per-type field sets stay as type-dispatch — they're different fields
// with different meanings for each backend. The Storage secret field is the
// one cross-cutting concern: any type with CapStorageSecrets gets it appended.
func (f *addServerForm) visibleFields() []formField {
	var base []formField
	switch f.connType {
	case ConnLocal:
		base = []formField{
			{"Name", &f.name, "e.g. local-dev", "logical name for this connection"},
			{"Path", &f.path, "/path/to/database.duckdb", "absolute path; or remote URI like s3://bucket/db.duckdb"},
		}
	case ConnDuckLake:
		base = []formField{
			{"Name", &f.name, "e.g. lake-prod", "logical name for this connection"},
			{"Catalog ref", &f.catalogRef, "name of another connection (preferred)", "use a configured connection as catalog — overrides Catalog path"},
			{"Catalog path", &f.catalogPath, "postgres://… · sqlite:///… · ./catalog.duckdb", "freeform catalog URL or path (used when Catalog ref is empty)"},
			{"Storage", &f.storagePath, "s3://bucket/lake or /mnt/lake", "object storage root (DATA_PATH)"},
		}
	default: // ConnQuack
		base = []formField{
			{"Name", &f.name, "e.g. prod-quack", "logical name for this connection"},
			{"Host", &f.host, "localhost or IP", "Quack server hostname"},
			{"Port", &f.port, "9494", "Quack server port"},
			{"Token", &f.token, "shared master token", "leave blank if server has no auth"},
			{"TLS", &f.tls, "n", "y for HTTPS, n for HTTP"},
		}
	}

	// Cross-cutting field: any backend with CapStorageSecrets gets the ref field.
	probe := ServerConfig{Type: f.connType}
	if probe.Supports(CapStorageSecrets) {
		hint := "name of a Storage secret — manage these on the token screen"
		if f.connType == ConnLocal {
			hint = "name of a Storage secret — needed when path is a remote URI"
		}
		base = append(base, formField{
			"Storage secret", &f.secretRef,
			"  (optional)", hint,
		})
	}
	return base
}

// toConfig builds a ServerConfig from the current form state.
func (f *addServerForm) toConfig() ServerConfig {
	port := 9494
	fmt.Sscanf(f.port, "%d", &port)
	tls := strings.ToLower(strings.TrimSpace(f.tls)) == "y"
	return ServerConfig{
		Name:             strings.TrimSpace(f.name),
		Type:             f.connType,
		Host:             strings.TrimSpace(f.host),
		Port:             port,
		Token:            f.token,
		TLS:              tls,
		Path:             strings.TrimSpace(f.path),
		CatalogPath:      strings.TrimSpace(f.catalogPath),
		CatalogRef:       strings.TrimSpace(f.catalogRef),
		StoragePath:      strings.TrimSpace(f.storagePath),
		StorageSecretRef: strings.TrimSpace(f.secretRef),
	}
}

// valid returns whether the form has the minimum required fields filled in.
func (f *addServerForm) valid() bool {
	if strings.TrimSpace(f.name) == "" {
		return false
	}
	switch f.connType {
	case ConnLocal:
		return f.path != ""
	case ConnDuckLake:
		return f.storagePath != "" && (f.catalogPath != "" || f.catalogRef != "")
	default:
		return f.host != ""
	}
}

// problem returns the reason this form cannot be saved, or "" when it can.
//
// Beyond required fields, this catches the references that used to be accepted
// and then failed silently at query time: a duplicate name (name lookup finds
// the first match, so the second connection is unreachable by catalog_ref, the
// CLI subcommands, and the scratchpad target list), a catalog_ref or
// storage_secret_ref pointing at something that doesn't exist, and a DuckLake
// naming itself as its own catalog.
func (f *addServerForm) problem(configs []ServerConfig, secrets []StorageSecret) string {
	name := strings.TrimSpace(f.name)
	if name == "" {
		return "name is required"
	}

	switch f.connType {
	case ConnLocal:
		if strings.TrimSpace(f.path) == "" {
			return "path is required for a local connection"
		}
	case ConnDuckLake:
		if strings.TrimSpace(f.storagePath) == "" {
			return "storage path is required for a DuckLake connection"
		}
		if strings.TrimSpace(f.catalogPath) == "" && strings.TrimSpace(f.catalogRef) == "" {
			return "a DuckLake connection needs either a catalog ref or a catalog path"
		}
	default:
		if strings.TrimSpace(f.host) == "" {
			return "host is required for a Quack connection"
		}
	}

	for i, cfg := range configs {
		if i == f.editingIdx {
			continue // editing this one; its own name is not a clash
		}
		if strings.EqualFold(cfg.Name, name) {
			return fmt.Sprintf("a connection named %q already exists", cfg.Name)
		}
	}

	if ref := strings.TrimSpace(f.catalogRef); ref != "" && f.connType == ConnDuckLake {
		if strings.EqualFold(ref, name) {
			return "a DuckLake cannot be its own catalog"
		}
		if !hasConfigNamed(configs, ref) {
			return fmt.Sprintf("no connection named %q to use as the catalog", ref)
		}
	}

	if ref := strings.TrimSpace(f.secretRef); ref != "" {
		if !hasSecretNamed(secrets, ref) {
			return fmt.Sprintf("no storage secret named %q — create it on the tokens screen", ref)
		}
	}

	return ""
}

func hasConfigNamed(configs []ServerConfig, name string) bool {
	for _, cfg := range configs {
		if cfg.Name == name {
			return true
		}
	}
	return false
}

func hasSecretNamed(secrets []StorageSecret, name string) bool {
	for _, s := range secrets {
		if s.Name == name {
			return true
		}
	}
	return false
}

// ── per-connection metadata ───────────────────────────────────────────────

// connData is the last metadata fetched for one connection. Errors are kept
// alongside the data rather than replacing it: a failed refresh should say so
// without discarding the last known-good listing.
type connData struct {
	sessions      []Connection
	catalog       []CatalogSchema
	reportedCount string // connection count as reported by the backend
	sessionErr    string
	catalogErr    string
	sessionsAt    time.Time
	catalogAt     time.Time
}

// syncConnData resizes the per-connection metadata to match the connection
// list and keeps the selection in range — connections can be added and deleted
// while fetches for the old indices are still in flight.
func (m *Model) syncConnData() {
	if len(m.data) > len(m.configs) {
		m.data = m.data[:len(m.configs)]
	}
	for len(m.data) < len(m.configs) {
		m.data = append(m.data, connData{})
	}
	if m.selected >= len(m.configs) {
		m.selected = len(m.configs) - 1
	}
	if m.selected < 0 {
		m.selected = 0
	}
}

// removeConnData drops the metadata for a deleted connection so the remaining
// entries stay aligned with the connections they describe.
func (m *Model) removeConnData(idx int) {
	if idx < 0 || idx >= len(m.data) {
		return
	}
	m.data = append(m.data[:idx], m.data[idx+1:]...)
}

// selectedData returns the metadata for the connection the dashboard is
// showing. The second return is false when there is nothing to show.
func (m Model) selectedData() (connData, bool) {
	if m.selected < 0 || m.selected >= len(m.data) {
		return connData{}, false
	}
	return m.data[m.selected], true
}

// selectedName is the display name of the selected connection, or "" if none.
func (m Model) selectedName() string {
	if m.selected < 0 || m.selected >= len(m.configs) {
		return ""
	}
	return m.configs[m.selected].Name
}

// selectConnection moves the dashboard to a connection by index, ignoring
// out-of-range requests (the digit keys are direct-dial, so 9 on a two-server
// setup should do nothing rather than jump).
func (m *Model) selectConnection(idx int) {
	if idx >= 0 && idx < len(m.configs) {
		m.selected = idx
		m.connTable.SetRows(connectionRows(m.data[idx].sessions))
		m.connTable.GotoTop()
	}
}

// cycleConnection steps the selection by delta, wrapping.
func (m *Model) cycleConnection(delta int) {
	if len(m.configs) == 0 {
		return
	}
	next := (m.selected + delta + len(m.configs)) % len(m.configs)
	m.selectConnection(next)
}

// ── Model ─────────────────────────────────────────────────────────────────

type Model struct {
	width  int
	height int

	currentView appView

	// real server clients (config + live state)
	clients        []*QuackClient
	configs        []ServerConfig  // kept in sync with clients
	storageSecrets []StorageSecret // referenced via Config.StorageSecretRef

	// per-client previous online state, for transition detection
	wasOnline []bool

	// dashboard
	focus     panel
	connTable table.Model
	tick      int

	// Metadata per connection, index-aligned with clients/configs, and which
	// one the dashboard panels are showing. This used to be a single global
	// set of sessions and catalog, so with several servers online the last
	// responder overwrote everyone else and nothing on screen said whose data
	// you were looking at.
	data     []connData
	selected int

	// token manager
	tokenMgr TokenManager

	// scratchpad
	scratchpad Scratchpad

	// auth policy editor
	authEditor AuthEditor

	// ducklake snapshots
	snapshots SnapshotsView

	// tls config generator
	tlsGen TLSGenerator

	// add-server form
	addForm *addServerForm
}

func NewModel() Model {
	configs := LoadServerConfigs()
	secrets := LoadStorageSecrets()
	clients := InitClients(configs, secrets)

	servers := make([]ServerInfo, len(configs))
	for i, cfg := range configs {
		servers[i] = cfg.ToServerInfo()
	}

	m := Model{
		clients:        clients,
		configs:        configs,
		storageSecrets: secrets,
		wasOnline:      make([]bool, len(clients)),
		data:           make([]connData, len(configs)),
		focus:          panelConnections,
		currentView:    viewDashboard,
		tokenMgr:       NewTokenManager(),
		scratchpad:     NewScratchpad(servers, clients),
		tlsGen:         NewTLSGenerator(configs),
		authEditor:     NewAuthEditor(LoadTokens(), clients),
		snapshots:      NewSnapshotsView(clients),
	}
	m.connTable = buildConnectionTable(nil)
	return m
}

func buildConnectionTable(conns []Connection) table.Model {
	cols := []table.Column{
		{Title: "ID", Width: 4},
		{Title: "IP Address", Width: 15},
		{Title: "Identity", Width: 16},
		{Title: "Catalog", Width: 12},
		{Title: "Duration", Width: 8},
		{Title: "Queries", Width: 8},
		{Title: "Status", Width: 10},
	}
	t := table.New(
		table.WithColumns(cols),
		table.WithRows(connectionRows(conns)),
		table.WithFocused(true),
		table.WithHeight(8),
	)
	s := table.DefaultStyles()
	s.Header = s.Header.
		BorderStyle(lipgloss.NormalBorder()).
		BorderForeground(colorPanelBorder).
		BorderBottom(true).Bold(true).
		Foreground(colorDuckYellow)
	s.Selected = s.Selected.
		Foreground(colorDarkBg).Background(colorDuckYellow).Bold(false)
	t.SetStyles(s)
	return t
}

// ── Init ──────────────────────────────────────────────────────────────────

func (m Model) Init() tea.Cmd {
	// Start data ticker + ping ticker + session ticker + initial ping
	cmds := []tea.Cmd{tickCmd(), pingTickCmd(), sessionTickCmd()}
	for i, c := range m.clients {
		cmds = append(cmds, pingServerCmd(c, i))
	}
	return tea.Batch(cmds...)
}

// ── Update ────────────────────────────────────────────────────────────────

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd

	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		tableH := m.height - 15
		if tableH < 2 {
			tableH = 2
		}
		m.connTable.SetHeight(tableH)
		m.scratchpad.Resize(m.width, m.height)

	// ── ping result: detect offline→online transition ─────────────────────
	case pingResultMsg:
		if msg.idx >= len(m.clients) {
			return m, nil
		}
		nowOnline := msg.err == nil
		justConnected := nowOnline && msg.idx < len(m.wasOnline) && !m.wasOnline[msg.idx]
		if msg.idx < len(m.wasOnline) {
			m.wasOnline[msg.idx] = nowOnline
		}
		// Only on a fresh connection do we pull catalog + sessions (catalog is
		// relatively static, so this avoids re-fetching it on every ping).
		if justConnected && m.clients[msg.idx].HasCLI() {
			c := m.clients[msg.idx]
			return m, tea.Batch(c.FetchSessionsCmd(msg.idx), c.FetchCatalogCmd(msg.idx))
		}
		return m, nil

	// ── ping ticker: cheap TCP check for every server ──────────────────────
	case pingTickMsg:
		cmds := []tea.Cmd{pingTickCmd()}
		for i, c := range m.clients {
			cmds = append(cmds, pingServerCmd(c, i))
		}
		return m, tea.Batch(cmds...)

	// ── session ticker: refresh sessions for ONLINE servers only ───────────
	case sessionTickMsg:
		cmds := []tea.Cmd{sessionTickCmd()}
		for i, c := range m.clients {
			if c.GetState().Online && c.HasCLI() {
				cmds = append(cmds, c.FetchSessionsCmd(i))
			}
		}
		return m, tea.Batch(cmds...)

	// ── live session results ───────────────────────────────────────────────
	case sessionResultMsg:
		// A result can arrive after its connection was deleted, in which case
		// the index no longer refers to the server that produced it.
		if msg.idx < 0 || msg.idx >= len(m.data) {
			return m, nil
		}
		d := &m.data[msg.idx]
		d.sessionsAt = time.Now()
		if msg.err != nil {
			d.sessionErr = msg.err.Error()
			return m, nil
		}
		// The result is applied even when it is empty, so rows that no longer
		// exist stop being shown as though they were live.
		d.sessionErr = ""
		d.reportedCount = msg.reportedCount
		d.sessions = msg.connections
		if msg.idx == m.selected {
			m.connTable.SetRows(connectionRows(d.sessions))
		}
		return m, nil

	// ── live catalog results ───────────────────────────────────────────────
	case catalogResultMsg:
		if msg.idx < 0 || msg.idx >= len(m.data) {
			return m, nil
		}
		d := &m.data[msg.idx]
		d.catalogAt = time.Now()
		if msg.err != nil {
			d.catalogErr = msg.err.Error()
			return m, nil
		}
		d.catalogErr = ""
		d.catalog = msg.catalog
		return m, nil

	// ── data ticker ───────────────────────────────────────────────────────
	case tickMsg:
		m.tick++
		var tmCmd tea.Cmd
		m.tokenMgr, tmCmd = m.tokenMgr.Update(msg)
		var authCmd tea.Cmd
		m.authEditor, authCmd = m.authEditor.Update(msg)
		return m, tea.Batch(tickCmd(), tmCmd, authCmd)

	// ── query result (scratchpad async) ───────────────────────────────────
	case queryResultMsg:
		var spCmd tea.Cmd
		m.scratchpad, spCmd = m.scratchpad.Update(msg)
		return m, spCmd

	// ── snapshot results (ducklake) ───────────────────────────────────────
	case snapshotsResultMsg:
		var snapCmd tea.Cmd
		m.snapshots, snapCmd = m.snapshots.Update(msg)
		return m, snapCmd

	// ── auth policy apply result ──────────────────────────────────────────
	case authApplyResultMsg:
		var authCmd tea.Cmd
		m.authEditor, authCmd = m.authEditor.Update(msg)
		return m, authCmd

	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c":
			// While a query is in flight, ctrl+c interrupts it rather than
			// quitting — the psql convention, and previously the only way to
			// escape a slow query was to kill the whole app.
			if m.currentView == viewScratchpad && m.scratchpad.Running() {
				var spCmd tea.Cmd
				m.scratchpad, spCmd = m.scratchpad.Update(msg)
				return m, spCmd
			}
			return m, tea.Quit
		}

		switch m.currentView {

		case viewDashboard:
			switch msg.String() {
			case "q":
				return m, tea.Quit
			case "t":
				m.currentView = viewTokens
				return m, nil
			case "s":
				m.currentView = viewScratchpad
				m.scratchpad.Resize(m.width, m.height)
				return m, nil
			case "a":
				m.addForm = newAddServerForm()
				m.currentView = viewAddServer
				return m, nil
			case "p":
				m.authEditor = NewAuthEditor(m.tokenMgr.tokens, m.clients)
				m.currentView = viewAuth
				return m, nil
			case "l":
				// Rebuild from current clients (configs may have changed)
				m.snapshots = NewSnapshotsView(m.clients)
				m.currentView = viewSnapshots
				if m.snapshots.HasLake() {
					m.snapshots.loading = true
					return m, m.snapshots.FetchCmd()
				}
				return m, nil
			case "x":
				m.tlsGen.SetWidth(m.width)
				m.currentView = viewTLS
				return m, nil
			case "tab", "shift+tab":
				if m.focus == panelConnections {
					m.focus = panelCatalog
					m.connTable.Blur()
				} else {
					m.focus = panelConnections
					m.connTable.Focus()
				}
				return m, nil
			case "r":
				// Refresh every online connection. Each result is attributed to
				// the connection that produced it, so this no longer races.
				var cmds []tea.Cmd
				for i, c := range m.clients {
					if c.GetState().Online && c.HasCLI() {
						cmds = append(cmds, c.FetchSessionsCmd(i), c.FetchCatalogCmd(i))
					}
				}
				return m, tea.Batch(cmds...)

			// Which connection the panels describe. ] / [ cycle, and the digits
			// dial one directly.
			case "]", "}":
				m.cycleConnection(1)
				return m, nil
			case "[", "{":
				m.cycleConnection(-1)
				return m, nil
			case "1", "2", "3", "4", "5", "6", "7", "8", "9":
				m.selectConnection(int(msg.String()[0] - '1'))
				return m, nil
			}

		case viewTokens:
			if msg.String() == "esc" &&
				m.tokenMgr.form == nil && !m.tokenMgr.rotateConfirm && !m.tokenMgr.revokeConfirm &&
				m.tokenMgr.secretForm == nil && !m.tokenMgr.secretDelConfirm {
				// Sync any storage-secret edits back to the model and rebuild
				// clients so their resolvers see the latest secret values.
				if !storageSecretsEqual(m.storageSecrets, m.tokenMgr.secrets) {
					m.storageSecrets = append([]StorageSecret(nil), m.tokenMgr.secrets...)
					m.clients = InitClients(m.configs, m.storageSecrets)
				}
				m.currentView = viewDashboard
				return m, nil
			}
			var tmCmd tea.Cmd
			m.tokenMgr, tmCmd = m.tokenMgr.Update(msg)
			return m, tmCmd

		case viewScratchpad:
			if msg.String() == "esc" {
				// esc cancels a running query before it leaves the screen, so
				// there is an interrupt that carries no risk of quitting.
				if m.scratchpad.Running() {
					var spCmd tea.Cmd
					m.scratchpad, spCmd = m.scratchpad.Update(msg)
					return m, spCmd
				}
				m.currentView = viewDashboard
				return m, nil
			}
			var spCmd tea.Cmd
			m.scratchpad, spCmd = m.scratchpad.Update(msg)
			return m, spCmd

		case viewTLS:
			if msg.String() == "esc" {
				m.currentView = viewDashboard
				return m, nil
			}
			var tlsCmd tea.Cmd
			m.tlsGen, tlsCmd = m.tlsGen.Update(msg)
			return m, tlsCmd

		case viewAuth:
			if msg.String() == "esc" {
				// Write permission toggles back to the tokens they came from,
				// and persist. Leaving this screen used to discard every edit.
				if m.authEditor.Dirty() {
					m.applyAuthEdits()
				}
				m.currentView = viewDashboard
				return m, nil
			}
			var authCmd tea.Cmd
			m.authEditor, authCmd = m.authEditor.Update(msg)
			return m, authCmd

		case viewSnapshots:
			if msg.String() == "esc" {
				m.currentView = viewDashboard
				return m, nil
			}
			var snapCmd tea.Cmd
			m.snapshots, snapCmd = m.snapshots.Update(msg)
			return m, snapCmd

		case viewAddServer:
			return m.updateAddServer(msg)
		}
	}

	if m.currentView == viewDashboard && m.focus == panelConnections {
		m.connTable, cmd = m.connTable.Update(msg)
	}
	return m, cmd
}

// updateAddServer handles key input on the connection manager form.
//
// Focus modes:
//
//	-2 = connection list (left panel)  — supports e (edit), d (delete), ↑↓
//	-1 = type selector at top of form  — ←→ / space cycles
//	 0..N-1 = visible form field index — typed input
func (m Model) updateAddServer(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	f := m.addForm
	visible := f.visibleFields()

	// ── list-focus mode ───────────────────────────────────────────────────
	if f.focusIdx == -2 {
		switch msg.String() {
		case "esc":
			m.addForm = nil
			m.currentView = viewDashboard
			return m, nil
		case "tab", "right":
			f.focusIdx = -1
			return m, nil
		case "up", "k":
			if f.listCursor > 0 {
				f.listCursor--
			}
			return m, nil
		case "down", "j":
			if f.listCursor < len(m.configs)-1 {
				f.listCursor++
			}
			return m, nil
		case "e":
			if f.listCursor < len(m.configs) {
				m.addForm = formFromConfig(m.configs[f.listCursor], f.listCursor)
			}
			return m, nil
		case "d":
			if f.listCursor < len(m.configs) && len(m.configs) > 1 {
				m.configs = append(m.configs[:f.listCursor], m.configs[f.listCursor+1:]...)
				if f.listCursor < len(m.wasOnline) {
					m.wasOnline = append(m.wasOnline[:f.listCursor], m.wasOnline[f.listCursor+1:]...)
				}
				// Drop this connection's metadata so the remaining entries stay
				// aligned with the connections they describe.
				m.removeConnData(f.listCursor)
				m.syncConnData()
				m.selectConnection(m.selected)
				m.clients = InitClients(m.configs, m.storageSecrets)
				servers := make([]ServerInfo, len(m.configs))
				for i, c := range m.configs {
					servers[i] = c.ToServerInfo()
				}
				m.scratchpad.SetTargets(servers, m.clients)
				SaveServerConfigs(m.configs)
				if f.listCursor >= len(m.configs) && f.listCursor > 0 {
					f.listCursor--
				}
			}
			return m, nil
		}
		return m, nil
	}

	// ── form-focus mode (type selector + fields) ──────────────────────────
	switch msg.String() {
	case "esc":
		// Return to the list view rather than exiting the whole screen.
		// Cancels any in-progress edit / new connection but preserves
		// which row in the list the user had highlighted.
		prevCursor := f.listCursor
		fresh := newAddServerForm()
		fresh.focusIdx = -2
		fresh.listCursor = prevCursor
		m.addForm = fresh
		return m, nil

	case "tab", "down":
		visible := f.visibleFields()
		// Path-completion: on a path-like field, tab first tries to complete
		// the partial path via glob. Only advances to the next field when the
		// value has no further completion available.
		if f.focusIdx >= 0 && f.focusIdx < len(visible) {
			fld := visible[f.focusIdx]
			if isPathField(fld.label) && *fld.value != "" {
				completed := completePath(*fld.value)
				if completed != *fld.value {
					*fld.value = completed
					return m, nil
				}
			}
		}
		// Advance — wrap from the last field back to the list panel.
		if f.focusIdx < len(visible)-1 {
			f.focusIdx++
		} else {
			f.focusIdx = -2
		}
		return m, nil

	case "shift+tab", "up":
		if f.focusIdx > -1 {
			f.focusIdx--
		}
		return m, nil

	case "left":
		if f.focusIdx == -1 {
			// Cycle type backwards
			for i := 0; i < len(AllConnTypes)-1; i++ {
				f.connType = f.connType.Next()
			}
		} else if f.focusIdx == 0 {
			// From first field, ←  jumps to the connection list
			f.focusIdx = -2
		}
		return m, nil

	case "shift+left":
		// Cycle type backwards from anywhere in the form (any field, type row,
		// or even the connection list). Clamps focus to a valid field for the
		// new type so we don't end up out-of-bounds when types have different
		// field counts.
		for i := 0; i < len(AllConnTypes)-1; i++ {
			f.connType = f.connType.Next()
		}
		if f.focusIdx >= 0 {
			if visible := f.visibleFields(); f.focusIdx >= len(visible) {
				f.focusIdx = len(visible) - 1
			}
		}
		return m, nil

	case "shift+right":
		// Cycle type forward from anywhere in the form.
		f.connType = f.connType.Next()
		if f.focusIdx >= 0 {
			if visible := f.visibleFields(); f.focusIdx >= len(visible) {
				f.focusIdx = len(visible) - 1
			}
		}
		return m, nil

	case "right":
		// Only meaningful on the type row. On a text field it used to append a
		// space, so pressing → while editing silently corrupted the value.
		if f.focusIdx == -1 {
			f.connType = f.connType.Next()
		}
		return m, nil

	case " ":
		if f.focusIdx == -1 {
			f.connType = f.connType.Next()
			return m, nil
		}
		if f.focusIdx >= 0 && f.focusIdx < len(visible) {
			fld := visible[f.focusIdx]
			*fld.value += " "
		}
		return m, nil

	case "enter":
		if f.focusIdx == -1 {
			f.focusIdx = 0
			return m, nil
		}
		if f.focusIdx < len(visible)-1 {
			f.focusIdx++
			return m, nil
		}
		if problem := f.problem(m.configs, m.storageSecrets); problem != "" {
			f.errMsg = problem
			return m, nil
		}
		f.errMsg = ""
		cfg := f.toConfig()
		if f.editingIdx >= 0 && f.editingIdx < len(m.configs) {
			m.configs[f.editingIdx] = cfg
		} else {
			m.configs = append(m.configs, cfg)
			m.wasOnline = append(m.wasOnline, false)
		}
		// Rebuild all clients so the shared resolver sees the latest configs.
		// This matters for DuckLake configs that reference others by name.
		m.clients = InitClients(m.configs, m.storageSecrets)
		m.syncConnData()

		servers := make([]ServerInfo, len(m.configs))
		for i, c := range m.configs {
			servers[i] = c.ToServerInfo()
		}
		m.scratchpad.SetTargets(servers, m.clients)

		SaveServerConfigs(m.configs)
		savedIdx := len(m.clients) - 1
		if f.editingIdx >= 0 {
			savedIdx = f.editingIdx
		}
		m.addForm = nil
		m.currentView = viewDashboard
		return m, pingServerCmd(m.clients[savedIdx], savedIdx)

	case "backspace":
		if f.focusIdx >= 0 && f.focusIdx < len(visible) {
			fld := visible[f.focusIdx]
			if len(*fld.value) > 0 {
				*fld.value = (*fld.value)[:len(*fld.value)-1]
			}
		}
		return m, nil

	default:
		if f.focusIdx >= 0 && f.focusIdx < len(visible) && len(msg.String()) == 1 {
			fld := visible[f.focusIdx]
			*fld.value += msg.String()
		}
	}
	return m, nil
}

// applyAuthEdits copies permission toggles from the auth editor onto the
// matching tokens and persists them. The editor is rebuilt from the token list
// each time the screen opens, so without this step the toggles were lost the
// moment the user pressed esc.
func (m *Model) applyAuthEdits() {
	perms := m.authEditor.Permissions()
	changed := false
	for i := range m.tokenMgr.tokens {
		ops, ok := perms[m.tokenMgr.tokens[i].Name]
		if !ok {
			continue
		}
		if len(ops) == 0 {
			ops = []string{}
		}
		if !sameStrings(m.tokenMgr.tokens[i].Permissions, ops) {
			m.tokenMgr.tokens[i].Permissions = ops
			changed = true
		}
	}
	if changed {
		m.tokenMgr.persist("permissions updated")
	}
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ── View dispatch ─────────────────────────────────────────────────────────

func (m Model) View() string {
	if m.width == 0 {
		return "Initializing Pintail…"
	}
	switch m.currentView {
	case viewTokens:
		return m.viewTokenManager()
	case viewScratchpad:
		return m.viewScratchpadScreen()
	case viewAddServer:
		return m.viewAddServerScreen()
	case viewTLS:
		return m.viewTLSScreen()
	case viewAuth:
		return m.viewAuthScreen()
	case viewSnapshots:
		return m.viewSnapshotsScreen()
	default:
		return m.viewDashboard()
	}
}

// ── Dashboard ─────────────────────────────────────────────────────────────

func (m Model) viewDashboard() string {
	header := m.viewHeader()
	footer := m.viewDashboardFooter()
	headerH := lipgloss.Height(header)
	footerH := lipgloss.Height(footer)
	panelH := m.height - headerH - footerH

	leftW := (m.width * 60) / 100
	rightW := m.width - leftW

	body := lipgloss.JoinHorizontal(lipgloss.Top,
		m.viewConnectionsPanel(leftW, panelH),
		m.viewCatalogPanel(rightW, panelH),
	)
	return lipgloss.JoinVertical(lipgloss.Left, header, body, footer)
}

func (m Model) viewHeader() string {
	titleBar := headerBarStyle.Width(m.width).Render(
		titleStyle.Render("🦆 Pintail") +
			mutedStyle.Render("  ─  DuckDB Quack Protocol Manager  ") +
			mutedStyle.Render(versionLabel()),
	)

	var chips []string
	for i, cfg := range m.configs {
		var state ConnState
		if i < len(m.clients) {
			state = m.clients[i].GetState()
		}

		// Status dot
		var dot string
		var statusDetail string
		if state.PingedAt.IsZero() {
			dot = mutedStyle.Render("◌")
			statusDetail = mutedStyle.Render(" pinging…")
		} else if state.Online && state.Method == "uri" {
			// Reachability was never actually checked for this one — say so
			// rather than showing a green dot we haven't earned.
			dot = amberStyle.Render("◍")
			statusDetail = mutedStyle.Render(" remote path · not probed")
		} else if state.Online {
			dot = greenStyle.Render("●")
			statusDetail = greenStyle.Render(fmt.Sprintf(" %dms", state.Latency.Milliseconds()))
			// Type-appropriate transport badge
			switch cfg.Type {
			case ConnQuack:
				if cfg.TLS {
					statusDetail += mutedStyle.Render(" HTTPS")
				} else {
					statusDetail += amberStyle.Render(" HTTP ⚠")
				}
			case ConnLocal:
				statusDetail += mutedStyle.Render(" file")
			case ConnDuckLake:
				statusDetail += mutedStyle.Render(" ducklake")
			}
		} else {
			dot = redStyle.Render("✕")
			statusDetail = redStyle.Render(" offline") + mutedStyle.Render("  "+state.ErrMsg)
		}

		typeBadge := mutedStyle.Render(" [" + string(cfg.Type) + "]")

		// The selected connection is the one the panels below describe, so it
		// has to be identifiable at a glance — and the digit that selects it is
		// worth showing while we're here.
		name := labelStyle.Render(cfg.Name)
		marker := "  "
		if i == m.selected {
			marker = amberStyle.Render("▸ ")
			name = lipgloss.NewStyle().
				Foreground(colorDarkBg).Background(colorDuckYellow).Bold(true).
				Render(" " + cfg.Name + " ")
		}
		index := ""
		if i < 9 {
			index = mutedStyle.Render(fmt.Sprintf("%d:", i+1))
		}

		chip := marker + index + dot + " " + name +
			typeBadge +
			mutedStyle.Render("  "+truncate(cfg.DisplayURI(), 40)) +
			statusDetail
		chips = append(chips, chip)
	}

	// The summary describes the selected connection, since that is whose
	// sessions the panel below is listing.
	connSummary := ""
	if d, ok := m.selectedData(); ok {
		activeCount := 0
		for _, c := range d.sessions {
			if c.Status == "active" {
				activeCount++
			}
		}
		connSummary = mutedStyle.Render("  sessions  ") +
			greenStyle.Render(fmt.Sprintf("%d active", activeCount)) +
			mutedStyle.Render(fmt.Sprintf(" / %d listed", len(d.sessions)))
	}

	serverRow := "  " + strings.Join(chips, mutedStyle.Render("   ·   ")) + connSummary
	divider := mutedStyle.Render(strings.Repeat("─", m.width))
	return lipgloss.JoinVertical(lipgloss.Left, titleBar, serverRow, divider)
}

func (m Model) viewConnectionsPanel(width, height int) string {
	d, have := m.selectedData()

	// Name the connection in the title: with several servers configured, an
	// unlabelled table of sessions says nothing about whose sessions they are.
	title := labelStyle.Render("ACTIVE CONNECTIONS")
	if name := m.selectedName(); name != "" {
		title += mutedStyle.Render("  ·  ") + brightStyle.Render(name)
	}
	if have && d.reportedCount != "" {
		// DuckDB reports a count but cannot enumerate peers, so the count is
		// labelled as the backend's rather than implied by the row count.
		title += mutedStyle.Render("   backend reports " + d.reportedCount + " connection(s)")
	}

	rows := []string{title, ""}
	if have && d.sessionErr != "" {
		rows = append(rows,
			redStyle.Render("✕ session query failed"),
			mutedStyle.Render("  "+truncate(firstLine(d.sessionErr), width-6)),
			"")
	}
	rows = append(rows, m.connTable.View())
	content := lipgloss.JoinVertical(lipgloss.Left, rows...)
	style := panelStyle
	if m.focus == panelConnections {
		style = activePanelStyle
	}
	return style.Width(width - 2).Height(height - 1).Render(content)
}

func (m Model) viewCatalogPanel(width, height int) string {
	d, _ := m.selectedData()

	var lines []string
	title := labelStyle.Render("CATALOG")
	if name := m.selectedName(); name != "" {
		title += mutedStyle.Render("  ·  ") + brightStyle.Render(name)
	}
	lines = append(lines, title, "")

	if len(d.catalog) == 0 {
		if d.catalogErr != "" {
			lines = append(lines,
				redStyle.Render("  ✕ catalog query failed"),
				"",
				mutedStyle.Render("  "+truncate(firstLine(d.catalogErr), width-6)),
			)
		} else {
			lines = append(lines,
				mutedStyle.Render("  ◌ no catalog data"),
				"",
				mutedStyle.Render("  populates from duckdb_tables() / duckdb_views()"),
				mutedStyle.Render("  when a connection comes online"),
				"",
				mutedStyle.Render("  see README §Getting started"),
			)
		}
		style := panelStyle
		if m.focus == panelCatalog {
			style = activePanelStyle
		}
		return style.Width(width - 2).Height(height - 1).Render(strings.Join(lines, "\n"))
	}

	for _, schema := range d.catalog {
		arrow := "▶"
		if schema.Open {
			arrow = "▼"
		}
		lines = append(lines, amberStyle.Render(arrow+" ")+brightStyle.Bold(true).Render(schema.Name))
		if schema.Open {
			for i, tbl := range schema.Tables {
				conn := "├─"
				if i == len(schema.Tables)-1 {
					conn = "└─"
				}
				size := ""
				if tbl.SizeKnown {
					size = "  " + fmtRows(tbl.Rows)
				}
				lines = append(lines,
					mutedStyle.Render("  "+conn+" ")+
						brightStyle.Render(tbl.Name)+
						mutedStyle.Render("  "+tbl.Format)+
						mutedStyle.Render(size))
			}
		}
		lines = append(lines, "")
	}
	style := panelStyle
	if m.focus == panelCatalog {
		style = activePanelStyle
	}
	return style.Width(width - 2).Height(height - 1).Render(strings.Join(lines, "\n"))
}

func (m Model) viewDashboardFooter() string {
	keys := strings.Join([]string{
		keyBadge("q") + " quit",
		keyBadge("tab") + " panel",
		keyBadge("[ ]") + " connection",
		keyBadge("r") + " refresh",
		keyBadge("t") + " tokens",
		keyBadge("s") + " sql",
		keyBadge("l") + " lake",
		keyBadge("x") + " tls",
		keyBadge("p") + " auth",
		keyBadge("a") + " conn",
	}, "  ")
	var hint string
	if m.focus == panelConnections {
		if row := m.connTable.SelectedRow(); len(row) >= 3 {
			hint = "   " + mutedStyle.Render("│") + "   " +
				mutedStyle.Render("selected  ") + brightStyle.Render(row[2]) +
				mutedStyle.Render("  @  "+row[1])
		}
	}
	divider := mutedStyle.Render(strings.Repeat("─", m.width))
	return lipgloss.JoinVertical(lipgloss.Left, divider, footerStyle.Render(keys)+hint)
}

// ── Add server screen ─────────────────────────────────────────────────────

func (m Model) viewAddServerScreen() string {
	titleBar := headerBarStyle.Width(m.width).Render(
		titleStyle.Render("🦆 Pintail") +
			mutedStyle.Render("  ─  ") +
			labelStyle.Render("Add Connection") +
			mutedStyle.Render("  "+versionLabel()),
	)
	divider := mutedStyle.Render(strings.Repeat("─", m.width))

	f := m.addForm
	visible := f.visibleFields()

	// Type selector at the top of the form
	var typeChips []string
	for _, t := range AllConnTypes {
		chip := mutedStyle.Render(" " + t.Label() + " ")
		if t == f.connType {
			chip = lipgloss.NewStyle().
				Foreground(colorDarkBg).Background(colorDuckYellow).Bold(true).
				Padding(0, 1).
				Render(t.Label())
		}
		typeChips = append(typeChips, chip)
	}
	typeCursor := "  "
	if f.focusIdx == -1 {
		typeCursor = amberStyle.Render("▶ ")
	}
	typeLine := typeCursor + mutedStyle.Render(padRight("Type", 8)) +
		strings.Join(typeChips, mutedStyle.Render("  "))

	// Per-type field rows
	var fieldLines []string
	for i, fld := range visible {
		cursor := "  "
		if i == f.focusIdx {
			cursor = amberStyle.Render("▶ ")
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
			cursor+mutedStyle.Render(padRight(fld.label, 8))+display,
			"    "+mutedStyle.Render(fld.hint),
		)
	}

	// Context-aware hint: on path-like fields, mention tab-completion.
	var hint string
	onPathField := false
	if f.focusIdx >= 0 && f.focusIdx < len(visible) {
		if isPathField(visible[f.focusIdx].label) {
			onPathField = true
		}
	}
	if onPathField {
		hint = mutedStyle.Render("  [tab] complete path  [↓] next field  [shift+←→] cycle type  [enter] save  [esc] back")
	} else {
		hint = mutedStyle.Render("  [↑↓/tab] field  [shift+←→] cycle type  [enter] advance/save  [esc] back")
	}
	if !f.valid() {
		hint += "  " + redStyle.Render("· required fields missing")
	}
	// Why the last save was refused — a duplicate name or a reference that
	// doesn't resolve, both of which used to be accepted and fail later.
	if f.errMsg != "" {
		hint += "\n  " + redStyle.Render("✕ "+f.errMsg)
	}

	// Existing connections panel (left)
	existingLines := []string{
		labelStyle.Render("CONFIGURED CONNECTIONS"),
		mutedStyle.Render("[↑↓] select  [e] edit  [d] delete  [→] back to form"),
		"",
	}
	for i, cfg := range m.configs {
		state := m.clients[i].GetState()
		dot := mutedStyle.Render("◌")
		if !state.PingedAt.IsZero() {
			if state.Online {
				dot = greenStyle.Render("●")
			} else {
				dot = redStyle.Render("✕")
			}
		}
		cursor := "  "
		nameStyle := brightStyle
		if f.focusIdx == -2 && i == f.listCursor {
			cursor = amberStyle.Render("▶ ")
			nameStyle = labelStyle
		}
		typeLabel := mutedStyle.Render("[" + string(cfg.Type) + "]")
		editing := ""
		if f.editingIdx == i {
			editing = amberStyle.Render("  (editing)")
		}
		existingLines = append(existingLines,
			cursor+dot+" "+nameStyle.Render(cfg.Name)+"  "+typeLabel+editing,
			"      "+mutedStyle.Render(truncate(cfg.DisplayURI(), 40)),
		)
	}

	leftStyle := panelStyle
	rightStyle := activePanelStyle
	if f.focusIdx == -2 {
		leftStyle = activePanelStyle
		rightStyle = panelStyle
	}
	leftPanel := leftStyle.Width((m.width / 2) - 2).Render(strings.Join(existingLines, "\n"))
	rightPanel := rightStyle.Width((m.width / 2) - 2).Render(
		lipgloss.JoinVertical(lipgloss.Left,
			labelStyle.Render(editingTitle(f)), "",
			typeLine,
			"",
			strings.Join(fieldLines, "\n"),
			"",
			hint,
		),
	)

	body := lipgloss.JoinHorizontal(lipgloss.Top, leftPanel, rightPanel)
	footerDiv := mutedStyle.Render(strings.Repeat("─", m.width))
	footerLine := footerStyle.Render(
		keyBadge("enter") + " save   " + keyBadge("←") + " connection list   " + keyBadge("esc") + " back",
	)
	return lipgloss.JoinVertical(lipgloss.Left, titleBar, divider, body, footerDiv, footerLine)
}

func editingTitle(f *addServerForm) string {
	if f.editingIdx >= 0 {
		return "EDIT CONNECTION"
	}
	return "NEW CONNECTION"
}

// ── DuckLake snapshots screen ─────────────────────────────────────────────

func (m Model) viewSnapshotsScreen() string {
	titleBar := headerBarStyle.Width(m.width).Render(
		titleStyle.Render("🦆 Pintail") + mutedStyle.Render("  ─  ") +
			labelStyle.Render("DuckLake Snapshots") + mutedStyle.Render("  "+versionLabel()),
	)
	divider := mutedStyle.Render(strings.Repeat("─", m.width))
	header := lipgloss.JoinVertical(lipgloss.Left,
		titleBar,
		m.snapshots.ViewTargetBar(),
		divider,
	)

	footerDiv := mutedStyle.Render(strings.Repeat("─", m.width))
	footer := lipgloss.JoinVertical(lipgloss.Left, footerDiv, m.snapshots.ViewFooter())

	headerH := lipgloss.Height(header)
	footerH := lipgloss.Height(footer)
	panelH := m.height - headerH - footerH

	leftW := (m.width * 35) / 100
	rightW := m.width - leftW

	leftContent := m.snapshots.ViewList(leftW - 4)
	rightContent := m.snapshots.ViewDetail(rightW - 4)

	leftPanel := activePanelStyle.Width(leftW - 2).Height(panelH - 1).Render(leftContent)
	rightPanel := panelStyle.Width(rightW - 2).Height(panelH - 1).Render(rightContent)

	body := lipgloss.JoinHorizontal(lipgloss.Top, leftPanel, rightPanel)
	return lipgloss.JoinVertical(lipgloss.Left, header, body, footer)
}

// ── Auth policy editor screen ─────────────────────────────────────────────

func (m Model) viewAuthScreen() string {
	titleBar := headerBarStyle.Width(m.width).Render(
		titleStyle.Render("🦆 Pintail") + mutedStyle.Render("  ─  ") +
			labelStyle.Render("Auth Policy Editor") + mutedStyle.Render("  "+versionLabel()),
	)
	divider := mutedStyle.Render(strings.Repeat("─", m.width))
	header := lipgloss.JoinVertical(lipgloss.Left, titleBar, divider)

	footerDiv := mutedStyle.Render(strings.Repeat("─", m.width))
	footer := lipgloss.JoinVertical(lipgloss.Left, footerDiv, m.authEditor.ViewFooter())

	headerH := lipgloss.Height(header)
	footerH := lipgloss.Height(footer)
	panelH := m.height - headerH - footerH

	leftW := (m.width * 32) / 100
	rightW := m.width - leftW

	listContent := m.authEditor.ViewPolicyList(leftW-4, panelH)
	permContent := m.authEditor.ViewPermGrid(rightW - 4)

	leftStyle := panelStyle
	rightStyle := panelStyle
	if m.authEditor.focus == 0 {
		leftStyle = activePanelStyle
	} else {
		rightStyle = activePanelStyle
	}

	leftPanel := leftStyle.Width(leftW - 2).Height(panelH - 1).Render(listContent)
	rightPanel := rightStyle.Width(rightW - 2).Height(panelH - 1).Render(permContent)

	body := lipgloss.JoinHorizontal(lipgloss.Top, leftPanel, rightPanel)
	return lipgloss.JoinVertical(lipgloss.Left, header, body, footer)
}

// ── TLS config generator screen ───────────────────────────────────────────

func (m Model) viewTLSScreen() string {
	titleBar := headerBarStyle.Width(m.width).Render(
		titleStyle.Render("🦆 Pintail") + mutedStyle.Render("  ─  ") +
			labelStyle.Render("TLS Config Generator") + mutedStyle.Render("  "+versionLabel()),
	)
	divider := mutedStyle.Render(strings.Repeat("─", m.width))
	header := lipgloss.JoinVertical(lipgloss.Left, titleBar, divider)

	footerDiv := mutedStyle.Render(strings.Repeat("─", m.width))
	statusBar := "  " + m.tlsGen.ViewStatusBar()
	footer := lipgloss.JoinVertical(lipgloss.Left, footerDiv, statusBar, m.tlsGen.ViewFooter())

	headerH := lipgloss.Height(header)
	footerH := lipgloss.Height(footer)
	panelH := m.height - headerH - footerH

	formW := (m.width * 38) / 100
	configW := m.width - formW

	formContent := m.tlsGen.ViewForm(formW - 4)
	formPanel := panelStyle.Width(formW - 2).Height(panelH - 1).Render(formContent)

	configPanel := activePanelStyle.Width(configW - 2).Height(panelH - 1).Render(
		m.tlsGen.ViewConfig(),
	)

	body := lipgloss.JoinHorizontal(lipgloss.Top, formPanel, configPanel)
	return lipgloss.JoinVertical(lipgloss.Left, header, body, footer)
}

// ── Token manager (mode-aware: dispatches between tokens and secrets) ────

func (m Model) viewTokenManager() string {
	header := m.viewTokenHeader()
	footer := m.viewTokenFooter()
	headerH := lipgloss.Height(header)
	footerH := lipgloss.Height(footer)
	panelH := m.height - headerH - footerH

	leftW := (m.width * 30) / 100
	rightW := m.width - leftW

	var leftContent, rightContent string

	if m.tokenMgr.mode == tmModeSecrets {
		// Secrets mode
		leftContent = m.tokenMgr.ViewSecretList(leftW-4, panelH)
		switch {
		case m.tokenMgr.secretForm != nil:
			rightContent = lipgloss.JoinVertical(lipgloss.Left,
				m.tokenMgr.ViewSecretDetail(rightW-6), "",
				lipgloss.PlaceHorizontal(rightW-4, lipgloss.Center,
					m.tokenMgr.ViewSecretForm(rightW-12, panelH)),
			)
		case m.tokenMgr.secretDelConfirm:
			sel := m.tokenMgr.selectedSecret()
			name := ""
			if sel != nil {
				name = sel.Name
			}
			rightContent = lipgloss.JoinVertical(lipgloss.Left,
				m.tokenMgr.ViewSecretDetail(rightW-6), "",
				lipgloss.PlaceHorizontal(rightW-4, lipgloss.Left,
					m.tokenMgr.ViewConfirmDialog("Delete secret",
						fmt.Sprintf("Delete the secret  %s?\nConnections that reference it will fail until reconfigured.", name)),
				))
		default:
			rightContent = m.tokenMgr.ViewSecretDetail(rightW - 6)
		}
	} else {
		// Tokens mode (original behaviour)
		leftContent = m.tokenMgr.ViewTokenList(leftW-4, panelH)
		switch {
		case m.tokenMgr.form != nil:
			rightContent = lipgloss.JoinVertical(lipgloss.Left,
				m.tokenMgr.ViewTokenDetail(rightW-6), "",
				lipgloss.PlaceHorizontal(rightW-4, lipgloss.Center, m.tokenMgr.ViewForm(rightW, panelH)),
			)
		case m.tokenMgr.rotateConfirm:
			sel := m.tokenMgr.selectedToken()
			name := ""
			if sel != nil {
				name = sel.Name
			}
			rightContent = lipgloss.JoinVertical(lipgloss.Left,
				m.tokenMgr.ViewTokenDetail(rightW-6), "",
				lipgloss.PlaceHorizontal(rightW-4, lipgloss.Left,
					m.tokenMgr.ViewConfirmDialog("Rotate token",
						fmt.Sprintf("Generate a new value for  %s?\nThe old value will stop working immediately.", name)),
				))
		case m.tokenMgr.revokeConfirm:
			sel := m.tokenMgr.selectedToken()
			name := ""
			if sel != nil {
				name = sel.Name
			}
			rightContent = lipgloss.JoinVertical(lipgloss.Left,
				m.tokenMgr.ViewTokenDetail(rightW-6), "",
				lipgloss.PlaceHorizontal(rightW-4, lipgloss.Left,
					m.tokenMgr.ViewConfirmDialog("Revoke token",
						fmt.Sprintf("Permanently revoke  %s?\nAll connections using it will be dropped.", name)),
				))
		default:
			rightContent = m.tokenMgr.ViewTokenDetail(rightW - 6)
		}
	}

	leftPanel := panelStyle.Width(leftW - 2).Height(panelH - 1).Render(leftContent)
	rightPanel := activePanelStyle.Width(rightW - 2).Height(panelH - 1).Render(rightContent)
	body := lipgloss.JoinHorizontal(lipgloss.Top, leftPanel, rightPanel)
	return lipgloss.JoinVertical(lipgloss.Left, header, body, footer)
}

func (m Model) viewTokenHeader() string {
	titleBar := headerBarStyle.Width(m.width).Render(
		titleStyle.Render("🦆 Pintail") + mutedStyle.Render("  ─  ") +
			labelStyle.Render("Token Manager") + mutedStyle.Render("  "+versionLabel()),
	)
	activeTokens := 0
	for _, t := range m.tokenMgr.tokens {
		if t.Active {
			activeTokens++
		}
	}
	statRow := "  " +
		mutedStyle.Render("tokens  ") +
		greenStyle.Render(fmt.Sprintf("%d active", activeTokens)) +
		mutedStyle.Render(fmt.Sprintf(" / %d total", len(m.tokenMgr.tokens)))
	divider := mutedStyle.Render(strings.Repeat("─", m.width))
	return lipgloss.JoinVertical(lipgloss.Left, titleBar, statRow, divider)
}

func (m Model) viewTokenFooter() string {
	divider := mutedStyle.Render(strings.Repeat("─", m.width))
	return lipgloss.JoinVertical(lipgloss.Left, divider, m.tokenMgr.ViewFooter())
}

// ── Scratchpad screen ─────────────────────────────────────────────────────

func (m Model) viewScratchpadScreen() string {
	titleBar := headerBarStyle.Width(m.width).Render(
		titleStyle.Render("🦆 Pintail") + mutedStyle.Render("  ─  ") +
			labelStyle.Render("SQL Scratchpad") + mutedStyle.Render("  "+versionLabel()),
	)
	divider := mutedStyle.Render(strings.Repeat("─", m.width))
	header := lipgloss.JoinVertical(lipgloss.Left, titleBar, divider)

	footerDivider := mutedStyle.Render(strings.Repeat("─", m.width))
	footer := lipgloss.JoinVertical(lipgloss.Left, footerDivider, m.scratchpad.ViewFooter())

	return lipgloss.JoinVertical(lipgloss.Left,
		header,
		m.scratchpad.ViewEditor(),
		"",
		m.scratchpad.ViewResultsStatus(),
		m.scratchpad.ViewResults(),
		footer,
	)
}

// ── format helpers ────────────────────────────────────────────────────────

func connectionRows(conns []Connection) []table.Row {
	rows := make([]table.Row, len(conns))
	for i, c := range conns {
		rows[i] = table.Row{
			c.ID, c.IP, c.Identity, c.Catalog,
			fmtDuration(c.Duration), fmt.Sprintf("%d", c.Queries),
			statusGlyph(c.Status) + c.Status,
		}
	}
	return rows
}

func statusGlyph(s string) string {
	switch s {
	case "active":
		return "● "
	case "idle":
		return "◌ "
	case "error":
		return "✕ "
	}
	return "  "
}

func fmtDuration(d time.Duration) string {
	h := int(d.Hours())
	mn := int(d.Minutes()) % 60
	sc := int(d.Seconds()) % 60
	if h > 0 {
		return fmt.Sprintf("%dh%02dm", h, mn)
	}
	if mn > 0 {
		return fmt.Sprintf("%dm%02ds", mn, sc)
	}
	return fmt.Sprintf("%ds", sc)
}

func fmtRows(n int64) string {
	switch {
	case n >= 1_000_000_000:
		return fmt.Sprintf("%.1fB rows", float64(n)/1_000_000_000)
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM rows", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fK rows", float64(n)/1_000)
	}
	return fmt.Sprintf("%d rows", n)
}
