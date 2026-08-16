package ui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/CathalByrneGit/pintail/internal/quack"
	"time"
)

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
	if got := quack.LoadTokens(); len(got) != 1 || got[0].Value != created {
		t.Fatalf("creation was not persisted: %+v", got)
	}

	// Rotate: the new value must be on disk, since it is the only copy.
	tm.rotateConfirm = true
	tm, _ = tm.updateRotateConfirm(key("y"))
	rotated := tm.tokens[0].Value
	if rotated == created {
		t.Fatal("rotate did not change the value")
	}
	if got := quack.LoadTokens(); len(got) != 1 || got[0].Value != rotated {
		t.Errorf("rotation was not persisted: %+v", got)
	}

	// Revoke.
	tm.revokeConfirm = true
	tm, _ = tm.updateRevokeConfirm(key("y"))
	if got := quack.LoadTokens(); len(got) != 1 || got[0].Active {
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

	tm := TokenManager{tokens: []quack.Token{quack.BuildToken("t", "*", "SELECT", "never")}}
	tm.persist("saved")
	if !strings.HasPrefix(tm.successMsg, "save failed") {
		t.Errorf("successMsg = %q, want a save failure", tm.successMsg)
	}
}

// A stored expiry has to render as the date the form accepts, or a round-trip
// through the token screen silently changes it.
func TestFmtExpiryRendersTheFormsDateFormat(t *testing.T) {
	when := time.Date(2030, 1, 15, 9, 30, 0, 0, time.UTC)
	if got := fmtExpiry(&when); got != "2030-01-15" {
		t.Errorf("fmtExpiry = %q, want 2030-01-15", got)
	}
	if got := fmtExpiry(nil); got != "never" {
		t.Errorf("fmtExpiry(nil) = %q, want never", got)
	}
}
