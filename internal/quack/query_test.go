package quack

import (
	"context"
	"fmt"
	"net"
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

// The ping for a Quack connection goes over HTTP to the server's banner
// endpoint, which is what lets it tell a Quack server apart from any other
// process holding the port. The query path that used to POST JSON at invented
// endpoints is gone: no Quack server ever served them.
func TestPingQuackIdentifiesTheEndpoint(t *testing.T) {
	tests := []struct {
		name          string
		body          string
		status        int
		wantOnline    bool
		wantMethod    string
		wantConfirmed bool
	}{
		{
			name:          "a real Quack server identifies itself",
			body:          "This is a DuckDB Quack RPC endpoint. Use ATTACH 'quack:...' to connect here.\n",
			status:        200,
			wantOnline:    true,
			wantMethod:    "quack",
			wantConfirmed: true,
		},
		{
			name:       "some other HTTP server is reachable but unidentified",
			body:       "<html>nginx</html>",
			status:     200,
			wantOnline: true,
			wantMethod: "http",
		},
		{
			// The Caddyfile Pintail generates answers 401 to unauthenticated
			// requests, so a refusal is a working deployment, not an outage.
			name:       "a proxy that refuses GET / still counts as reachable",
			body:       "",
			status:     401,
			wantOnline: true,
			wantMethod: "http",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" || r.URL.Path != "/" {
					http.NotFound(w, r)
					return
				}
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer srv.Close()

			cfg := quackConfigFor(t, srv.URL)
			c := NewQuackClient(cfg, nil, nil)

			_, err := c.Ping(context.Background())
			st := c.GetState()
			if (err == nil) != tc.wantOnline {
				t.Fatalf("ping err = %v, want online = %v", err, tc.wantOnline)
			}
			if st.Online != tc.wantOnline {
				t.Errorf("Online = %v, want %v", st.Online, tc.wantOnline)
			}
			if st.Method != tc.wantMethod {
				t.Errorf("Method = %q, want %q", st.Method, tc.wantMethod)
			}

			confirmed, err := c.probeQuackHTTP(context.Background())
			if err != nil {
				t.Fatalf("probe: %v", err)
			}
			if confirmed != tc.wantConfirmed {
				t.Errorf("confirmed = %v, want %v", confirmed, tc.wantConfirmed)
			}
		})
	}
}

func TestPingQuackOfflineWhenNothingListens(t *testing.T) {
	// Port 1 on loopback: nothing is there.
	c := NewQuackClient(ServerConfig{Name: "q", Type: ConnQuack, Host: "127.0.0.1", Port: 1}, nil, nil)
	if _, err := c.Ping(context.Background()); err == nil {
		t.Fatal("want an error when nothing is listening")
	}
	if st := c.GetState(); st.Online || st.ErrMsg == "" {
		t.Errorf("state = %+v, want offline with a reason", st)
	}
}

// The ping sends the token, so a server behind auth can still be probed.
func TestPingQuackSendsTheToken(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		fmt.Fprint(w, "This is a DuckDB Quack RPC endpoint.\n")
	}))
	defer srv.Close()

	cfg := quackConfigFor(t, srv.URL)
	cfg.Token = "qk_secret"
	c := NewQuackClient(cfg, nil, nil)
	if _, err := c.Ping(context.Background()); err != nil {
		t.Fatalf("ping: %v", err)
	}
	if gotAuth != "Bearer qk_secret" {
		t.Errorf("Authorization = %q, want the bearer token", gotAuth)
	}
}

// quackConfigFor builds a plaintext Quack config pointing at a test server.
func quackConfigFor(t *testing.T, url string) ServerConfig {
	t.Helper()
	host, port, err := net.SplitHostPort(strings.TrimPrefix(url, "http://"))
	if err != nil {
		t.Fatalf("parsing test server address %q: %v", url, err)
	}
	cfg := ServerConfig{Name: "quack", Type: ConnQuack, Host: host}
	if _, err := fmt.Sscanf(port, "%d", &cfg.Port); err != nil {
		t.Fatalf("parsing test server port %q: %v", port, err)
	}
	return cfg
}
