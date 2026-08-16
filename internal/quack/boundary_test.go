package quack

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// This package is the data layer: it talks to duckdb, generates SQL and parses
// results. It must not import a terminal library.
//
// That was the point of splitting it out. Before the split everything lived in
// package main, so the client returned Bubble Tea messages and the only thing
// keeping SQL generation separable from rendering was discipline. Discipline is
// not a boundary; this test is.
//
// If you are here because this test failed: the thing you want almost certainly
// belongs in internal/ui. A client method should return values and errors, and
// the tea.Cmd that carries them to the update loop goes in internal/ui/commands.go.
func TestDataLayerImportsNoTerminalLibrary(t *testing.T) {
	banned := []string{
		"github.com/charmbracelet/bubbletea",
		"github.com/charmbracelet/bubbles",
		"github.com/charmbracelet/lipgloss",
		"github.com/charmbracelet/x/ansi",
		"github.com/muesli/termenv",
		// The sibling packages, too: a dependency from the data layer back up to
		// the screens would invert the whole arrangement.
		"github.com/CathalByrneGit/pintail/internal/ui",
		"github.com/CathalByrneGit/pintail/internal/cli",
	}

	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("globbing package files: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no source files found; this test is not checking anything")
	}

	fset := token.NewFileSet()
	checked := 0
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue // tests may import whatever they need
		}
		f, err := parser.ParseFile(fset, file, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parsing %s: %v", file, err)
		}
		checked++
		for _, imp := range f.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			for _, bad := range banned {
				if path == bad || strings.HasPrefix(path, bad+"/") {
					t.Errorf("%s imports %s — the data layer must not depend on the UI", file, path)
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("every file was skipped; this test is not checking anything")
	}
}
