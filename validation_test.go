package main

import (
	"strings"
	"testing"
)

// Names and refs are resolved by lookup, so a duplicate name makes the second
// connection unreachable and a dangling ref fails at query time. Both used to
// be accepted by the form without comment.
func TestFormValidation(t *testing.T) {
	existing := []ServerConfig{
		{Name: "central", Type: ConnQuack, Host: "a", Port: 9494},
		{Name: "local-dev", Type: ConnLocal, Path: "/tmp/x.duckdb"},
	}
	secrets := []StorageSecret{{Name: "lake_s3", Type: SecretS3, KeyID: "k", Secret: "s"}}

	tests := []struct {
		name    string
		form    func() *addServerForm
		wantHas string // substring of the expected complaint; "" means saveable
	}{
		{
			name: "a valid new quack connection",
			form: func() *addServerForm {
				f := newAddServerForm()
				f.name, f.host = "edge", "edge.internal"
				return f
			},
		},
		{
			name: "missing name",
			form: func() *addServerForm {
				f := newAddServerForm()
				f.host = "edge.internal"
				return f
			},
			wantHas: "name is required",
		},
		{
			name: "missing host on a quack connection",
			form: func() *addServerForm {
				f := newAddServerForm()
				f.name = "edge"
				return f
			},
			wantHas: "host is required",
		},
		{
			name: "duplicate name",
			form: func() *addServerForm {
				f := newAddServerForm()
				f.name, f.host = "central", "elsewhere"
				return f
			},
			wantHas: `named "central" already exists`,
		},
		{
			name: "duplicate name differing only in case",
			form: func() *addServerForm {
				f := newAddServerForm()
				f.name, f.host = "CENTRAL", "elsewhere"
				return f
			},
			wantHas: "already exists",
		},
		{
			name: "editing a connection keeps its own name",
			form: func() *addServerForm {
				f := formFromConfig(existing[0], 0)
				f.host = "moved.internal"
				return f
			},
		},
		{
			name: "local connection without a path",
			form: func() *addServerForm {
				f := newAddServerForm()
				f.connType, f.name = ConnLocal, "files"
				return f
			},
			wantHas: "path is required",
		},
		{
			name: "ducklake with neither catalog ref nor path",
			form: func() *addServerForm {
				f := newAddServerForm()
				f.connType, f.name, f.storagePath = ConnDuckLake, "lake", "/tmp/data"
				return f
			},
			wantHas: "catalog ref or a catalog path",
		},
		{
			name: "ducklake without storage",
			form: func() *addServerForm {
				f := newAddServerForm()
				f.connType, f.name, f.catalogRef = ConnDuckLake, "lake", "central"
				return f
			},
			wantHas: "storage path is required",
		},
		{
			name: "ducklake with a valid catalog ref",
			form: func() *addServerForm {
				f := newAddServerForm()
				f.connType, f.name = ConnDuckLake, "lake"
				f.catalogRef, f.storagePath = "central", "/tmp/data"
				return f
			},
		},
		{
			name: "ducklake pointing at a connection that does not exist",
			form: func() *addServerForm {
				f := newAddServerForm()
				f.connType, f.name = ConnDuckLake, "lake"
				f.catalogRef, f.storagePath = "typo-central", "/tmp/data"
				return f
			},
			wantHas: `no connection named "typo-central"`,
		},
		{
			name: "ducklake naming itself as its catalog",
			form: func() *addServerForm {
				f := newAddServerForm()
				f.connType, f.name = ConnDuckLake, "lake"
				f.catalogRef, f.storagePath = "lake", "/tmp/data"
				return f
			},
			wantHas: "cannot be its own catalog",
		},
		{
			name: "storage secret that does not exist",
			form: func() *addServerForm {
				f := newAddServerForm()
				f.connType, f.name, f.path = ConnLocal, "files", "/tmp/a.duckdb"
				f.secretRef = "nope"
				return f
			},
			wantHas: `no storage secret named "nope"`,
		},
		{
			name: "storage secret that exists",
			form: func() *addServerForm {
				f := newAddServerForm()
				f.connType, f.name, f.path = ConnLocal, "files", "/tmp/a.duckdb"
				f.secretRef = "lake_s3"
				return f
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.form().problem(existing, secrets)
			if tc.wantHas == "" {
				if got != "" {
					t.Errorf("form should be saveable, got %q", got)
				}
				return
			}
			if !strings.Contains(got, tc.wantHas) {
				t.Errorf("problem() = %q, want it to mention %q", got, tc.wantHas)
			}
		})
	}
}

// Enter on an unsaveable form used to do nothing at all, which reads as a
// broken key. It now explains itself and leaves the config untouched.
func TestSaveRefusedShowsWhy(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	configs := []ServerConfig{{Name: "central", Type: ConnQuack, Host: "a", Port: 9494}}
	m := Model{
		configs:     configs,
		clients:     InitClients(configs, nil),
		data:        make([]connData, len(configs)),
		wasOnline:   make([]bool, len(configs)),
		currentView: viewAddServer,
		width:       120,
		height:      40,
	}
	m.connTable = buildConnectionTable(nil)
	m.scratchpad = NewScratchpad(nil, nil)

	// Try to save a second connection with a name that is already taken.
	m.addForm = newAddServerForm()
	m.addForm.name = "central"
	m.addForm.host = "elsewhere"
	m.addForm.focusIdx = len(m.addForm.visibleFields()) - 1

	next, _ := m.updateAddServer(key("enter"))
	m = next.(Model)

	if len(m.configs) != 1 {
		t.Errorf("configs = %d, want the duplicate rejected", len(m.configs))
	}
	if m.addForm == nil {
		t.Fatal("the form should stay open after a refused save")
	}
	if !strings.Contains(m.addForm.errMsg, "already exists") {
		t.Errorf("errMsg = %q, want an explanation", m.addForm.errMsg)
	}
	if view := m.viewAddServerScreen(); !strings.Contains(view, "already exists") {
		t.Errorf("the screen does not show the reason:\n%s", view)
	}

	// Fixing the name lets it through, and clears the message.
	m.addForm.name = "edge"
	next, _ = m.updateAddServer(key("enter"))
	m = next.(Model)

	if len(m.configs) != 2 || m.configs[1].Name != "edge" {
		t.Fatalf("configs after fixing the name = %+v", m.configs)
	}
	if m.addForm != nil {
		t.Error("a successful save should close the form")
	}
	if m.currentView != viewDashboard {
		t.Errorf("view = %v, want the dashboard after saving", m.currentView)
	}
}
