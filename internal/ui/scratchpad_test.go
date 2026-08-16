package ui

import (
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/CathalByrneGit/pintail/internal/quack"
)

// Deleting connections in the connection manager replaced the scratchpad's
// server list without clamping serverIdx, so the next render of the screen
// indexed past the end of the shorter slice and took the app down.
func TestSetTargetsClampsSelection(t *testing.T) {
	three := []quack.ServerInfo{{Name: "a"}, {Name: "b"}, {Name: "c"}}

	tests := []struct {
		name        string
		startIdx    int
		newServers  []quack.ServerInfo
		wantIdx     int
		wantHasName string
	}{
		{
			name:        "selection inside the new bounds is kept",
			startIdx:    1,
			newServers:  three,
			wantIdx:     1,
			wantHasName: "b",
		},
		{
			name:        "selection past the end clamps to the last entry",
			startIdx:    2,
			newServers:  three[:1],
			wantIdx:     0,
			wantHasName: "a",
		},
		{
			name:       "empty list leaves no target",
			startIdx:   2,
			newServers: nil,
			wantIdx:    0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sp := NewScratchpad(three, nil)
			sp.serverIdx = tc.startIdx

			sp.SetTargets(tc.newServers, nil)

			if sp.serverIdx != tc.wantIdx {
				t.Errorf("serverIdx = %d, want %d", sp.serverIdx, tc.wantIdx)
			}
			srv, ok := sp.target()
			if ok != (len(tc.newServers) > 0) {
				t.Fatalf("target() ok = %v, want %v", ok, len(tc.newServers) > 0)
			}
			if ok && srv.Name != tc.wantHasName {
				t.Errorf("target name = %q, want %q", srv.Name, tc.wantHasName)
			}
			// The regression: this used to panic.
			_ = sp.ViewEditor()
		})
	}
}

func TestScratchpadWithNoTargets(t *testing.T) {
	sp := NewScratchpad(nil, nil)
	sp.Resize(100, 40)

	view := sp.ViewEditor()
	if !strings.Contains(view, "add a connection") {
		t.Errorf("editor view should explain the empty state, got:\n%s", view)
	}

	sp.editor.SetValue("SELECT 1;")
	next, cmd := sp.runQuery()
	if cmd == nil {
		t.Fatal("runQuery returned no command")
	}
	if next.running {
		t.Error("running should stay false when there is nothing to query")
	}

	msg, ok := cmd().(queryResultMsg)
	if !ok {
		t.Fatalf("got %T, want queryResultMsg", cmd())
	}
	if msg.result == nil || !strings.Contains(msg.result.Err, "no connection configured") {
		t.Errorf("want a 'no connection configured' error, got %+v", msg.result)
	}
}

func TestTruncate(t *testing.T) {
	tests := []struct {
		name string
		in   string
		n    int
		want string
	}{
		{"fits", "abc", 5, "abc"},
		{"exact", "abcde", 5, "abcde"},
		{"cut with ellipsis", "abcdefgh", 5, "abcd…"},
		{"tiny width has no room for an ellipsis", "abcdefgh", 1, "a"},
		{"zero width yields nothing", "abcdefgh", 0, ""},
		{"negative width yields nothing", "abcdefgh", -7, ""},
		// Widths are terminal cells, not runes: each of these characters
		// occupies two cells, so three of them do not fit in five.
		{"wide characters are measured in cells", "日本語", 5, "日本…"},
		{"wide characters that fit are untouched", "日本語", 6, "日本語"},
		{"wide cut leaves room for the ellipsis", "日本語テスト", 7, "日本語…"},
		{"emoji counts as two cells", "🦆🦆🦆", 4, "🦆…"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := truncate(tc.in, tc.n)
			if got != tc.want {
				t.Errorf("truncate(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
			}
		})
	}
}

// Both helpers are handed styled strings by the views (a muted "(no scope)",
// for instance). Escape sequences occupy no cells and must survive intact:
// slicing bytes could sever one and leave the rest of the line coloured by the
// fragment.
func TestTruncateAndPadRightHandleStyledStrings(t *testing.T) {
	// Written as literal escapes rather than via lipgloss: with no TTY attached
	// lipgloss renders plain text, which would make this test pass for the
	// wrong reason.
	const styled = "\x1b[38;5;244m(no scope)\x1b[0m" // 10 visible cells
	if got := ansi.StringWidth(styled); got != 10 {
		t.Fatalf("fixture width = %d, want 10", got)
	}

	if got := truncate(styled, 20); got != styled {
		t.Errorf("a styled string that fits should be untouched")
	}

	cut := truncate(styled, 6)
	if w := ansi.StringWidth(cut); w != 6 {
		t.Errorf("truncated width = %d cells, want 6 (%q)", w, cut)
	}
	if !strings.Contains(cut, "…") {
		t.Errorf("truncated styled string lost its ellipsis: %q", cut)
	}
	if strings.Count(cut, "\x1b[") < 1 {
		t.Errorf("truncation stripped the styling entirely: %q", cut)
	}

	padded := padRight(styled, 15)
	if w := ansi.StringWidth(padded); w != 15 {
		t.Errorf("padded width = %d cells, want 15", w)
	}

	// Padding measures cells, so a wide string pads by the right amount.
	if w := ansi.StringWidth(padRight("日本語", 10)); w != 10 {
		t.Errorf("wide string padded to %d cells, want 10", w)
	}
	// Nothing to pad when it already overflows.
	if got := padRight("日本語テスト", 4); got != "日本語テスト" {
		t.Errorf("padRight should not alter an overlong string, got %q", got)
	}
}

// Columns that don't fit are dropped to keep the layout intact; the reader has
// to be told, or a truncated result looks complete.
func TestRenderResultTableReportsDroppedColumns(t *testing.T) {
	r := quack.QueryResult{
		Columns: []string{"col_one", "col_two", "col_three", "col_four"},
		Rows:    [][]string{{"aaaaaaa", "bbbbbbb", "ccccccc", "ddddddd"}},
	}

	// Wide enough for everything: no notice.
	full := renderResultTable(r, 200)
	if strings.Contains(full, "more column") {
		t.Errorf("no columns should be dropped at width 200:\n%s", full)
	}
	for _, col := range r.Columns {
		if !strings.Contains(full, col) {
			t.Errorf("column %q missing from the full render", col)
		}
	}

	// Narrow: some columns go, and the notice says how many.
	narrow := renderResultTable(r, 30)
	if !strings.Contains(narrow, "more column") {
		t.Errorf("dropped columns were not reported:\n%s", narrow)
	}
	if !strings.Contains(narrow, "col_one") {
		t.Errorf("the first column should survive:\n%s", narrow)
	}
	if strings.Contains(narrow, "col_four") {
		t.Errorf("the last column should have been dropped:\n%s", narrow)
	}

	// Singular wording when exactly one is dropped.
	two := quack.QueryResult{
		Columns: []string{"a", "b"},
		Rows:    [][]string{{"1", "2"}},
	}
	if got := renderResultTable(two, 4); !strings.Contains(got, "+ 1 more column ") &&
		!strings.Contains(got, "+ 1 more column—") && !strings.Contains(got, "+ 1 more column") {
		t.Errorf("want singular wording for one dropped column:\n%s", got)
	}
}

// Wide characters must not push a table out of alignment: every rendered row
// has to be the same width as the header.
func TestRenderResultTableAlignsWideCharacters(t *testing.T) {
	r := quack.QueryResult{
		Columns: []string{"name", "note"},
		Rows: [][]string{
			{"ascii", "plain"},
			{"日本語", "テスト"},
			{"🦆", "duck"},
		},
	}

	out := renderResultTable(r, 100)
	var widths []int
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		widths = append(widths, ansi.StringWidth(line))
	}
	if len(widths) < 4 {
		t.Fatalf("expected a header, a rule and three rows, got %d lines:\n%s", len(widths), out)
	}
	for i, w := range widths {
		if w != widths[0] {
			t.Errorf("line %d is %d cells wide, want %d (all lines must align):\n%s",
				i, w, widths[0], out)
		}
	}
}

func TestCutRunes(t *testing.T) {
	tests := []struct {
		in   string
		n    int
		want string
	}{
		{"1", 4, "1"},
		{"c0128afe", 4, "c012"},
		{"", 4, ""},
		{"abc", 0, ""},
		{"abc", -1, ""},
		{"日本語テスト", 3, "日本語"},
	}
	for _, tc := range tests {
		if got := quack.CutRunes(tc.in, tc.n); got != tc.want {
			t.Errorf("quack.CutRunes(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
		}
	}
}

func TestHrule(t *testing.T) {
	tests := []struct {
		n    int
		want string
	}{
		{-5, ""},
		{0, ""},
		{1, "─"},
		{3, "───"},
	}
	for _, tc := range tests {
		if got := hrule(tc.n); got != tc.want {
			t.Errorf("hrule(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}

// Exports used to be named from a whole-second timestamp and written with
// os.Create, so two exports in the same second silently overwrote each other —
// easy to hit, since the CSV and Parquet keys sit next to each other.
func TestExportPathDoesNotCollideWithinASecond(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // windows

	seen := map[string]bool{}
	for i := 0; i < 5; i++ {
		path, err := exportPath("csv")
		if err != nil {
			t.Fatalf("exportPath: %v", err)
		}
		if seen[path] {
			t.Fatalf("exportPath returned %q twice; the earlier export would be overwritten", path)
		}
		seen[path] = true
		// exportPath only reserves a name by checking the filesystem, so the
		// caller creating the file is what makes the next call pick a new one.
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatalf("writing %s: %v", path, err)
		}
	}
}

// A result export holds whatever the query returned. The connection file and
// token store are kept at 0600; an export left at 0644 was the odd one out.
func TestExportCSVIsNotWorldReadable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission bits")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)

	r := quack.QueryResult{
		Columns: []string{"id", "secret_value"},
		Rows:    [][]string{{"1", "hunter2"}},
	}
	path, err := exportCSV(r)
	if err != nil {
		t.Fatalf("exportCSV: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("export mode = %#o, want 0600", perm)
	}

	// And it must actually contain the rows.
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got := string(body); !strings.Contains(got, "id,secret_value") || !strings.Contains(got, "1,hunter2") {
		t.Errorf("export body = %q, want the header and row", got)
	}
}

// The export prompt is where the user chooses between serialising the rows on
// screen and re-running the query on the backend. Those differ for a volatile
// table, so the prompt has to say which is which.
func TestExportPromptDistinguishesTheTwoFormats(t *testing.T) {
	sp := NewScratchpad(nil, nil)
	sp.exportPrompt = true

	got := sp.ViewResultsStatus()
	for _, want := range []string{"rows shown", "re-runs query"} {
		if !strings.Contains(got, want) {
			t.Errorf("export prompt is missing %q: %s", want, got)
		}
	}
}
