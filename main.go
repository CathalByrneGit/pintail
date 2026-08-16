package main

import (
	"fmt"
	"os"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/CathalByrneGit/pintail/internal/cli"
	"github.com/CathalByrneGit/pintail/internal/ui"
)

// Pintail is a terminal admin console for DuckDB Quack servers and DuckLake
// lakehouses. Running with no arguments launches the TUI; the subcommands in
// internal/cli give the same client layer a scriptable surface.
//
// Everything else lives behind internal/: quack is the data layer (it talks to
// duckdb and generates SQL, and imports no terminal library at all), ui draws
// the screens, cli is the non-interactive commands. Keeping this file thin is
// the point — there is nothing here to test.

func main() {
	if len(os.Args) > 1 {
		env := cli.Env{Out: os.Stdout, Err: os.Stderr}
		if err := cli.Run(env, os.Args[1:]); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}

	p := tea.NewProgram(ui.NewModel(), tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "error running Pintail: %v\n", err)
		os.Exit(1)
	}
}
