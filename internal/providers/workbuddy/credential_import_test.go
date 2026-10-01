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

// A standard-base64 token can contain '+'; without a uid in front of it the
// delimiter rule used to hand back only the tail of the token as the access
// token (and a JWT fragment as the uid).
func TestDecodeCredentialTokenWithPlusIsNotTruncated(t *testing.T) {
	const token = "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ1In0.ab+cd/ef=="
	payload := `{"session": {"token": "` + token + `"}}`
	credential, err := DecodeCredential([]byte(payload))
	if err != nil {
		t.Fatalf("DecodeCredential: %v", err)
	}
	if credential.AccessToken != token {
		t.Fatalf("token truncated to %q", credential.AccessToken)
	}
	if credential.UID != "" {
		t.Fatalf("uid must stay empty when the token carries no uid prefix, got %q", credential.UID)
	}
	if credential.Ready() {
		t.Fatalf("a credential without uid must not be Ready")
	}
}

// The packed form still splits when the token itself contains '+', and the
// uid fallback stays available when it does not.
func TestDecodeCredentialPackedTokenWithPlusInToken(t *testing.T) {
	payload := `{"session": {"token": "cb-77+head.eyJhbGc+abc/def=="}, "account": {"uid": "cb-77"}}`
	credential, err := DecodeCredential([]byte(payload))
	if err != nil {
		t.Fatalf("DecodeCredential: %v", err)
	}
	if credential.AccessToken != "head.eyJhbGc+abc/def==" || credential.UID != "cb-77" {
		t.Fatalf("unexpected split: %#v", credential)
	}
}

func TestSplitPackedTokenCases(t *testing.T) {
	cases := []struct {
		in    string
		uid   string
		value string
	}{
		{"cb-42+packed-token", "cb-42", "packed-token"},
		{"cb-42+head.eyJhbGc+abc/def==", "cb-42", "head.eyJhbGc+abc/def=="},
		{"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ1In0.ab+cd/ef==", "", "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ1In0.ab+cd/ef=="},
		{"plain-token", "", "plain-token"},
		{"uid+", "", "uid+"},
		{"", "", ""},
		{"  cb-1+tok  ", "cb-1", "tok"},
	}
	for _, tc := range cases {
		uid, value := splitPackedToken(tc.in)
		if uid != tc.uid || value != tc.value {
			t.Errorf("splitPackedToken(%q) = (%q, %q), want (%q, %q)", tc.in, uid, value, tc.uid, tc.value)
		}
	}
}

// The {account, auth} export shape used to be accepted on identity alone, so
// a payload that carried only "account" matched it and returned a token-less
// credential, hiding the token that lived under another key.
func TestDecodeCredentialAccountIdentityDoesNotShadowToken(t *testing.T) {
	payload := `{"account": {"uid": "cb-9"}, "auth": {"refreshToken": "rt-9"}, "session": {"token": "cb-9+tok-9"}}`
	credential, err := DecodeCredential([]byte(payload))
	if err != nil {
		t.Fatalf("DecodeCredential: %v", err)
	}
	if credential.AccessToken != "tok-9" || credential.UID != "cb-9" {
		t.Fatalf("unexpected decode: %#v", credential)
	}
	if !credential.Ready() {
		t.Fatalf("credential should be Ready: %#v", credential)
	}
}

func TestDecodeCredentialIdentityOnlyIsRejected(t *testing.T) {
	_, err := DecodeCredential([]byte(`{"account": {"uid": "cb-9"}}`))
	if err == nil {
		t.Fatal("an identity-only payload must be rejected instead of yielding a token-less credential")
	}
}
