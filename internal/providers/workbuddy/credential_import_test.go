package workbuddy

import (
	"testing"
)

func TestDecodeCredentialVscdbAuthValue(t *testing.T) {
	// CodeBuddy desktop state.vscdb auth value: the blob is a JSON-encoded
	// string under an outer key, identity nested below account.profile.
	payload := `{
		"auth": "{\"accessToken\": \"tok-1\", \"refreshToken\": \"rt-1\", \"expiresAt\": \"1780000000000\"}",
		"account": {"profile": {"uid": "cb-1", "nickname": "cb-user"}},
		"domain": "codebuddy.cn"
	}`
	credential, err := DecodeCredential([]byte(payload))
	if err != nil {
		t.Fatalf("DecodeCredential: %v", err)
	}
	if credential.AccessToken != "tok-1" || credential.RefreshToken != "rt-1" {
		t.Fatalf("unexpected tokens: %#v", credential)
	}
	if credential.UID != "cb-1" || credential.Nickname != "cb-user" {
		t.Fatalf("unexpected identity: %#v", credential)
	}
	if credential.ExpiresAt != 1780000000 {
		t.Fatalf("want ms timestamp clamped to seconds, got %d", credential.ExpiresAt)
	}
}

func TestDecodeCredentialPackedToken(t *testing.T) {
	// CodeBuddy local sessions pack the user id into the token: "uid+token".
	payload := `{"session": {"token": "cb-42+packed-token"}}`
	credential, err := DecodeCredential([]byte(payload))
	if err != nil {
		t.Fatalf("DecodeCredential: %v", err)
	}
	if credential.AccessToken != "packed-token" || credential.UID != "cb-42" {
		t.Fatalf("unexpected split: %#v", credential)
	}
}
