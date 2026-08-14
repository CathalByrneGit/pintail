package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestQueryTimeoutOverride(t *testing.T) {
	tests := []struct {
		env  string
		want time.Duration
	}{
		{"", defaultQueryTimeout},
		{"5", 5 * time.Second},
		{"600", 600 * time.Second},
		{"0", defaultQueryTimeout},      // non-positive is ignored
		{"-3", defaultQueryTimeout},     // as is negative
		{"banana", defaultQueryTimeout}, /* and unparseable */
	}
	for _, tc := range tests {
		t.Run("PINTAIL_QUERY_TIMEOUT="+tc.env, func(t *testing.T) {
			if tc.env == "" {
				os.Unsetenv("PINTAIL_QUERY_TIMEOUT")
			} else {
				t.Setenv("PINTAIL_QUERY_TIMEOUT", tc.env)
			}
			if got := QueryTimeout(); got != tc.want {
				t.Errorf("QueryTimeout() = %v, want %v", got, tc.want)
			}
		})
	}
}

// A cancelled query has to be reported as cancelled, not as whatever the killed
// subprocess said on its way out ("signal: killed").
func TestQueryCancellationIsReportedAsSuch(t *testing.T) {
	if _, err := exec.LookPath("duckdb"); err != nil {
		t.Skip("duckdb not in PATH — skipping integration test")
	}

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "fixture.duckdb")
	if out, err := exec.Command("duckdb", dbPath, "-c", "SELECT 1;").CombinedOutput(); err != nil {
		t.Fatalf("seeding: %v\n%s", err, out)
	}

	c := NewQuackClient(ServerConfig{Name: "local", Type: ConnLocal, Path: dbPath}, nil, nil)
	if _, err := c.Ping(context.Background()); err != nil {
		t.Fatalf("ping: %v", err)
	}

	t.Run("cancel", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan *QueryResult, 1)
		go func() {
			// A query long enough that it is still running when we cancel.
			done <- c.Query(ctx, "SELECT count(*) FROM range(2000000000);")
		}()

		time.Sleep(150 * time.Millisecond)
		cancel()

		select {
		case r := <-done:
			if r.Err != "cancelled" {
				t.Errorf("Err = %q, want %q", r.Err, "cancelled")
			}
		case <-time.After(20 * time.Second):
			t.Fatal("cancelling the context did not stop the query")
		}
	})

	t.Run("deadline", func(t *testing.T) {
		t.Setenv("PINTAIL_QUERY_TIMEOUT", "1")
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()

		r := c.Query(ctx, "SELECT count(*) FROM range(2000000000);")
		if !strings.HasPrefix(r.Err, "timed out") {
			t.Errorf("Err = %q, want a timeout", r.Err)
		}
	})
}

// The scratchpad has to hold onto the cancel func while a query is in flight,
// and clear it when the result lands.
func TestScratchpadTracksCancellation(t *testing.T) {
	cfg := ServerConfig{Name: "local", Type: ConnLocal, Path: "/tmp/whatever.duckdb"}
	c := NewQuackClient(cfg, nil, nil)
	c.state = ConnState{Online: true}

	sp := NewScratchpad([]ServerInfo{cfg.ToServerInfo()}, []*QuackClient{c})
	sp.Resize(100, 40)
	sp.editor.SetValue("SELECT 1;")

	sp, cmd := sp.runQuery()
	if !sp.Running() {
		t.Fatal("Running() should be true with a query in flight")
	}
	if sp.cancelQuery == nil {
		t.Fatal("no cancel func was kept, so nothing could interrupt the query")
	}
	if cmd == nil {
		t.Fatal("runQuery returned no command")
	}

	// The status line tells the user how to interrupt.
	if status := sp.ViewResultsStatus(); !strings.Contains(status, "interrupt") {
		t.Errorf("running status should mention interrupting, got %q", status)
	}

	// ctrl+c while running cancels rather than falling through to other keys.
	sp, _ = sp.Update(key("ctrl+c"))
	if sp.cancelQuery != nil {
		t.Error("cancel func should be cleared once used")
	}

	// The result arriving clears the running state.
	sp, _ = sp.Update(queryResultMsg{result: &QueryResult{Query: "SELECT 1;", Err: "cancelled", Method: "cli"}})
	if sp.Running() {
		t.Error("Running() should be false once a result arrives")
	}
}

// ctrl+c quits the app normally, but interrupts the query when one is running —
// otherwise the only way out of a slow query is killing the process.
func TestRootModelRoutesCtrlCWhileQuerying(t *testing.T) {
	cfg := ServerConfig{Name: "local", Type: ConnLocal, Path: "/tmp/whatever.duckdb"}
	c := NewQuackClient(cfg, nil, nil)
	c.state = ConnState{Online: true}

	m := Model{
		configs:     []ServerConfig{cfg},
		clients:     []*QuackClient{c},
		data:        make([]connData, 1),
		currentView: viewScratchpad,
		width:       100,
		height:      40,
	}
	m.connTable = buildConnectionTable(nil)
	m.scratchpad = NewScratchpad([]ServerInfo{cfg.ToServerInfo()}, []*QuackClient{c})
	m.scratchpad.Resize(100, 40)

	// Not running: ctrl+c quits.
	_, cmd := m.Update(key("ctrl+c"))
	if cmd == nil {
		t.Fatal("ctrl+c with no query running should quit")
	}
	if msg := cmd(); msg == nil || fmt.Sprintf("%T", msg) != "tea.QuitMsg" {
		t.Errorf("expected a quit message, got %T", msg)
	}

	// Running: ctrl+c is handed to the scratchpad and the app stays up.
	m.scratchpad.editor.SetValue("SELECT 1;")
	m.scratchpad, _ = m.scratchpad.runQuery()
	if !m.scratchpad.Running() {
		t.Fatal("expected a query in flight")
	}

	next, cmd := m.Update(key("ctrl+c"))
	if cmd != nil {
		if msg := cmd(); msg != nil && fmt.Sprintf("%T", msg) == "tea.QuitMsg" {
			t.Error("ctrl+c during a query should not quit the app")
		}
	}
	if next.(Model).scratchpad.cancelQuery != nil {
		t.Error("the query should have been cancelled")
	}

	// esc while running cancels too, and stays on the screen.
	m.scratchpad, _ = m.scratchpad.runQuery()
	next, _ = m.Update(key("esc"))
	if got := next.(Model).currentView; got != viewScratchpad {
		t.Errorf("esc during a query left the screen (view = %v)", got)
	}
}

// The CLI subcommand used to refuse outright without a duckdb binary, even for
// a Quack server it could reach over HTTP.
func TestQueryOverHTTPWithoutTheCLI(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if r.URL.Path != "/query" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `[{"answer":42}]`)
	}))
	defer srv.Close()

	host, port := splitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	cfg := ServerConfig{Name: "quack", Type: ConnQuack, Host: host, Token: "qk_secret"}
	fmt.Sscanf(port, "%d", &cfg.Port)

	c := NewQuackClient(cfg, nil, nil)
	c.hasCLI = false // the point of the test
	if _, err := c.Ping(context.Background()); err != nil {
		t.Fatalf("ping: %v", err)
	}

	r := c.Query(context.Background(), "SELECT 42 AS answer;")
	if r.Err != "" {
		t.Fatalf("query failed without the CLI: %s", r.Err)
	}
	if r.Method != "http" {
		t.Errorf("Method = %q, want http", r.Method)
	}
	if len(r.Rows) != 1 || r.Rows[0][0] != "42" {
		t.Errorf("rows = %v, want [[42]]", r.Rows)
	}
	if gotAuth != "Bearer qk_secret" {
		t.Errorf("Authorization = %q, want the bearer token", gotAuth)
	}
}
