package codex

import (
	"encoding/base64"
	"encoding/json"
	"testing"
)

// fakeJWT builds an unsigned test JWT carrying the given claims.
func fakeJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	return "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

func TestDecodeCredentialOfficialAuthJSON(t *testing.T) {
	idToken := fakeJWT(t, map[string]any{
		"email": "user@example.com",
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "acct-123",
		},
	})
	payload := `{"auth_mode":"chatgpt","OPENAI_API_KEY":null,"tokens":{"id_token":"` + idToken +
		`","access_token":"at","refresh_token":"rt"},"last_refresh":"2026-01-01T00:00:00Z"}`
	credential, err := DecodeCredential([]byte(payload))
	if err != nil {
		t.Fatalf("DecodeCredential: %v", err)
	}
	if credential.AccessToken != "at" || credential.RefreshToken != "rt" {
		t.Fatalf("unexpected tokens: %#v", credential)
	}
	if credential.AccountID != "acct-123" {
		t.Fatalf("want account_id from id_token claims, got %q", credential.AccountID)
	}
	if credential.Email != "user@example.com" {
		t.Fatalf("want email from id_token claims, got %q", credential.Email)
	}
	if !credential.Ready() {
		t.Fatalf("expected ready credential")
	}
}

func TestDecodeCredentialCockpitBackupShape(t *testing.T) {
	payload := `{"id":"x","email":"c@example.com","token":{"access_token":"at2","refresh_token":"rt2","expiry_timestamp":1780000000,"token_type":"Bearer"}}`
	credential, err := DecodeCredential([]byte(payload))
	if err != nil {
		t.Fatalf("DecodeCredential: %v", err)
	}
	if credential.AccessToken != "at2" || credential.RefreshToken != "rt2" {
		t.Fatalf("unexpected tokens: %#v", credential)
	}
	if credential.Email != "c@example.com" {
		t.Fatalf("want wrapped email, got %q", credential.Email)
	}
	// No JWT anywhere: AccountID stays empty, so the account imports but is
	// not ready until a refresh materializes an id_token.
	if credential.Ready() {
		t.Fatalf("expected not-ready without account_id")
	}
}

func TestDecodeCredentialAPIKeyAuthJSONRejected(t *testing.T) {
	payload := `{"auth_mode":"apikey","OPENAI_API_KEY":"sk-proj-abc"}`
	if _, err := DecodeCredential([]byte(payload)); err == nil {
		t.Fatalf("expected api-key-only auth.json to be rejected")
	}
}
