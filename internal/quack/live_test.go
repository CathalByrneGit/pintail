package quack

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Tests against a running Quack server.
//
// Everything else in this package is verified against the extension's source and
// its own test suite, which is a good deal better than memory but still not
// execution: it cannot tell you that ATTACH accepts the option list Pintail
// emits, that quack_query reaches the server rather than describing us, or that
// duckdb_logs_parsed('Quack') has the columns the log screen reads. These do.
//
// The server is started by CI, which sets:
//
//	PINTAIL_TEST_QUACK_HOST   127.0.0.1
//	PINTAIL_TEST_QUACK_PORT   9494
//	PINTAIL_TEST_QUACK_TOKEN  the token passed to quack_serve
//
// Without those the tests skip, because the quack extension needs a download
// that not every environment allows.

// liveConfig returns the connection for the CI-provided server, or skips.
func liveConfig(t *testing.T) ServerConfig {
	t.Helper()

	host := os.Getenv("PINTAIL_TEST_QUACK_HOST")
	token := os.Getenv("PINTAIL_TEST_QUACK_TOKEN")
	portStr := os.Getenv("PINTAIL_TEST_QUACK_PORT")
	if host == "" || token == "" || portStr == "" {
		t.Skip("no live Quack server configured (PINTAIL_TEST_QUACK_{HOST,PORT,TOKEN})")
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("PINTAIL_TEST_QUACK_PORT = %q: %v", portStr, err)
	}

	return ServerConfig{
		Name: "live", Type: ConnQuack,
		Host: host, Port: port, Token: token,
		TLS: false, // plaintext loopback, so DISABLE_SSL must come out true
	}
}

func liveClient(t *testing.T) *QuackClient {
	t.Helper()
	c := NewQuackClient(liveConfig(t), nil, nil)
	if !c.HasCLI() {
		t.Fatal("duckdb CLI not found, but a live server was configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := c.Ping(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}
	return c
}

// The ping goes to GET / and looks for the server's banner, which is what lets
// the header distinguish a confirmed Quack endpoint from "something answered".
func TestLivePingIdentifiesTheServer(t *testing.T) {
	c := liveClient(t)

	st := c.GetState()
	if !st.Online {
		t.Fatalf("state = %+v, want online", st)
	}
	if st.Method != "quack" {
		t.Errorf("Method = %q, want quack: the banner should have identified the server", st.Method)
	}
	if st.Latency <= 0 {
		t.Errorf("Latency = %v, want a measurement", st.Latency)
	}
}

// The ATTACH option list is the thing most likely to be wrong, since it is
// assembled by us and only the server can say whether it parses.
func TestLiveQueryThroughAttach(t *testing.T) {
	c := liveClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	res := c.Query(ctx, "SELECT 41 + 1 AS answer;")
	if res.Err != "" {
		t.Fatalf("query failed: %s\n--- prologue:\n%s", res.Err, c.attachPrefix())
	}
	if len(res.Rows) != 1 || len(res.Columns) != 1 || res.Columns[0] != "answer" {
		t.Fatalf("got columns %v rows %v", res.Columns, res.Rows)
	}
	if res.Rows[0][0] != "42" {
		t.Errorf("row = %v, want 42", res.Rows[0])
	}

	// The seeded table has to be reachable by an unqualified name, which is what
	// the USE in the prologue is for.
	res = c.Query(ctx, "SELECT count(*) AS n FROM ci_events;")
	if res.Err != "" {
		t.Fatalf("unqualified table name did not resolve: %s", res.Err)
	}
	if len(res.Rows) != 1 || res.Rows[0][0] != "1000" {
		t.Errorf("count = %v, want 1000", res.Rows)
	}

	// And a real SQL error must arrive as the server's message.
	res = c.Query(ctx, "SELECT no_such_column FROM ci_events;")
	if res.Err == "" {
		t.Fatal("want an error for an invalid column")
	}
	if !strings.Contains(res.Err, "no_such_column") {
		t.Errorf("Err = %q, want the server's own message", res.Err)
	}
}

// The catalog query names duckdb_tables()/duckdb_views() and filters on
// current_database(); over a Quack attach that has to resolve to the server's
// catalog, not the local CLI's in-memory one.
func TestLiveCatalog(t *testing.T) {
	c := liveClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	schemas, err := c.Catalog(ctx)
	if err != nil {
		t.Fatalf("Catalog: %v", err)
	}

	found := map[string]CatalogTable{}
	for _, s := range schemas {
		for _, tbl := range s.Tables {
			found[s.Name+"."+tbl.Name] = tbl
		}
	}
	tbl, ok := found["main.ci_events"]
	if !ok {
		t.Fatalf("ci_events missing from the catalog; got %v", found)
	}
	if !tbl.SizeKnown || tbl.Rows != 1000 {
		t.Errorf("ci_events = %+v, want 1000 rows with a known size", tbl)
	}
	if _, ok := found["main.ci_recent"]; !ok {
		t.Errorf("the view is missing from the catalog; got %v", found)
	}
	if found["main.ci_recent"].Format != "view" {
		t.Errorf("ci_recent = %+v, want it reported as a view", found["main.ci_recent"])
	}
}

// quack_active_connections() reports on whichever process evaluates it, so this
// is the test that proves quack_query hands the statement to the server instead
// of running it in the CLI we just started. A self-report would describe one
// connection with no server-side state.
func TestLiveSessions(t *testing.T) {
	c := liveClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conns, _, err := c.Sessions(ctx)
	if err != nil {
		t.Fatalf("Sessions: %v", err)
	}
	// The server has at least the connection this call arrived on. The count is
	// not fixed — Pintail's own polls come and go — so assert the shape.
	if len(conns) == 0 {
		t.Fatal("no sessions reported; quack_active_connections() found nothing")
	}
	for i, conn := range conns {
		if conn.ID == "" {
			t.Errorf("session %d has no id: %+v", i, conn)
		}
		if conn.Status == "" {
			t.Errorf("session %d has no state: %+v", i, conn)
		}
		// The dashboard gives each reported state its own glyph; an unexpected
		// one means the extension's enum moved.
		switch conn.Status {
		case "idle", "active", "finished", "cancelled", "unknown":
		default:
			t.Errorf("session %d has state %q, which is not in the extension's enum", i, conn.Status)
		}
	}
}

// The log screen reads duckdb_logs_parsed('Quack'), whose columns come from the
// extension. Enabling logging is a server-side state change, which is why it is
// bound to a key rather than done by the poll — and why it is worth proving it
// works.
func TestLiveLogging(t *testing.T) {
	c := liveClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	// Enabling returns what is visible immediately afterwards, on the same
	// connection. Anything Pintail has already sent this server is in there.
	entries, err := c.EnableLogging(ctx)
	if err != nil {
		t.Fatalf("EnableLogging: %v", err)
	}

	// Generate more traffic and read again. A separate quack_query is a new
	// connection, so this also tells us whether the setting outlives the
	// connection that set it.
	if res := c.Query(ctx, "SELECT 'log me' AS marker;"); res.Err != "" {
		t.Fatalf("generating traffic: %s", res.Err)
	}
	later, err := c.Logs(ctx)
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	if len(later) > 0 {
		entries = later
	}

	if len(entries) == 0 {
		t.Fatal("no log entries either on the enabling connection or a later one")
	}

	// The ORDER BY timestamp in logSQL only works if that column exists, so
	// getting here at all proves it. Check the fields the screen actually shows.
	var sawType bool
	for _, e := range entries {
		if e.MessageType != "" {
			sawType = true
		}
		if e.Raw == nil {
			t.Error("entry has no raw row, so the detail panel would be empty")
		}
	}
	if !sawType {
		t.Error("no entry carried a message_type; the column name may have changed")
	}

	// Pintail's own poll must not appear in the log it is reading.
	for _, e := range entries {
		if strings.Contains(e.Query, "duckdb_logs_parsed") {
			t.Errorf("the log poll is reporting itself: %q", e.Query)
		}
	}
}

// The authorization hook is the sharpest thing Pintail can do to a server, so
// the read-back, the install and the reset are all exercised — and the hook is
// restored to the default afterwards regardless of what fails.
func TestLiveAuthorizationHook(t *testing.T) {
	c := liveClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := c.RunServerSQL(cleanupCtx, "RESET GLOBAL quack_authorization_function"); err != nil {
			t.Errorf("could not restore the default hook: %v", err)
		}
	})

	// A fresh server reports the shipped allow-all callback, not an empty
	// string. Pintail's conflict check depends on that value being what we think.
	got, err := c.ServerSetting(ctx, AuthzSetting)
	if err != nil {
		t.Fatalf("ServerSetting: %v", err)
	}
	if got != AuthzDefault {
		t.Errorf("%s = %q on a fresh server, want quack_nop_authorization — "+
			"hookIsForeign treats that value as 'nothing installed'", AuthzSetting, got)
	}

	// Install a permissive hook through the same path the auth screen uses.
	install := "CREATE OR REPLACE MACRO pintail_authz(sid, query) AS true; " +
		"SET GLOBAL quack_authorization_function = 'pintail_authz';"
	if err := c.RunServerSQL(ctx, install); err != nil {
		t.Fatalf("installing the hook: %v", err)
	}

	got, err = c.ServerSetting(ctx, AuthzSetting)
	if err != nil {
		t.Fatalf("reading the setting back: %v", err)
	}
	if got != "pintail_authz" {
		t.Fatalf("%s = %q after install, want pintail_authz", AuthzSetting, got)
	}

	// With the hook admitting everything, queries still work.
	if res := c.Query(ctx, "SELECT 1 AS ok;"); res.Err != "" {
		t.Errorf("query rejected under an allow-all hook: %s", res.Err)
	}

	// RESET restores the default — the escape hatch the [R] key offers.
	if err := c.RunServerSQL(ctx, "RESET GLOBAL "+AuthzSetting); err != nil {
		t.Fatalf("resetting the hook: %v", err)
	}
	got, err = c.ServerSetting(ctx, AuthzSetting)
	if err != nil {
		t.Fatalf("reading the setting after reset: %v", err)
	}
	if got != AuthzDefault {
		t.Errorf("%s = %q after RESET, want the default back", AuthzSetting, got)
	}
}

// A wrong token must be rejected, and the failure must reach the user as the
// server's authentication error rather than a transport guess.
func TestLiveWrongTokenIsRejected(t *testing.T) {
	cfg := liveConfig(t)
	cfg.Token = "definitely-not-the-token"

	c := NewQuackClient(cfg, nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// The banner probe is unauthenticated, so reachability still succeeds — it
	// is the query that has to fail.
	if _, err := c.Ping(ctx); err != nil {
		t.Fatalf("ping should still reach the server: %v", err)
	}

	res := c.Query(ctx, "SELECT 1;")
	if res.Err == "" {
		t.Fatal("a wrong token should not be able to query")
	}
	if !strings.Contains(strings.ToLower(res.Err), "authentication") {
		t.Errorf("Err = %q, want the server's authentication failure", res.Err)
	}
}
