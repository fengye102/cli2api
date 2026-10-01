package trae

import (
	"testing"
)

func TestDecodeCredentialStorageJSON(t *testing.T) {
	// Trae IDE storage.json: the auth blob sits behind an
	// "iCubeAuthInfo://<provider>" key and is serialized as a JSON string.
	payload := `{
		"iCubeAuthInfo://icube.cloudide": "{\"accessToken\":\"at\",\"refreshToken\":\"rt\",\"userId\":\"u-1\",\"nickname\":\"nick\",\"expiresAt\":1780000000000}",
		"other": {"unrelated": true}
	}`
	credential, err := DecodeCredential([]byte(payload))
	if err != nil {
		t.Fatalf("DecodeCredential: %v", err)
	}
	if credential.AccessToken != "at" || credential.RefreshToken != "rt" {
		t.Fatalf("unexpected tokens: %#v", credential)
	}
	if credential.UID != "u-1" || credential.Nickname != "nick" {
		t.Fatalf("unexpected identity: %#v", credential)
	}
	if credential.ExpiresAt != 1780000000 {
		t.Fatalf("want ms timestamp clamped to seconds, got %d", credential.ExpiresAt)
	}
}

func TestDecodeCredentialStorageJSONObjectValue(t *testing.T) {
	// Same storage.json shape but the blob is a real object, not a string.
	payload := `{
		"iCubeAuthInfo://icube.cloudide": {
			"data": {"access_token": "dat2", "refresh_token": "drt2"},
			"account": {"uid": "u-9"},
			"nickname": "n2"
		}
	}`
	credential, err := DecodeCredential([]byte(payload))
	if err != nil {
		t.Fatalf("DecodeCredential: %v", err)
	}
	if credential.AccessToken != "dat2" || credential.RefreshToken != "drt2" {
		t.Fatalf("unexpected tokens: %#v", credential)
	}
	if credential.UID != "u-9" {
		t.Fatalf("unexpected uid: %#v", credential)
	}
}

func TestDecodeCredentialStorageJSONWithoutTokensRejected(t *testing.T) {
	payload := `{"iCubeAuthInfo://icube.cloudide": {"nickname": "n"}}`
	if _, err := DecodeCredential([]byte(payload)); err == nil {
		t.Fatalf("expected storage.json without tokens to be rejected")
	}
}
