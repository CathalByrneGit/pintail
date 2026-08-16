package quack

import (
	"crypto/rand"
	"encoding/hex"
	"time"
)

// The bearer-credential model. Rendering a token for a screen or an export file
// is the UI's job; minting one, and what fields it has, is not.

type Token struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	Value       string     `json:"value"`       // full token value; never logged
	Scope       []string   `json:"scope"`       // catalogs this token can access ("*" = global)
	Permissions []string   `json:"permissions"` // SQL operations allowed
	CreatedAt   time.Time  `json:"created_at"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"` // nil = never
	LastUsed    time.Time  `json:"last_used,omitempty"`
	Active      bool       `json:"active"`
}

// TokenManager holds all state for the token management view.
// tmMode toggles the token manager between the two kinds of secrets it
// manages: Quack auth tokens and storage credentials.

func BuildToken(name, scope, perms, expiry string) Token {
	scopeParts := SplitTrim(scope)
	permParts := SplitTrim(perms)
	if len(scopeParts) == 0 {
		scopeParts = []string{"*"}
	}
	if len(permParts) == 0 {
		permParts = []string{"SELECT"}
	}
	if name == "" {
		name = "token_" + time.Now().Format("0102150405")
	}

	var exp *time.Time
	if expiry != "" && expiry != "never" {
		t, err := time.Parse("2006-01-02", expiry)
		if err == nil {
			exp = &t
		}
	}

	return Token{
		ID:          GenerateID(),
		Name:        name,
		Value:       GenerateTokenValue(),
		Scope:       scopeParts,
		Permissions: permParts,
		CreatedAt:   time.Now(),
		ExpiresAt:   exp,
		LastUsed:    time.Time{},
		Active:      true,
	}
}

func GenerateTokenValue() string {
	b := make([]byte, 24)
	rand.Read(b)
	return "qk_" + hex.EncodeToString(b)
}

func GenerateID() string {
	b := make([]byte, 4)
	rand.Read(b)
	return hex.EncodeToString(b)
}
