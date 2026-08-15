package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Each Save* helper writes one section and must leave the others alone. They
// used to re-read the file per call, so the sections could only survive by
// accident — and tokens were not persisted at all.
func TestConfigSectionsRoundTripIndependently(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	servers := []ServerConfig{
		{Name: "central", Type: ConnQuack, Host: "catalog.internal", Port: 9494, Token: "qk_abc"},
		{Name: "lake", Type: ConnDuckLake, CatalogRef: "central", StoragePath: "s3://bucket/lake"},
	}
	secrets := []StorageSecret{{Name: "lake_s3", Type: SecretS3, KeyID: "AKIA", Secret: "shh", Region: "us-east-1"}}
	tokens := []Token{buildToken("etl", "analytics", "SELECT", "never")}

	if err := SaveServerConfigs(servers); err != nil {
		t.Fatalf("SaveServerConfigs: %v", err)
	}
	if err := SaveStorageSecrets(secrets); err != nil {
		t.Fatalf("SaveStorageSecrets: %v", err)
	}
	if err := SaveTokens(tokens); err != nil {
		t.Fatalf("SaveTokens: %v", err)
	}

	// Writing the last section must not have dropped the first two.
	gotServers := LoadServerConfigs()
	if len(gotServers) != 2 || gotServers[0].Name != "central" || gotServers[1].CatalogRef != "central" {
		t.Errorf("servers did not survive: %+v", gotServers)
	}
	gotSecrets := LoadStorageSecrets()
	if len(gotSecrets) != 1 || gotSecrets[0].Secret != "shh" {
		t.Errorf("secrets did not survive: %+v", gotSecrets)
	}
	gotTokens := LoadTokens()
	if len(gotTokens) != 1 {
		t.Fatalf("tokens did not survive: %+v", gotTokens)
	}
	if gotTokens[0].Value != tokens[0].Value {
		t.Errorf("token value = %q, want %q", gotTokens[0].Value, tokens[0].Value)
	}
	if !gotTokens[0].Active || gotTokens[0].Scope[0] != "analytics" {
		t.Errorf("token round-tripped wrong: %+v", gotTokens[0])
	}

	// Re-saving one section repeatedly must stay stable.
	for i := 0; i < 3; i++ {
		if err := SaveStorageSecrets(gotSecrets); err != nil {
			t.Fatalf("re-save: %v", err)
		}
	}
	if len(LoadTokens()) != 1 || len(LoadServerConfigs()) != 2 {
		t.Error("a repeated secrets save disturbed the other sections")
	}
}

// The file holds tokens and cloud credentials in plaintext, so it must not be
// group- or world-readable.
func TestConfigFilePermissions(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	if err := SaveTokens([]Token{buildToken("t", "*", "SELECT", "never")}); err != nil {
		t.Fatal(err)
	}

	fi, err := os.Stat(ConfigFilePath())
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0600 {
		t.Errorf("config file mode = %04o, want 0600", perm)
	}
	di, err := os.Stat(filepath.Dir(ConfigFilePath()))
	if err != nil {
		t.Fatal(err)
	}
	if perm := di.Mode().Perm(); perm&0077 != 0 {
		t.Errorf("config dir mode = %04o, want no group/other access", perm)
	}

	// No temp files left behind by the atomic write.
	entries, err := os.ReadDir(filepath.Dir(ConfigFilePath()))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".pintail-") {
			t.Errorf("temp file %q was left behind", e.Name())
		}
	}
}

func TestTokenManagerPersistsMutations(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	tm := NewTokenManager()
	if len(tm.tokens) != 0 {
		t.Fatalf("expected no tokens in a fresh config, got %d", len(tm.tokens))
	}

	// Create through the form, exactly as the UI does.
	tm.form = newTokenForm()
	tm.form.fields[0].SetValue("etl_pipeline")
	tm.form.focusIdx = len(tm.form.fields) - 1
	tm, _ = tm.updateForm(key("enter"))

	if len(tm.tokens) != 1 {
		t.Fatalf("token was not added: %+v", tm.tokens)
	}
	created := tm.tokens[0].Value
	if got := LoadTokens(); len(got) != 1 || got[0].Value != created {
		t.Fatalf("creation was not persisted: %+v", got)
	}

	// Rotate: the new value must be on disk, since it is the only copy.
	tm.rotateConfirm = true
	tm, _ = tm.updateRotateConfirm(key("y"))
	rotated := tm.tokens[0].Value
	if rotated == created {
		t.Fatal("rotate did not change the value")
	}
	if got := LoadTokens(); len(got) != 1 || got[0].Value != rotated {
		t.Errorf("rotation was not persisted: %+v", got)
	}

	// Revoke.
	tm.revokeConfirm = true
	tm, _ = tm.updateRevokeConfirm(key("y"))
	if got := LoadTokens(); len(got) != 1 || got[0].Active {
		t.Errorf("revocation was not persisted: %+v", got)
	}

	// A fresh manager sees all of it.
	if reloaded := NewTokenManager(); len(reloaded.tokens) != 1 || reloaded.tokens[0].Active {
		t.Errorf("reloaded manager state = %+v", reloaded.tokens)
	}
}

func TestPersistReportsFailure(t *testing.T) {
	home := t.TempDir()
	blocked := filepath.Join(home, "not-a-dir")
	if err := os.WriteFile(blocked, nil, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", blocked)

	tm := TokenManager{tokens: []Token{buildToken("t", "*", "SELECT", "never")}}
	tm.persist("saved")
	if !strings.HasPrefix(tm.successMsg, "save failed") {
		t.Errorf("successMsg = %q, want a save failure", tm.successMsg)
	}
}

func TestLegacyConfigStillLoads(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// A pre-existing file with no type field and no tokens section.
	legacy := `{"servers":[{"name":"old","host":"h","port":9494}]}`
	dir := filepath.Join(home, ".duckdb")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pintail.json"), []byte(legacy), 0600); err != nil {
		t.Fatal(err)
	}

	got := LoadServerConfigs()
	if len(got) != 1 || got[0].Type != ConnQuack {
		t.Errorf("legacy config = %+v, want one quack connection", got)
	}
	if tokens := LoadTokens(); len(tokens) != 0 {
		t.Errorf("expected no tokens, got %+v", tokens)
	}

	// Saving tokens onto a legacy file keeps the servers.
	if err := SaveTokens([]Token{buildToken("new", "*", "SELECT", "never")}); err != nil {
		t.Fatal(err)
	}
	if got := LoadServerConfigs(); len(got) != 1 || got[0].Name != "old" {
		t.Errorf("servers lost when adding tokens: %+v", got)
	}

	var raw map[string]json.RawMessage
	data, err := os.ReadFile(ConfigFilePath())
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	for _, section := range []string{"servers", "tokens"} {
		if _, ok := raw[section]; !ok {
			t.Errorf("section %q missing from the written file:\n%s", section, data)
		}
	}
}

func TestTokenExpiryRoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	tok := buildToken("expiring", "*", "SELECT", "2030-01-15")
	if tok.ExpiresAt == nil {
		t.Fatal("expiry was not parsed")
	}
	if err := SaveTokens([]Token{tok}); err != nil {
		t.Fatal(err)
	}

	got := LoadTokens()
	if len(got) != 1 || got[0].ExpiresAt == nil {
		t.Fatalf("expiry did not round-trip: %+v", got)
	}
	if !got[0].ExpiresAt.Equal(*tok.ExpiresAt) {
		t.Errorf("ExpiresAt = %v, want %v", got[0].ExpiresAt, tok.ExpiresAt)
	}
	if fmtExpiry(got[0].ExpiresAt) != "2030-01-15" {
		t.Errorf("fmtExpiry = %q", fmtExpiry(got[0].ExpiresAt))
	}

	// A token with no expiry keeps nil rather than a zero time.
	never := buildToken("forever", "*", "SELECT", "never")
	if err := SaveTokens([]Token{never}); err != nil {
		t.Fatal(err)
	}
	if got := LoadTokens(); got[0].ExpiresAt != nil {
		t.Errorf("ExpiresAt = %v, want nil", got[0].ExpiresAt)
	}
}
