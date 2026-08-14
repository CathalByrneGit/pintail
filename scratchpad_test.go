package main

import (
	"strings"
	"testing"
)

// Deleting connections in the connection manager replaced the scratchpad's
// server list without clamping serverIdx, so the next render of the screen
// indexed past the end of the shorter slice and took the app down.
func TestSetTargetsClampsSelection(t *testing.T) {
	three := []ServerInfo{{Name: "a"}, {Name: "b"}, {Name: "c"}}

	tests := []struct {
		name        string
		startIdx    int
		newServers  []ServerInfo
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
		{"tiny width has no room for an ellipsis", "abcdefgh", 2, "ab"},
		{"zero width yields nothing", "abcdefgh", 0, ""},
		{"negative width yields nothing", "abcdefgh", -7, ""},
		{"multibyte cut stays on a rune boundary", "日本語テスト", 5, "日本語テ…"},
		{"multibyte that fits is untouched", "日本語", 5, "日本語"},
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
		if got := cutRunes(tc.in, tc.n); got != tc.want {
			t.Errorf("cutRunes(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
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
