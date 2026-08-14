package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// ── connection type ───────────────────────────────────────────────────────

// ConnType identifies which kind of DuckDB backend a config points at.
type ConnType string

const (
	ConnQuack    ConnType = "quack"    // remote Quack server  (quack://host:port)
	ConnLocal    ConnType = "local"    // local .duckdb file   (file:/path)
	ConnDuckLake ConnType = "ducklake" // DuckLake lakehouse   (catalog + object store)
)

// AllConnTypes is the cycle order used by the add-server form.
var AllConnTypes = []ConnType{ConnQuack, ConnLocal, ConnDuckLake}

// Next returns the next ConnType in cycle order — used by the type selector.
func (t ConnType) Next() ConnType {
	for i, ct := range AllConnTypes {
		if ct == t {
			return AllConnTypes[(i+1)%len(AllConnTypes)]
		}
	}
	return ConnQuack
}

// Label returns a short display label for the type.
func (t ConnType) Label() string {
	switch t {
	case ConnQuack:
		return "Quack"
	case ConnLocal:
		return "Local"
	case ConnDuckLake:
		return "DuckLake"
	}
	return string(t)
}

// ── capabilities ──────────────────────────────────────────────────────────
//
// Capabilities formalise the cross-cutting questions the UI keeps asking
// about a backend: "does this support sessions? snapshots? storage secrets?"
// Without this, those questions become implicit `switch c.Type` blocks
// scattered through the codebase.
//
// IMPORTANT — when to use a capability check vs a type switch:
//
//   USE a capability check for cross-cutting *behaviour gating*:
//     - "should this screen show up for this connection?"
//     - "should this field appear in the form?"
//     - "is this operation supported at all?"
//
//   USE a type switch for *per-type implementations of the same verb*:
//     - how to ping (TCP dial vs file stat)
//     - how to build the attach prefix
//     - what URI to display in the header
//
// Mixing these up makes the abstraction less useful, not more.

type Capability string

const (
	CapSessions       Capability = "sessions"        // active connection list (duckdb_connections())
	CapSnapshots      Capability = "snapshots"       // DuckLake snapshot time-travel
	CapStorageSecrets Capability = "storage_secrets" // object-store credentials needed
	CapTokenAuth      Capability = "token_auth"      // bearer token authentication
)

// AllCapabilities is the registry used by display code that wants to enumerate.
var AllCapabilities = []Capability{CapSessions, CapSnapshots, CapStorageSecrets, CapTokenAuth}

// Capabilities returns the set of capabilities supported by this connection.
// The map is keyed for fast lookup; absent keys mean "not supported".
func (c ServerConfig) Capabilities() map[Capability]bool {
	switch c.Type {
	case ConnQuack:
		return map[Capability]bool{
			CapSessions:  true,
			CapTokenAuth: true,
		}
	case ConnLocal:
		return map[Capability]bool{
			CapStorageSecrets: true,
		}
	case ConnDuckLake:
		return map[Capability]bool{
			CapSnapshots:      true,
			CapStorageSecrets: true,
		}
	}
	return map[Capability]bool{}
}

// Supports is a convenience predicate — Supports(CapSnapshots) reads cleaner
// at call sites than indexing the map directly.
func (c ServerConfig) Supports(cap Capability) bool {
	return c.Capabilities()[cap]
}

// ── storage secrets ───────────────────────────────────────────────────────

// SecretType identifies which kind of object-store DuckDB secret this is.
// These map directly onto DuckDB's CREATE SECRET (TYPE …) syntax.
type SecretType string

const (
	SecretS3    SecretType = "s3"
	SecretR2    SecretType = "r2"
	SecretGCS   SecretType = "gcs"
	SecretAzure SecretType = "azure"
)

// AllSecretTypes is the cycle order used by the secret form.
var AllSecretTypes = []SecretType{SecretS3, SecretR2, SecretGCS, SecretAzure}

// Next returns the next SecretType in cycle order.
func (t SecretType) Next() SecretType {
	for i, st := range AllSecretTypes {
		if st == t {
			return AllSecretTypes[(i+1)%len(AllSecretTypes)]
		}
	}
	return SecretS3
}

// StorageSecret holds credentials for an object store (S3 / R2 / GCS / Azure).
// Stored in ~/.duckdb/pintail.json alongside connections. The plaintext
// caveat applies — file permissions are the user's responsibility.
type StorageSecret struct {
	Name      string     `json:"name"`
	Type      SecretType `json:"type"`
	KeyID     string     `json:"key_id,omitempty"`            // s3 / r2 / gcs
	Secret    string     `json:"secret,omitempty"`            // s3 / r2 / gcs
	Region    string     `json:"region,omitempty"`            // s3
	AccountID string     `json:"account_id,omitempty"`        // r2
	ConnStr   string     `json:"connection_string,omitempty"` // azure
	Scope     string     `json:"scope,omitempty"`             // optional everywhere
	CreatedAt time.Time  `json:"created_at,omitempty"`
}

// sqlQuote escapes a value for use inside a single-quoted SQL literal. Every
// generated statement here is handed to `duckdb -c`, so an unescaped quote in a
// token, path or secret produced a broken script (TOKEN 'ab'cd') rather than a
// working one.
func sqlQuote(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

// parts returns the per-type field list used to build CREATE SECRET SQL.
func (s StorageSecret) parts() []string {
	out := []string{"TYPE " + string(s.Type)}
	switch s.Type {
	case SecretS3:
		out = append(out, fmt.Sprintf("KEY_ID '%s'", sqlQuote(s.KeyID)), fmt.Sprintf("SECRET '%s'", sqlQuote(s.Secret)))
		if s.Region != "" {
			out = append(out, fmt.Sprintf("REGION '%s'", sqlQuote(s.Region)))
		}
	case SecretR2:
		out = append(out,
			fmt.Sprintf("KEY_ID '%s'", sqlQuote(s.KeyID)),
			fmt.Sprintf("SECRET '%s'", sqlQuote(s.Secret)),
			fmt.Sprintf("ACCOUNT_ID '%s'", sqlQuote(s.AccountID)))
	case SecretGCS:
		out = append(out, fmt.Sprintf("KEY_ID '%s'", sqlQuote(s.KeyID)), fmt.Sprintf("SECRET '%s'", sqlQuote(s.Secret)))
	case SecretAzure:
		out = append(out, fmt.Sprintf("CONNECTION_STRING '%s'", sqlQuote(s.ConnStr)))
	}
	if s.Scope != "" {
		out = append(out, fmt.Sprintf("SCOPE '%s'", sqlQuote(s.Scope)))
	}
	return out
}

// ToSQL returns a multi-line CREATE SECRET statement suitable for export
// files or human display.
func (s StorageSecret) ToSQL() string {
	return fmt.Sprintf("CREATE OR REPLACE SECRET %s (\n    %s\n);",
		s.Name, strings.Join(s.parts(), ",\n    "))
}

// ToSQLInline returns the same statement on one line, suitable for inlining
// inside an AttachPrefix string.
func (s StorageSecret) ToSQLInline(asName string) string {
	return fmt.Sprintf("CREATE OR REPLACE SECRET %s (%s);",
		asName, strings.Join(s.parts(), ", "))
}

// extension returns the DuckDB extension needed to use this secret type.
func (s StorageSecret) extension() string {
	if s.Type == SecretAzure {
		return "azure"
	}
	return "httpfs"
}

// SecretResolver looks up a StorageSecret by name — used by AttachPrefix to
// inject CREATE SECRET when a connection has a StorageSecretRef.
type SecretResolver func(name string) (StorageSecret, bool)

// ── config ────────────────────────────────────────────────────────────────

// ServerConfig holds the persisted connection parameters for one backend.
// Which fields are meaningful depends on Type; omitempty keeps unused fields
// out of the on-disk JSON.
type ServerConfig struct {
	Name string   `json:"name"`
	Type ConnType `json:"type"`

	// Quack-remote fields
	Host  string `json:"host,omitempty"`
	Port  int    `json:"port,omitempty"`
	Token string `json:"token,omitempty"`
	TLS   bool   `json:"tls,omitempty"`

	// Local-file field
	Path string `json:"path,omitempty"`

	// DuckLake fields
	CatalogPath string `json:"catalog_path,omitempty"` // freeform catalog URL/path
	CatalogRef  string `json:"catalog_ref,omitempty"`  // OR name of another configured connection to use as the catalog (overrides CatalogPath when set)
	StoragePath string `json:"storage_path,omitempty"` // object storage root (DATA_PATH)

	// Storage credentials — applies to local and ducklake types. For local,
	// only meaningful when the path is a remote URI (s3://, gs://, …) or the
	// data inside references remote files. For quack, this field is ignored:
	// the Quack server has its own credentials and that's the server's concern.
	StorageSecretRef string `json:"storage_secret_ref,omitempty"`
}

func (c ServerConfig) Addr() string {
	return fmt.Sprintf("%s:%d", c.Host, c.Port)
}

// LocalIsRemote reports whether a local-type connection points at a URI rather
// than a file on disk (s3://bucket/db.duckdb and friends, which the README
// documents as supported). Such a path cannot be stat'd, and cannot be opened
// as duckdb's positional argument either: the storage secret has to exist
// before the database is opened, so it needs the ATTACH form instead.
func (c ServerConfig) LocalIsRemote() bool {
	return c.Type == ConnLocal && strings.Contains(c.Path, "://")
}

func (c ServerConfig) BaseURL() string {
	scheme := "http"
	if c.TLS {
		scheme = "https"
	}
	return fmt.Sprintf("%s://%s:%d", scheme, c.Host, c.Port)
}

func (c ServerConfig) QuackURI() string {
	return fmt.Sprintf("quack://%s", c.Addr())
}

// DisplayURI is the human-readable string shown in the header per server.
func (c ServerConfig) DisplayURI() string {
	switch c.Type {
	case ConnLocal:
		return "file:" + c.Path
	case ConnDuckLake:
		cat := c.CatalogPath
		if c.CatalogRef != "" {
			cat = "→" + c.CatalogRef
		}
		return "ducklake:" + cat + "  →  " + c.StoragePath
	default:
		return c.QuackURI()
	}
}

// AttachPrefix returns the SQL prologue that gets prepended to user queries so
// the rest of the query can use unqualified table names. For ConnLocal this is
// empty — we open the file directly via argv — except when a storage secret
// is referenced (e.g. the path is a remote URI).
//
// For ConnDuckLake, if CatalogRef is set we resolve it via `resolve` and emit
// a two-step ATTACH (resolved-conn AS _catalog, then ducklake:_catalog AS _lake).
// This is the pattern that lets a Quack-fronted DuckDB act as a DuckLake catalog,
// supporting the multi-writer deployment Quack was designed for.
//
// If StorageSecretRef is set (and the connection isn't ConnQuack), the resolved
// storage secret is injected as CREATE SECRET _storage(...) at the very front,
// preceded by the relevant extension load (httpfs / azure).
func (c ServerConfig) AttachPrefix(resolve ConfigResolver, resolveSecret SecretResolver) string {
	var b strings.Builder

	// 1. Storage secret injection. Whether to inject is a capability check
	// (some backends use object-store credentials, some don't); the injection
	// itself is the same SQL for any backend that needs it.
	if c.Supports(CapStorageSecrets) && c.StorageSecretRef != "" && resolveSecret != nil {
		if sec, ok := resolveSecret(c.StorageSecretRef); ok {
			ext := sec.extension()
			b.WriteString(fmt.Sprintf("INSTALL %s; LOAD %s; ", ext, ext))
			b.WriteString(sec.ToSQLInline("_storage"))
			b.WriteString(" ")
		}
	}

	// 2. Type-specific attach.
	switch c.Type {
	case ConnLocal:
		// On-disk files are opened positionally by the caller, so nothing to
		// attach. A remote path has to be attached after the secret exists —
		// and read-only, which is all DuckDB supports over object storage.
		if c.LocalIsRemote() {
			b.WriteString(fmt.Sprintf(
				"ATTACH '%s' AS _local (READ_ONLY); USE _local; ", sqlQuote(c.Path)))
		}
	case ConnQuack:
		b.WriteString(fmt.Sprintf(
			"ATTACH '%s' AS _remote (TOKEN '%s'); USE _remote; ",
			c.QuackURI(), sqlQuote(c.Token),
		))
	case ConnDuckLake:
		if c.CatalogRef != "" && resolve != nil {
			if catCfg, ok := resolve(c.CatalogRef); ok {
				catalogAttach := buildCatalogAttach(catCfg)
				b.WriteString(fmt.Sprintf(
					"INSTALL ducklake; LOAD ducklake; %sATTACH 'ducklake:_catalog' AS _lake (DATA_PATH '%s'); USE _lake; ",
					catalogAttach, sqlQuote(c.StoragePath),
				))
				return b.String()
			}
		}
		b.WriteString(fmt.Sprintf(
			"INSTALL ducklake; LOAD ducklake; ATTACH 'ducklake:%s' AS _lake (DATA_PATH '%s'); USE _lake; ",
			sqlQuote(c.CatalogPath), sqlQuote(c.StoragePath),
		))
	}
	return b.String()
}

// buildCatalogAttach renders just the ATTACH ... AS _catalog statement for a
// referenced config. Handles Quack (with token), local file, and freeform URLs.
func buildCatalogAttach(catCfg ServerConfig) string {
	switch catCfg.Type {
	case ConnQuack:
		return fmt.Sprintf(
			"ATTACH '%s' AS _catalog (TOKEN '%s'); ",
			catCfg.QuackURI(), sqlQuote(catCfg.Token),
		)
	case ConnLocal:
		return fmt.Sprintf("ATTACH '%s' AS _catalog; ", sqlQuote(catCfg.Path))
	default:
		// Treat as freeform URL (postgres://, mysql://, etc.)
		return fmt.Sprintf("ATTACH '%s' AS _catalog; ", sqlQuote(catCfg.CatalogPath))
	}
}

// ToServerInfo converts a ServerConfig into the display-oriented ServerInfo type.
func (c ServerConfig) ToServerInfo() ServerInfo {
	return ServerInfo{
		Name: c.Name,
		Type: c.Type,
		URI:  c.DisplayURI(),
		Host: c.Host,
		Port: c.Port,
		TLS:  c.TLS,
	}
}

// ── conn state ────────────────────────────────────────────────────────────

// ConnState is the live health snapshot for one server.
type ConnState struct {
	Online   bool
	Latency  time.Duration
	ErrMsg   string
	PingedAt time.Time
	Method   string // "tcp" | "http" | "cli" | "mock"
}

// ── async messages ────────────────────────────────────────────────────────

// pingResultMsg is sent on the Bubble Tea bus when a server ping completes.
type pingResultMsg struct {
	idx     int
	latency time.Duration
	err     error
}

// queryResultMsg is sent on the Bubble Tea bus when a query completes. The
// result always carries its own error (in QueryResult.Err), so there is no
// separate error field, and no isMock flag now that the fabricated executor is
// gone.
type queryResultMsg struct {
	result *QueryResult
}

// pingServerCmd launches an async TCP ping for client[idx].
func pingServerCmd(c *QuackClient, idx int) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		lat, err := c.Ping(ctx)
		return pingResultMsg{idx: idx, latency: lat, err: err}
	}
}

// ── client ────────────────────────────────────────────────────────────────

// ConfigResolver looks up a ServerConfig by name. The DuckLake catalog-ref
// machinery uses this to find the referenced connection at query time.
type ConfigResolver func(name string) (ServerConfig, bool)

// QuackClient manages connectivity and query execution for a single backend.
type QuackClient struct {
	Config         ServerConfig
	resolver       ConfigResolver
	secretResolver SecretResolver

	mu      sync.RWMutex
	state   ConnState
	http    *http.Client
	hasCLI  bool
	cliPath string
}

// NewQuackClient constructs a client and probes for the duckdb CLI.
// `resolver` is used by DuckLake configs that reference another connection
// as their catalog (Config.CatalogRef). `secretResolver` is used by configs
// that reference a stored credential (Config.StorageSecretRef). Pass nil
// for either when no resolution is needed.
func NewQuackClient(cfg ServerConfig, resolver ConfigResolver, secretResolver SecretResolver) *QuackClient {
	if resolver == nil {
		resolver = func(string) (ServerConfig, bool) { return ServerConfig{}, false }
	}
	if secretResolver == nil {
		secretResolver = func(string) (StorageSecret, bool) { return StorageSecret{}, false }
	}
	c := &QuackClient{
		Config:         cfg,
		resolver:       resolver,
		secretResolver: secretResolver,
		http:           &http.Client{Timeout: 8 * time.Second},
	}
	if path, err := exec.LookPath("duckdb"); err == nil {
		c.hasCLI = true
		c.cliPath = path
	}
	return c
}

// attachPrefix is the client-side wrapper around ServerConfig.AttachPrefix
// that injects this client's resolvers.
func (c *QuackClient) attachPrefix() string {
	return c.Config.AttachPrefix(c.resolver, c.secretResolver)
}

// HasCLI reports whether the duckdb CLI binary is in PATH.
func (c *QuackClient) HasCLI() bool { return c.hasCLI }

// GetState returns a snapshot of the current connection state.
func (c *QuackClient) GetState() ConnState {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.state
}

// Ping checks reachability for whichever backend type this client points at.
// It updates the client's internal ConnState and returns the latency.
func (c *QuackClient) Ping(ctx context.Context) (time.Duration, error) {
	switch c.Config.Type {
	case ConnLocal:
		return c.pingLocal()
	case ConnDuckLake:
		return c.pingDuckLake(ctx)
	default: // ConnQuack
		return c.pingTCP(ctx)
	}
}

// pingTCP dials host:port for a Quack remote.
func (c *QuackClient) pingTCP(ctx context.Context) (time.Duration, error) {
	start := time.Now()
	dialer := &net.Dialer{}
	conn, err := dialer.DialContext(ctx, "tcp", c.Config.Addr())
	latency := time.Since(start)

	c.mu.Lock()
	defer c.mu.Unlock()

	if err != nil {
		c.state = ConnState{Online: false, ErrMsg: simplifyNetErr(err), PingedAt: time.Now(), Method: "tcp"}
		return 0, err
	}
	conn.Close()
	c.state = ConnState{Online: true, Latency: latency, PingedAt: time.Now(), Method: "tcp"}
	return latency, nil
}

// pingLocal stats the .duckdb file and verifies it's a regular file.
//
// A remote path (s3://…) cannot be stat'd, and probing it properly would mean
// spawning duckdb on every 5s tick — which is exactly what the ping cadence
// exists to avoid. Such connections are reported as unprobed rather than as
// missing files: queries are attempted, and the first one surfaces the real
// error if the path or credentials are wrong.
func (c *QuackClient) pingLocal() (time.Duration, error) {
	if c.Config.LocalIsRemote() {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.state = ConnState{Online: true, PingedAt: time.Now(), Method: "uri"}
		return 0, nil
	}

	start := time.Now()
	info, err := os.Stat(c.Config.Path)
	latency := time.Since(start)

	c.mu.Lock()
	defer c.mu.Unlock()

	if err != nil {
		c.state = ConnState{Online: false, ErrMsg: "file not found", PingedAt: time.Now(), Method: "stat"}
		return 0, err
	}
	if info.IsDir() {
		errMsg := "path is a directory"
		c.state = ConnState{Online: false, ErrMsg: errMsg, PingedAt: time.Now(), Method: "stat"}
		return 0, fmt.Errorf("%s", errMsg)
	}
	c.state = ConnState{Online: true, Latency: latency, PingedAt: time.Now(), Method: "stat"}
	return latency, nil
}

// pingDuckLake reaches the catalog DB — TCP-dialing it if it's a URL,
// or stat'ing it if it's a local file path.
func (c *QuackClient) pingDuckLake(ctx context.Context) (time.Duration, error) {
	catalogPath := c.Config.CatalogPath
	start := time.Now()

	// URL form: postgres://host:port/db, mysql://host:port/db, sqlite:///path
	if strings.Contains(catalogPath, "://") {
		// Extract host:port portion for TCP dial
		afterScheme := catalogPath[strings.Index(catalogPath, "://")+3:]
		hostPort := afterScheme
		if i := strings.IndexAny(afterScheme, "/?"); i >= 0 {
			hostPort = afterScheme[:i]
		}
		if !strings.Contains(hostPort, ":") {
			// File-based scheme like sqlite:///path/db
			return c.pingDuckLakeFile(strings.TrimPrefix(afterScheme, "/"), start)
		}
		dialer := &net.Dialer{}
		conn, err := dialer.DialContext(ctx, "tcp", hostPort)
		latency := time.Since(start)

		c.mu.Lock()
		defer c.mu.Unlock()
		if err != nil {
			c.state = ConnState{Online: false, ErrMsg: simplifyNetErr(err), PingedAt: time.Now(), Method: "tcp"}
			return 0, err
		}
		conn.Close()
		c.state = ConnState{Online: true, Latency: latency, PingedAt: time.Now(), Method: "tcp"}
		return latency, nil
	}

	// Bare path — treat as local file catalog
	return c.pingDuckLakeFile(catalogPath, start)
}

func (c *QuackClient) pingDuckLakeFile(path string, start time.Time) (time.Duration, error) {
	_, err := os.Stat(path)
	latency := time.Since(start)

	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil {
		c.state = ConnState{Online: false, ErrMsg: "catalog file not found", PingedAt: time.Now(), Method: "stat"}
		return 0, err
	}
	c.state = ConnState{Online: true, Latency: latency, PingedAt: time.Now(), Method: "stat"}
	return latency, nil
}

// ── query routing ─────────────────────────────────────────────────────────

// defaultQueryTimeout bounds a single scratchpad or CLI query. An admin check
// against a cold object store can legitimately take a while, so it is
// overridable rather than a hardcoded 30s.
const defaultQueryTimeout = 30 * time.Second

// QueryTimeout is the per-query deadline, overridable with
// PINTAIL_QUERY_TIMEOUT (seconds). An unparseable or non-positive value falls
// back to the default rather than disabling the deadline.
func QueryTimeout() time.Duration {
	if v := os.Getenv("PINTAIL_QUERY_TIMEOUT"); v != "" {
		if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
			return time.Duration(secs) * time.Second
		}
	}
	return defaultQueryTimeout
}

// QueryAsync returns a tea.Cmd that runs the query in a goroutine and delivers
// a queryResultMsg back to the update loop. Cancelling ctx aborts the query —
// the CLI subprocess is killed with it — which is what makes ctrl+c in the
// scratchpad able to interrupt a long-running statement.
func (c *QuackClient) QueryAsync(ctx context.Context, sql string) tea.Cmd {
	return func() tea.Msg {
		return queryResultMsg{result: c.Query(ctx, sql)}
	}
}

// Query runs sql and always returns a result: failures come back with Err set
// rather than as a Go error, so every caller reports them the same way. Both
// the TUI and the `pintail query` subcommand go through here, which is why the
// CLI can now reach a Quack server over HTTP without a duckdb binary.
func (c *QuackClient) Query(ctx context.Context, sql string) *QueryResult {
	state := c.GetState()
	start := time.Now()

	if !state.Online {
		// Offline — surface a clear error rather than fabricating data.
		r := offlineResult(sql)
		r.ElapsedMs = int(time.Since(start).Milliseconds())
		return &r
	}

	done := func(r *QueryResult, method string) *QueryResult {
		r.ElapsedMs = int(time.Since(start).Milliseconds())
		r.Method = method
		return r
	}
	fail := func(method, msg string) *QueryResult {
		return &QueryResult{
			Query:     sql,
			Err:       msg,
			Timestamp: time.Now(),
			ElapsedMs: int(time.Since(start).Milliseconds()),
			Method:    method,
		}
	}

	// Only Quack remotes have a host:port to POST to. For local files and
	// DuckLake the CLI is the one and only path, so a CLI failure IS the
	// answer: falling through to HTTP dialed http://:0 and replaced the
	// real DuckDB error with "no endpoint responded", which is what the
	// user then had to debug.
	overHTTP := c.Config.Type == ConnQuack

	var errs []string
	method := "cli"

	if c.hasCLI {
		r, err := c.queryCLI(ctx, sql)
		if err == nil {
			return done(r, "cli")
		}
		// A cancelled or timed-out query is not a SQL failure; say which.
		if ctxErr := ctxReason(ctx); ctxErr != "" {
			return fail("cli", ctxErr)
		}
		errs = append(errs, err.Error())
	} else if !overHTTP {
		return fail("cli", fmt.Sprintf(
			"duckdb CLI not found in PATH — required for %s connections", c.Config.Type))
	}

	if overHTTP {
		method = "http"
		r, err := c.queryHTTP(ctx, sql)
		if err == nil {
			return done(r, "http")
		}
		if ctxErr := ctxReason(ctx); ctxErr != "" {
			return fail("http", ctxErr)
		}
		errs = append(errs, err.Error())
	}

	return fail(method, strings.Join(errs, "; "))
}

// ctxReason turns a finished context into the reason a query stopped, or ""
// when the context is still live and the failure was the query's own.
func ctxReason(ctx context.Context) string {
	switch ctx.Err() {
	case context.Canceled:
		return "cancelled"
	case context.DeadlineExceeded:
		return fmt.Sprintf("timed out after %s", QueryTimeout())
	}
	return ""
}

// cliArgs builds the duckdb argv for a script.
//
// A local file on disk is opened positionally; everything else is reached with
// the ATTACH + USE prologue so unqualified table names resolve. The prologue is
// prepended for *every* type, including local: it is empty for a plain local
// file, but carries the CREATE SECRET when the connection references a storage
// secret. Local connections previously skipped it entirely, so the documented
// combination of a local path plus storage_secret_ref silently did nothing.
func (c *QuackClient) cliArgs(sql string, flags ...string) []string {
	args := make([]string, 0, len(flags)+3)
	if c.Config.Type == ConnLocal && !c.Config.LocalIsRemote() {
		args = append(args, c.Config.Path)
	}
	args = append(args, flags...)
	return append(args, "-c", c.attachPrefix()+sql)
}

// queryCLI shells out to the duckdb binary with the argv from cliArgs.
func (c *QuackClient) queryCLI(ctx context.Context, sql string) (*QueryResult, error) {
	cmd := exec.CommandContext(ctx, c.cliPath, c.cliArgs(sql, "-json")...)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
			return nil, fmt.Errorf("%s", strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, err
	}
	return parseJSONRows(sql, out)
}

// queryHTTP tries a JSON POST to common Quack HTTP endpoint patterns.
func (c *QuackClient) queryHTTP(ctx context.Context, sql string) (*QueryResult, error) {
	body, _ := json.Marshal(map[string]string{"query": sql, "sql": sql})

	for _, ep := range []string{"/query", "/", "/v1/query"} {
		req, err := http.NewRequestWithContext(ctx, "POST",
			c.Config.BaseURL()+ep, bytes.NewReader(body))
		if err != nil {
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		if c.Config.Token != "" {
			req.Header.Set("Authorization", "Bearer "+c.Config.Token)
		}

		resp, err := c.http.Do(req)
		if err != nil {
			continue
		}

		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			resp.Body.Close()
			return nil, fmt.Errorf("auth failed (HTTP %d) — check token", resp.StatusCode)
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
			resp.Body.Close()
			if err != nil {
				return nil, err
			}
			return parseJSONRows(sql, data)
		}
		resp.Body.Close()
	}
	return nil, fmt.Errorf("no endpoint responded (tried /query, /, /v1/query)")
}

// parseJSONRows converts DuckDB's -json output (array of objects) to QueryResult.
func parseJSONRows(query string, data []byte) (*QueryResult, error) {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return &QueryResult{Query: query, Timestamp: time.Now()}, nil
	}

	var rows []map[string]interface{}
	if err := json.Unmarshal(data, &rows); err != nil {
		return nil, fmt.Errorf("unexpected response (not JSON array): %.80s", data)
	}
	if len(rows) == 0 {
		return &QueryResult{Query: query, Timestamp: time.Now()}, nil
	}

	// Stable column order from first row
	cols := make([]string, 0, len(rows[0]))
	for k := range rows[0] {
		cols = append(cols, k)
	}
	sort.Strings(cols)

	r := &QueryResult{Query: query, Columns: cols, Timestamp: time.Now()}
	for _, row := range rows {
		cells := make([]string, len(cols))
		for i, col := range cols {
			cells[i] = fmt.Sprintf("%v", row[col])
		}
		r.Rows = append(r.Rows, cells)
	}
	return r, nil
}

// sessionResultMsg is sent when a session poll completes.
type sessionResultMsg struct {
	// idx identifies which connection this result describes. Without it the
	// dashboard stored one global result set, so with several servers online
	// the last responder won and nothing said whose data was on screen.
	idx         int
	connections []Connection
	// reportedCount is the connection count the backend gave us, if any. It is
	// carried separately from `connections` because DuckDB exposes a count but
	// not a per-connection listing — the two are different facts.
	reportedCount string
	err           error
}

// FetchSessionsCmd polls the server for active connections via the CLI.
// Behaviour by type:
//   - Quack    — queries duckdb_connections() on the remote.
//   - Local    — reports a single "local" session (us).
//   - DuckLake — reports the most recent snapshots as pseudo-sessions.
func (c *QuackClient) FetchSessionsCmd(idx int) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()

		state := c.GetState()
		if !state.Online || !c.hasCLI {
			return sessionResultMsg{idx: idx, err: fmt.Errorf("offline or no CLI")}
		}

		// Real sessions only exist on backends that expose duckdb_connections()
		// over a network. For others we synthesise something useful per type
		// so the dashboard panel isn't empty.
		if c.Config.Supports(CapSessions) {
			return c.fetchQuackSessions(ctx, idx)
		}

		switch c.Config.Type {
		case ConnDuckLake:
			return c.fetchDuckLakeSnapshots(ctx, idx)
		case ConnLocal:
			return sessionResultMsg{idx: idx, connections: []Connection{{
				ID: "loc1", IP: "local", Identity: "duckdb-cli",
				Catalog: filepath.Base(c.Config.Path), Status: "active",
			}}}
		}
		return sessionResultMsg{idx: idx, err: fmt.Errorf("no sessions for %s", c.Config.Type)}
	}
}

// fetchQuackSessions reports what the session is able to know about itself.
//
// This used to query duckdb_connections(), which does not exist in DuckDB —
// the call failed with a Catalog Error on every poll, and the dropped error
// left the panel looking merely empty. DuckDB exposes no per-connection
// listing: current_connection_id(), session_user() and duckdb_connection_count()
// are the whole surface, and metadata functions evaluate wherever the query
// runs. So one row is reported — ours — with the backend's connection count
// alongside it, rather than a fabricated list of peers.
//
// If a Quack server grows a real session-listing function, that is the right
// source for this panel and this query should be replaced by it.
func (c *QuackClient) fetchQuackSessions(ctx context.Context, idx int) tea.Msg {
	sql := `SELECT current_connection_id() AS connection_id,
	               session_user()          AS client_context,
	               current_database()      AS catalog,
	               (SELECT count FROM duckdb_connection_count()) AS connection_count;`
	cmd := exec.CommandContext(ctx, c.cliPath, c.cliArgs(sql, "-json")...)
	out, err := cmd.Output()
	if err != nil {
		return sessionResultMsg{idx: idx, err: fmt.Errorf("session query: %s", cliError(err))}
	}
	conns, reported, err := parseSessionRows(out, c.Config)
	return sessionResultMsg{idx: idx, connections: conns, reportedCount: reported, err: err}
}

// cliError prefers the subprocess's stderr over Go's bare "exit status 1",
// which is what the fetch paths were surfacing (when they surfaced anything).
func cliError(err error) string {
	if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
		return strings.TrimSpace(string(ee.Stderr))
	}
	return err.Error()
}

func (c *QuackClient) fetchDuckLakeSnapshots(ctx context.Context, idx int) tea.Msg {
	// snapshots() is a table macro DuckLake registers in the attached catalog's
	// default schema, expanding to ducklake_snapshots('_lake'); either spelling
	// works once ATTACH ... AS _lake has run.
	sql := "SELECT snapshot_id, snapshot_time, schema_version FROM _lake.snapshots() ORDER BY snapshot_id DESC LIMIT 5;"
	cmd := exec.CommandContext(ctx, c.cliPath, c.cliArgs(sql, "-json")...)
	out, err := cmd.Output()
	if err != nil {
		// Report the failure. This used to substitute a synthetic "lake" row,
		// which made a missing ducklake extension or an unreachable catalog
		// look like a healthy connection with one session.
		return sessionResultMsg{idx: idx, err: fmt.Errorf("snapshot query: %s", cliError(err))}
	}
	conns, err := parseDuckLakeSnapshots(out, c.Config)
	return sessionResultMsg{idx: idx, connections: conns, err: err}
}

// parseDuckLakeSnapshots renders snapshots as "connections" for the dashboard.
func parseDuckLakeSnapshots(data []byte, cfg ServerConfig) ([]Connection, error) {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || string(data) == "[]" {
		return nil, fmt.Errorf("no snapshots")
	}
	var rows []map[string]interface{}
	if err := json.Unmarshal(data, &rows); err != nil {
		return nil, err
	}
	conns := make([]Connection, 0, len(rows))
	for _, row := range rows {
		id := fmt.Sprintf("%v", row["snapshot_id"])
		ver := fmt.Sprintf("%v", row["schema_version"])
		since := time.Duration(0)
		if v, ok := row["snapshot_time"]; ok {
			if t, err := time.Parse(time.RFC3339, fmt.Sprintf("%v", v)); err == nil {
				since = time.Since(t)
			}
		}
		conns = append(conns, Connection{
			ID:       "s" + id,
			IP:       cfg.CatalogPath,
			Identity: "snap v" + ver,
			Catalog:  "_lake",
			Duration: since,
			Status:   "active",
		})
	}
	return conns, nil
}

// parseSessionRows converts the session-facts JSON into []Connection. The
// second return is the connection count the backend reported, verbatim and
// possibly empty — the panel labels it rather than inventing a row per peer.
func parseSessionRows(data []byte, cfg ServerConfig) ([]Connection, string, error) {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || string(data) == "[]" {
		return nil, "", fmt.Errorf("empty")
	}

	var rows []map[string]interface{}
	if err := json.Unmarshal(data, &rows); err != nil || len(rows) == 0 {
		return nil, "", fmt.Errorf("parse error")
	}

	reportedCount := ""
	if v, ok := rows[0]["connection_count"]; ok && v != nil {
		reportedCount = fmt.Sprintf("%v", v)
	}

	conns := make([]Connection, 0, len(rows))
	for i, row := range rows {
		// Both of these are cut to the dashboard column width. cutRunes is
		// used rather than a byte slice: connection_id is frequently a small
		// integer (shorter than the column), and client_context is free-form
		// text that may be multibyte.
		id := fmt.Sprintf("c%02d", i+1)
		if v, ok := row["connection_id"]; ok {
			if s := cutRunes(fmt.Sprintf("%v", v), 4); s != "" {
				id = s
			}
		}

		identity := cfg.Name
		if v, ok := row["client_context"]; ok {
			if s := cutRunes(fmt.Sprintf("%v", v), 16); s != "" {
				identity = s
			}
		}

		since := time.Duration(0)
		if v, ok := row["connected_since"]; ok {
			if t, err := time.Parse(time.RFC3339, fmt.Sprintf("%v", v)); err == nil {
				since = time.Since(t)
			}
		}

		catalog := "_remote"
		if v, ok := row["catalog"]; ok && v != nil {
			if s := cutRunes(fmt.Sprintf("%v", v), 12); s != "" {
				catalog = s
			}
		}

		conns = append(conns, Connection{
			ID:       id,
			IP:       cfg.Host,
			Identity: identity,
			Catalog:  catalog,
			Duration: since,
			Queries:  0,
			Status:   "active",
		})
	}
	return conns, reportedCount, nil
}

// ── catalog fetch ─────────────────────────────────────────────────────────

// catalogResultMsg is sent when a catalog poll completes.
type catalogResultMsg struct {
	idx     int // which connection this catalog belongs to
	catalog []CatalogSchema
	err     error
}

// FetchCatalogCmd lists the relations on whichever backend this client points
// at — works uniformly across Quack, Local, and DuckLake. idx identifies the
// connection so the dashboard can attribute the result.
func (c *QuackClient) FetchCatalogCmd(idx int) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		state := c.GetState()
		if !state.Online || !c.hasCLI {
			return catalogResultMsg{idx: idx, err: fmt.Errorf("offline or no CLI")}
		}

		// information_schema.tables has no estimated_size column — selecting it
		// failed with a Binder Error on every backend, and because the error was
		// dropped by the update loop the catalog panel just stayed empty. Row
		// counts live on duckdb_tables(); views have no size and come from
		// duckdb_views(). Filtering on current_database() keeps the listing to
		// the backend we attached, rather than also reporting the CLI's own
		// in-memory database.
		sql := `SELECT schema_name AS table_schema, table_name, estimated_size,
			           'table' AS object_type
			      FROM duckdb_tables()
			     WHERE database_name = current_database() AND NOT internal
			    UNION ALL
			    SELECT schema_name AS table_schema, view_name AS table_name,
			           NULL AS estimated_size, 'view' AS object_type
			      FROM duckdb_views()
			     WHERE database_name = current_database() AND NOT internal
			    ORDER BY table_schema, table_name;`

		cmd := exec.CommandContext(ctx, c.cliPath, c.cliArgs(sql, "-json")...)
		out, err := cmd.Output()
		if err != nil {
			// stderr carries the Binder/Catalog error; "exit status 1" alone
			// left the panel saying nothing useful.
			return catalogResultMsg{idx: idx, err: fmt.Errorf("catalog query: %s", cliError(err))}
		}
		schemas, err := parseCatalogRows(out)
		return catalogResultMsg{idx: idx, catalog: schemas, err: err}
	}
}

func parseCatalogRows(data []byte) ([]CatalogSchema, error) {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || string(data) == "[]" {
		return nil, fmt.Errorf("empty")
	}
	var rows []map[string]interface{}
	if err := json.Unmarshal(data, &rows); err != nil {
		return nil, err
	}

	schemaMap := make(map[string]*CatalogSchema)
	var order []string
	for _, row := range rows {
		sn := fmt.Sprintf("%v", row["table_schema"])
		tn := fmt.Sprintf("%v", row["table_name"])

		// Views report no size; anything else is a row-count estimate. Reading
		// it as "0 rows" would have been a claim we can't make.
		var n int64
		sizeKnown := false
		if v, ok := row["estimated_size"]; ok && v != nil {
			if _, err := fmt.Sscanf(fmt.Sprintf("%v", v), "%d", &n); err == nil {
				sizeKnown = true
			}
		}

		kind := "table"
		if v, ok := row["object_type"]; ok && v != nil {
			kind = fmt.Sprintf("%v", v)
		}

		if _, ok := schemaMap[sn]; !ok {
			schemaMap[sn] = &CatalogSchema{Name: sn, Open: true}
			order = append(order, sn)
		}
		schemaMap[sn].Tables = append(schemaMap[sn].Tables, CatalogTable{
			Name: tn, Format: kind, Rows: n, SizeKnown: sizeKnown,
		})
	}
	out := make([]CatalogSchema, 0, len(order))
	for _, name := range order {
		out = append(out, *schemaMap[name])
	}
	return out, nil
}

type configFile struct {
	Servers        []ServerConfig  `json:"servers"`
	StorageSecrets []StorageSecret `json:"storage_secrets,omitempty"`
	Tokens         []Token         `json:"tokens,omitempty"`
}

// ConfigFilePath returns the on-disk location of the persisted config.
func ConfigFilePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".duckdb", "pintail.json")
}

// loadConfigFile reads and decodes the whole config file. A missing or
// unparseable file yields an empty struct — every Load* helper below layers its
// own defaults on top, and every Save* helper reads the current file first so
// that writing one section cannot drop another.
func loadConfigFile() configFile {
	var f configFile
	data, err := os.ReadFile(ConfigFilePath())
	if err != nil {
		return f
	}
	if err := json.Unmarshal(data, &f); err != nil {
		return configFile{}
	}
	return f
}

// LoadServerConfigs reads persisted server configs; returns defaults if none.
// Backfills Type=ConnQuack on legacy configs that pre-date the type field.
func LoadServerConfigs() []ServerConfig {
	f := loadConfigFile()
	if len(f.Servers) == 0 {
		return defaultConfigs()
	}
	for i := range f.Servers {
		if f.Servers[i].Type == "" {
			f.Servers[i].Type = ConnQuack
		}
	}
	return f.Servers
}

// LoadStorageSecrets reads persisted storage secrets from the same config file.
// Returns nil if the file or section is missing.
func LoadStorageSecrets() []StorageSecret {
	return loadConfigFile().StorageSecrets
}

// LoadTokens reads persisted Quack tokens. Tokens live in the same file as the
// storage secrets and carry the same plaintext caveat; the file is written
// 0600 and its directory 0700 because both sections are credentials.
func LoadTokens() []Token {
	return loadConfigFile().Tokens
}

// SaveServerConfigs persists server configs, preserving the other sections.
func SaveServerConfigs(cfgs []ServerConfig) error {
	f := loadConfigFile()
	f.Servers = cfgs
	return saveConfigFile(f)
}

// SaveStorageSecrets persists secrets, preserving the other sections.
func SaveStorageSecrets(secrets []StorageSecret) error {
	f := loadConfigFile()
	f.StorageSecrets = secrets
	return saveConfigFile(f)
}

// SaveTokens persists Quack tokens, preserving the other sections. Without
// this, a token created or rotated in the token manager existed only in memory
// and was destroyed on quit — taking the only copy of a rotated value with it.
func SaveTokens(tokens []Token) error {
	f := loadConfigFile()
	f.Tokens = tokens
	return saveConfigFile(f)
}

// saveConfigFile writes the config atomically: a temp file in the same
// directory, then a rename. A crash partway through a direct write left the
// file truncated, which read back as "no connections configured".
func saveConfigFile(f configFile) error {
	path := ConfigFilePath()
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}

	tmp, err := os.CreateTemp(dir, ".pintail-*.json")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeds

	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// InitClients builds a QuackClient for every server config. The resolvers
// passed to each client point at the full slices, so any DuckLake config
// that uses CatalogRef and any connection that uses StorageSecretRef can
// resolve them by name.
func InitClients(cfgs []ServerConfig, secrets []StorageSecret) []*QuackClient {
	cfgResolver := func(name string) (ServerConfig, bool) {
		for _, cfg := range cfgs {
			if cfg.Name == name {
				return cfg, true
			}
		}
		return ServerConfig{}, false
	}
	secResolver := func(name string) (StorageSecret, bool) {
		for _, s := range secrets {
			if s.Name == name {
				return s, true
			}
		}
		return StorageSecret{}, false
	}
	out := make([]*QuackClient, len(cfgs))
	for i, cfg := range cfgs {
		out[i] = NewQuackClient(cfg, cfgResolver, secResolver)
	}
	return out
}

func defaultConfigs() []ServerConfig {
	return []ServerConfig{
		{Name: "localhost", Type: ConnQuack, Host: "localhost", Port: 9494, TLS: false},
	}
}

// ── helpers ───────────────────────────────────────────────────────────────

// simplifyNetErr condenses verbose dial errors to a single short reason.
func simplifyNetErr(err error) string {
	s := err.Error()
	switch {
	case strings.Contains(s, "connection refused"):
		return "connection refused"
	case strings.Contains(s, "no route"):
		return "no route to host"
	case strings.Contains(s, "timeout"):
		return "timeout"
	case strings.Contains(s, "network is unreachable"):
		return "network unreachable"
	}
	return s
}

// storageSecretsEqual reports whether two secret slices have identical content.
// Used by the model to decide whether to rebuild clients after a token-mgr edit.
func storageSecretsEqual(a, b []StorageSecret) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Name != b[i].Name ||
			a[i].Type != b[i].Type ||
			a[i].KeyID != b[i].KeyID ||
			a[i].Secret != b[i].Secret ||
			a[i].Region != b[i].Region ||
			a[i].AccountID != b[i].AccountID ||
			a[i].ConnStr != b[i].ConnStr ||
			a[i].Scope != b[i].Scope {
			return false
		}
	}
	return true
}
