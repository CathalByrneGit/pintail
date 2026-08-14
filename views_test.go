package main

import (
	"fmt"
	"testing"
)

// Every panel width in the app is derived from the terminal size, so any of
// these can be handed a small or negative width on a narrow terminal. They
// used to panic there — the token list did so at a plain 80 columns, which is
// the width these render calls receive from an 80-column terminal.
//
// The assertion is simply that rendering completes: a panic fails the test.
func TestViewsSurviveNarrowWidths(t *testing.T) {
	widths := []int{-20, -1, 0, 1, 4, 10, 20, 26, 40, 200}

	tok := buildToken("etl_pipeline_prod", "analytics, raw", "SELECT, INSERT", "never")
	revoked := buildToken("old_token", "analytics", "SELECT", "never")
	revoked.Active = false

	secret := StorageSecret{
		Name: "lake_s3", Type: SecretS3,
		KeyID: "AKIAEXAMPLE", Secret: "supersecret",
		Region: "us-east-1", Scope: "s3://datalake-prod/lake",
	}

	lakeCfg := ServerConfig{
		Name: "lake-prod", Type: ConnDuckLake,
		CatalogPath: "/tmp/lake/catalog.duckdb", StoragePath: "/tmp/lake/data",
	}

	renders := map[string]func(w int){
		"ViewTokenList": func(w int) {
			tm := TokenManager{tokens: []Token{tok, revoked}}
			_ = tm.ViewTokenList(w, 20)
		},
		"ViewTokenList/empty": func(w int) {
			tm := TokenManager{}
			_ = tm.ViewTokenList(w, 20)
		},
		"ViewTokenDetail": func(w int) {
			tm := TokenManager{tokens: []Token{tok}}
			_ = tm.ViewTokenDetail(w)
		},
		"ViewSecretList": func(w int) {
			tm := TokenManager{mode: tmModeSecrets, secrets: []StorageSecret{secret}}
			_ = tm.ViewSecretList(w, 20)
		},
		"ViewSecretDetail": func(w int) {
			tm := TokenManager{mode: tmModeSecrets, secrets: []StorageSecret{secret}}
			_ = tm.ViewSecretDetail(w)
		},
		"ViewSecretForm": func(w int) {
			tm := TokenManager{mode: tmModeSecrets, secretForm: newSecretForm()}
			_ = tm.ViewSecretForm(w, 20)
		},
		"ViewPolicyList": func(w int) {
			_ = NewAuthEditor([]Token{tok, revoked}, nil).ViewPolicyList(w, 20)
		},
		"ViewPermGrid": func(w int) {
			_ = NewAuthEditor([]Token{tok}, nil).ViewPermGrid(w)
		},
		"ViewPermGrid/noPolicies": func(w int) {
			_ = NewAuthEditor(nil, nil).ViewPermGrid(w)
		},
		"TLSGenerator.ViewForm": func(w int) {
			_ = NewTLSGenerator([]ServerConfig{{Name: "q", Host: "localhost", Port: 9494}}).ViewForm(w)
		},
		"SnapshotsView.ViewList": func(w int) {
			_ = newTestSnapshotsView(lakeCfg).ViewList(w)
		},
		"SnapshotsView.ViewDetail": func(w int) {
			_ = newTestSnapshotsView(lakeCfg).ViewDetail(w)
		},
	}

	for name, render := range renders {
		for _, w := range widths {
			t.Run(fmt.Sprintf("%s/width=%d", name, w), func(t *testing.T) {
				render(w)
			})
		}
	}
}

// The exact regression: an 80-column terminal gives the token list a width of
// (80*30/100)-4 = 20, and any token whose scope is longer than two characters
// then sliced below zero.
func TestViewTokenListAtEightyColumns(t *testing.T) {
	tm := TokenManager{tokens: []Token{
		buildToken("etl_pipeline_prod", "analytics, raw, staging", "SELECT", "never"),
	}}
	const eightyColumnLeftPanel = (80 * 30 / 100) - 4
	if got := tm.ViewTokenList(eightyColumnLeftPanel, 20); got == "" {
		t.Fatal("ViewTokenList rendered nothing")
	}
}

func newTestSnapshotsView(cfg ServerConfig) SnapshotsView {
	v := NewSnapshotsView([]*QuackClient{NewQuackClient(cfg, nil, nil)})
	v.snapshots = []Snapshot{{
		ID: "3", Time: "2026-08-14T10:00:00Z", SchemaVersion: "2",
		Raw: map[string]interface{}{
			"snapshot_id": 3, "snapshot_time": "2026-08-14T10:00:00Z", "schema_version": 2,
		},
	}}
	return v
}
