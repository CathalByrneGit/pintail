package quack

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
	tokens := []Token{BuildToken("etl", "analytics", "SELECT", "never")}

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

	if err := SaveTokens([]Token{BuildToken("t", "*", "SELECT", "never")}); err != nil {
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
	if err := SaveTokens([]Token{BuildToken("new", "*", "SELECT", "never")}); err != nil {
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

	tok := BuildToken("expiring", "*", "SELECT", "2030-01-15")
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
	// How that timestamp is rendered for a screen is asserted in the UI.

	// A token with no expiry keeps nil rather than a zero time.
	never := BuildToken("forever", "*", "SELECT", "never")
	if err := SaveTokens([]Token{never}); err != nil {
		t.Fatal(err)
	}
	if got := LoadTokens(); got[0].ExpiresAt != nil {
		t.Errorf("ExpiresAt = %v, want nil", got[0].ExpiresAt)
	}
}
