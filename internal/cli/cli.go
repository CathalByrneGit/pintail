package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/CathalByrneGit/pintail/internal/quack"
	"github.com/CathalByrneGit/pintail/internal/version"
)

// The non-interactive surface: the same client layer the TUI drives, reachable
// from a shell script.
//
//	pintail list                   List configured connections.
//	pintail ping <name>            Ping one connection by name.
//	pintail query <name> "<sql>"   Execute SQL against one connection.
//	pintail version                Print the version.
//	pintail help                   Show this help.
//
// Every command writes through an injected io.Writer rather than straight to
// os.Stdout. That is what lets a test assert on the output instead of only on
// the exit status — this whole surface was previously uncovered.

// Env carries the ambient dependencies a command needs, so tests can supply
// their own instead of reaching for the real home directory and stdout.
type Env struct {
	Out io.Writer
	Err io.Writer
	// LoadConfigs and LoadSecrets default to the on-disk config when nil.
	LoadConfigs func() []quack.ServerConfig
	LoadSecrets func() []quack.StorageSecret
	// NewClient defaults to quack.NewQuackClient when nil. A test can return a
	// client pointed at a fixture, or one whose state is forced.
	NewClient func(quack.ServerConfig, quack.ConfigResolver, quack.SecretResolver, ...quack.ClientOption) *quack.QuackClient
}

func (e Env) configs() []quack.ServerConfig {
	if e.LoadConfigs != nil {
		return e.LoadConfigs()
	}
	return quack.LoadServerConfigs()
}

func (e Env) secrets() []quack.StorageSecret {
	if e.LoadSecrets != nil {
		return e.LoadSecrets()
	}
	return quack.LoadStorageSecrets()
}

func (e Env) client(cfg quack.ServerConfig) *quack.QuackClient {
	newClient := e.NewClient
	if newClient == nil {
		newClient = quack.NewQuackClient
	}
	return newClient(cfg, e.resolver(), e.secretResolver())
}

// resolver returns a ConfigResolver closed over the current connection list, so
// DuckLake-with-CatalogRef works the same here as in the TUI.
func (e Env) resolver() quack.ConfigResolver {
	all := e.configs()
	return func(name string) (quack.ServerConfig, bool) {
		for _, cfg := range all {
			if cfg.Name == name {
				return cfg, true
			}
		}
		return quack.ServerConfig{}, false
	}
}

// secretResolver returns a SecretResolver closed over the current storage
// secrets, so a connection with a StorageSecretRef resolves outside the TUI.
func (e Env) secretResolver() quack.SecretResolver {
	all := e.secrets()
	return func(name string) (quack.StorageSecret, bool) {
		for _, s := range all {
			if s.Name == name {
				return s, true
			}
		}
		return quack.StorageSecret{}, false
	}
}

func (e Env) findConfig(name string) (quack.ServerConfig, bool) {
	for _, cfg := range e.configs() {
		if cfg.Name == name {
			return cfg, true
		}
	}
	return quack.ServerConfig{}, false
}

// Run dispatches one subcommand. args excludes the program name.
func Run(env Env, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("no subcommand given")
	}
	switch args[0] {
	case "list":
		return List(env, HasFlag(args, "--json"))
	case "ping":
		if len(args) < 2 {
			return fmt.Errorf("usage: pintail ping <name>")
		}
		return Ping(env, args[1], HasFlag(args, "--json"))
	case "query":
		if len(args) < 3 {
			return fmt.Errorf("usage: pintail query <name> \"<sql>\" [--json]")
		}
		return Query(env, args[1], args[2], HasFlag(args, "--json"))
	case "version", "-v", "--version":
		fmt.Fprintln(env.Out, "pintail "+version.Label())
		return nil
	case "help", "-h", "--help":
		Help(env.Out)
		return nil
	default:
		return fmt.Errorf("unknown subcommand: %q (try 'pintail help')", args[0])
	}
}

// HasFlag reports whether flag appears anywhere in args.
func HasFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

func List(env Env, asJSON bool) error {
	configs := env.configs()
	if asJSON {
		return json.NewEncoder(env.Out).Encode(configs)
	}
	if len(configs) == 0 {
		fmt.Fprintln(env.Out, "no connections configured — add one with 'pintail' (press 'a')")
		return nil
	}
	fmt.Fprintf(env.Out, "%-20s %-10s %s\n", "NAME", "TYPE", "URI")
	for _, cfg := range configs {
		fmt.Fprintf(env.Out, "%-20s %-10s %s\n", cfg.Name, cfg.Type, cfg.DisplayURI())
	}
	return nil
}

func Ping(env Env, name string, asJSON bool) error {
	cfg, ok := env.findConfig(name)
	if !ok {
		return fmt.Errorf("no connection named %q", name)
	}
	c := env.client(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	lat, err := c.Ping(ctx)

	if asJSON {
		out := map[string]interface{}{
			"name":    cfg.Name,
			"type":    cfg.Type,
			"online":  err == nil,
			"latency": lat.Milliseconds(),
		}
		if err != nil {
			out["error"] = err.Error()
		}
		return json.NewEncoder(env.Out).Encode(out)
	}

	if err != nil {
		fmt.Fprintf(env.Out, "✕ %s offline: %s\n", cfg.Name, err.Error())
		return err
	}
	fmt.Fprintf(env.Out, "● %s online: %dms (%s)\n", cfg.Name, lat.Milliseconds(), cfg.DisplayURI())
	return nil
}

func Query(env Env, name, sql string, asJSON bool) error {
	cfg, ok := env.findConfig(name)
	if !ok {
		return fmt.Errorf("no connection named %q", name)
	}
	c := env.client(cfg)

	// Best-effort ping so the client knows whether it is online.
	pingCtx, pingCancel := context.WithTimeout(context.Background(), 3*time.Second)
	c.Ping(pingCtx)
	pingCancel()

	if !c.GetState().Online {
		return fmt.Errorf("connection %q is offline (%s)", name, c.GetState().ErrMsg)
	}

	ctx, cancel := context.WithTimeout(context.Background(), quack.QueryTimeout())
	defer cancel()

	result := c.Query(ctx, sql)
	if result.Err != "" {
		return fmt.Errorf("%s", result.Err)
	}

	if asJSON {
		out := map[string]interface{}{
			"columns":    result.Columns,
			"rows":       result.Rows,
			"elapsed_ms": result.ElapsedMs,
		}
		return json.NewEncoder(env.Out).Encode(out)
	}

	if len(result.Columns) == 0 {
		fmt.Fprintln(env.Out, "(no rows)")
		return nil
	}
	for i, col := range result.Columns {
		if i > 0 {
			fmt.Fprint(env.Out, "\t")
		}
		fmt.Fprint(env.Out, col)
	}
	fmt.Fprintln(env.Out)
	for _, row := range result.Rows {
		for i, cell := range row {
			if i > 0 {
				fmt.Fprint(env.Out, "\t")
			}
			fmt.Fprint(env.Out, cell)
		}
		fmt.Fprintln(env.Out)
	}
	return nil
}

func Help(w io.Writer) {
	fmt.Fprintln(w, `Pintail — DuckDB Quack Protocol Manager

Usage:
  pintail                          Launch the TUI (default).
  pintail list [--json]            List configured connections.
  pintail ping <name> [--json]     Ping one connection.
  pintail query <name> "<sql>" [--json]
                                    Execute SQL against one connection.
  pintail version                  Print the version.
  pintail help                     Show this help.

Connection types (set in the TUI's "Add Connection" screen):
  quack      Remote Quack server  (host:port, optional token, TLS via reverse proxy)
  local      Local .duckdb file   (path to file on disk)
  ducklake   DuckLake lakehouse   (catalog DB URL + object storage path)

Config file:  ~/.duckdb/pintail.json`)
}
