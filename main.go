package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// Pintail has a small CLI surface alongside the TUI, inspired by msgvault.
// Running with no args launches the TUI; subcommands provide scriptable
// access to the same QuackClient layer.
//
//   pintail                        Launch the TUI (default).
//   pintail list                   List configured connections + live status.
//   pintail ping <name>            Ping one connection by name.
//   pintail query <name> "<sql>"   Execute SQL against one connection.
//                                   Append --json for machine-readable output.
//   pintail version                Print the version.
//   pintail help                   Show this help.

func main() {
	if len(os.Args) > 1 {
		if err := runSubcommand(os.Args[1:]); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}

	m := NewModel()
	p := tea.NewProgram(m, tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "error running Pintail: %v\n", err)
		os.Exit(1)
	}
}

func runSubcommand(args []string) error {
	switch args[0] {
	case "list":
		return cmdList(hasFlag(args, "--json"))
	case "ping":
		if len(args) < 2 {
			return fmt.Errorf("usage: pintail ping <name>")
		}
		return cmdPing(args[1], hasFlag(args, "--json"))
	case "query":
		if len(args) < 3 {
			return fmt.Errorf("usage: pintail query <name> \"<sql>\" [--json]")
		}
		return cmdQuery(args[1], args[2], hasFlag(args, "--json"))
	case "version", "-v", "--version":
		fmt.Println("pintail " + versionLabel())
		return nil
	case "help", "-h", "--help":
		printHelp()
		return nil
	default:
		return fmt.Errorf("unknown subcommand: %q (try 'pintail help')", args[0])
	}
}

func hasFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

func findConfig(name string) (ServerConfig, bool) {
	for _, cfg := range LoadServerConfigs() {
		if cfg.Name == name {
			return cfg, true
		}
	}
	return ServerConfig{}, false
}

// makeResolver returns a ConfigResolver closed over the current on-disk
// connection list — used by the CLI subcommands so DuckLake-with-CatalogRef
// works the same as in the TUI.
func makeResolver() ConfigResolver {
	all := LoadServerConfigs()
	return func(name string) (ServerConfig, bool) {
		for _, cfg := range all {
			if cfg.Name == name {
				return cfg, true
			}
		}
		return ServerConfig{}, false
	}
}

// makeSecretResolver returns a SecretResolver closed over the current on-disk
// storage secrets list — used by the CLI subcommands so connections that
// reference a StorageSecretRef resolve correctly outside the TUI.
func makeSecretResolver() SecretResolver {
	all := LoadStorageSecrets()
	return func(name string) (StorageSecret, bool) {
		for _, s := range all {
			if s.Name == name {
				return s, true
			}
		}
		return StorageSecret{}, false
	}
}

// ── subcommands ───────────────────────────────────────────────────────────

func cmdList(asJSON bool) error {
	configs := LoadServerConfigs()
	if asJSON {
		return json.NewEncoder(os.Stdout).Encode(configs)
	}
	if len(configs) == 0 {
		fmt.Println("no connections configured — add one with 'pintail' (press 'a')")
		return nil
	}
	fmt.Printf("%-20s %-10s %s\n", "NAME", "TYPE", "URI")
	for _, cfg := range configs {
		fmt.Printf("%-20s %-10s %s\n", cfg.Name, cfg.Type, cfg.DisplayURI())
	}
	return nil
}

func cmdPing(name string, asJSON bool) error {
	cfg, ok := findConfig(name)
	if !ok {
		return fmt.Errorf("no connection named %q", name)
	}
	c := NewQuackClient(cfg, makeResolver(), makeSecretResolver())
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
		return json.NewEncoder(os.Stdout).Encode(out)
	}

	if err != nil {
		fmt.Printf("✕ %s offline: %s\n", cfg.Name, err.Error())
		return err
	}
	fmt.Printf("● %s online: %dms (%s)\n", cfg.Name, lat.Milliseconds(), cfg.DisplayURI())
	return nil
}

func cmdQuery(name, sql string, asJSON bool) error {
	cfg, ok := findConfig(name)
	if !ok {
		return fmt.Errorf("no connection named %q", name)
	}
	c := NewQuackClient(cfg, makeResolver(), makeSecretResolver())

	// Best-effort ping so the client knows whether it is online
	pingCtx, pingCancel := context.WithTimeout(context.Background(), 3*time.Second)
	c.Ping(pingCtx)
	pingCancel()

	if !c.GetState().Online {
		return fmt.Errorf("connection %q is offline (%s)", name, c.GetState().ErrMsg)
	}

	ctx, cancel := context.WithTimeout(context.Background(), QueryTimeout())
	defer cancel()

	// Same routing as the TUI, which is the point: a Quack server reachable
	// over HTTP is queryable here without a duckdb binary. This used to reject
	// the query outright unless the CLI was installed.
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
		return json.NewEncoder(os.Stdout).Encode(out)
	}

	// Human-friendly table output to stdout
	if len(result.Columns) == 0 {
		fmt.Println("(no rows)")
		return nil
	}
	for i, col := range result.Columns {
		if i > 0 {
			fmt.Print("\t")
		}
		fmt.Print(col)
	}
	fmt.Println()
	for _, row := range result.Rows {
		for i, cell := range row {
			if i > 0 {
				fmt.Print("\t")
			}
			fmt.Print(cell)
		}
		fmt.Println()
	}
	return nil
}

func printHelp() {
	fmt.Println(`Pintail — DuckDB Quack Protocol Manager

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
