package zcode

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

// jwtForTest builds an unsigned 3-segment JWT with the given payload fields.
func jwtForTest(t *testing.T, payload map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	seg := base64.RawURLEncoding.EncodeToString(raw)
	return "eyJhbGciOiJIUzI1NiJ9." + seg + ".sig"
}

func TestDecodeCredential_OwnBundleAPIKey(t *testing.T) {
	payload := []byte(`{"format":"zcode-credential-v1","auth_mode":"api_key","provider":"zai","api_key":"abc.def"}`)
	cred, err := DecodeCredential(payload)
	if err != nil {
		t.Fatalf("DecodeCredential: %v", err)
	}
	if cred.AuthMode != AuthModeAPIKey {
		t.Errorf("AuthMode=%q want %q", cred.AuthMode, AuthModeAPIKey)
	}
	if cred.APIKey != "abc.def" {
		t.Errorf("APIKey=%q", cred.APIKey)
	}
	if cred.Provider != RegionZAI {
		t.Errorf("Provider=%q want %q", cred.Provider, RegionZAI)
	}
	if cred.Format != CredentialFormat {
		t.Errorf("Format=%q want %q", cred.Format, CredentialFormat)
	}
}

func TestDecodeCredential_OwnBundleOAuth(t *testing.T) {
	jwt := jwtForTest(t, map[string]any{"user_id": "u-1", "email": "a@b.c", "provider": "zai"})
	payload, _ := json.Marshal(map[string]any{
		"format":          "zcode-credential-v1",
		"auth_mode":       "oauth",
		"provider":        "zai",
		"zcode_jwt_token": jwt,
		"access_token":    "at",
		"refresh_token":   "rt",
		"expires_at":      12345,
		"email":           "a@b.c",
		"user_id":         "u-1",
	})
	cred, err := DecodeCredential(payload)
	if err != nil {
		t.Fatalf("DecodeCredential: %v", err)
	}
	if !cred.IsOAuth() {
		t.Errorf("IsOAuth=false")
	}
	if cred.ZCodeJWT != jwt {
		t.Errorf("ZCodeJWT mismatch")
	}
	if cred.UserID != "u-1" {
		t.Errorf("UserID=%q", cred.UserID)
	}
	if !cred.Ready() {
		t.Errorf("Ready=false")
	}
}

func TestDecodeCredential_BareAPIKey(t *testing.T) {
	cred, err := DecodeCredential([]byte("my-api-key.my-secret"))
	if err != nil {
		t.Fatalf("DecodeCredential: %v", err)
	}
	if cred.AuthMode != AuthModeAPIKey {
		t.Errorf("AuthMode=%q want %q", cred.AuthMode, AuthModeAPIKey)
	}
	if cred.APIKey != "my-api-key.my-secret" {
		t.Errorf("APIKey=%q", cred.APIKey)
	}
}

func TestDecodeCredential_BareJWT(t *testing.T) {
	jwt := jwtForTest(t, map[string]any{"user_id": "u-2", "email": "x@y.z"})
	cred, err := DecodeCredential([]byte(jwt))
	if err != nil {
		t.Fatalf("DecodeCredential: %v", err)
	}
	if !cred.IsOAuth() {
		t.Errorf("IsOAuth=false")
	}
	if cred.ZCodeJWT != jwt {
		t.Errorf("ZCodeJWT mismatch")
	}
	if cred.UserID != "u-2" {
		t.Errorf("UserID=%q want u-2", cred.UserID)
	}
	if cred.Email != "x@y.z" {
		t.Errorf("Email=%q want x@y.z", cred.Email)
	}
}

func TestDecodeCredential_ConfigJSONZAI(t *testing.T) {
	doc := `{
		"provider": {
			"builtin:zai-coding-plan": {
				"options": {
					"apiKey": "abc.def",
					"baseURL": "https://api.z.ai/api/anthropic"
				}
			}
		}
	}`
	cred, err := DecodeCredential([]byte(doc))
	if err != nil {
		t.Fatalf("DecodeCredential: %v", err)
	}
	if cred.AuthMode != AuthModeAPIKey {
		t.Errorf("AuthMode=%q want api_key", cred.AuthMode)
	}
	if cred.APIKey != "abc.def" {
		t.Errorf("APIKey=%q", cred.APIKey)
	}
	if cred.Provider != RegionZAI {
		t.Errorf("Provider=%q want zai", cred.Provider)
	}
	if cred.BaseURL != "https://api.z.ai/api/anthropic" {
		t.Errorf("BaseURL=%q", cred.BaseURL)
	}
}

func TestDecodeCredential_ConfigJSONZaiBaseURL(t *testing.T) {
	doc := `{
		"provider": {
			"custom:zai": {
				"options": {
					"apiKey": "plain-zai-key",
					"baseURL": "https://api.z.ai/api/anthropic"
				}
			}
		}
	}`
	cred, err := DecodeCredential([]byte(doc))
	if err != nil {
		t.Fatalf("DecodeCredential: %v", err)
	}
	if cred.Provider != RegionZAI {
		t.Errorf("Provider=%q want zai", cred.Provider)
	}
}

func TestDecodeCredential_ConfigJSONJWTFallsToOAuth(t *testing.T) {
	jwt := jwtForTest(t, map[string]any{"user_id": "u-3"})
	doc := `{
		"provider": {
			"builtin:zai-coding-plan": {
				"options": {
					"apiKey": "` + jwt + `",
					"baseURL": "https://api.z.ai/api/anthropic"
				}
			}
		}
	}`
	cred, err := DecodeCredential([]byte(doc))
	if err != nil {
		t.Fatalf("DecodeCredential: %v", err)
	}
	if !cred.IsOAuth() {
		t.Errorf("IsOAuth=false")
	}
	if cred.ZCodeJWT != jwt {
		t.Errorf("ZCodeJWT mismatch")
	}
	if cred.APIKey != "" {
		t.Errorf("APIKey should be empty in oauth mode, got %q", cred.APIKey)
	}
}

func TestDecodeCredential_CredentialsJSONPlaintext(t *testing.T) {
	jwt := jwtForTest(t, map[string]any{"user_id": "u-4"})
	doc := `{
		"zcodejwttoken": "` + jwt + `",
		"oauth:active_provider": "zai",
		"oauth:zai:access_token": "at-plain",
		"oauth:zai:refresh_token": "rt-plain"
	}`
	cred, err := DecodeCredential([]byte(doc))
	if err != nil {
		t.Fatalf("DecodeCredential: %v", err)
	}
	if !cred.IsOAuth() {
		t.Errorf("IsOAuth=false")
	}
	if cred.ZCodeJWT != jwt {
		t.Errorf("ZCodeJWT mismatch")
	}
	if cred.Provider != RegionZAI {
		t.Errorf("Provider=%q want zai", cred.Provider)
	}
}

func TestDecodeCredential_SealedPayloadRejected(t *testing.T) {
	payload := []byte("enc:v1:AAAAAAAAAAAAAAAA")
	_, err := DecodeCredential(payload)
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "enc:v1:") {
		t.Errorf("error should mention enc:v1:, got %q", msg)
	}
	if !strings.Contains(msg, "plaintext apiKey") {
		t.Errorf("error should be actionable, got %q", msg)
	}
}

func TestDecodeCredential_SealedInsideDocument(t *testing.T) {
	doc := `{
		"zcodejwttoken": "enc:v1:sealed-jwt",
		"oauth:zai:access_token": "enc:v1:sealed-at"
	}`
	_, err := DecodeCredential([]byte(doc))
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "zcodejwttoken") && !strings.Contains(err.Error(), "access_token") {
		t.Errorf("error should name the sealed key, got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "plaintext apiKey") {
		t.Errorf("error should be actionable, got %q", err.Error())
	}
}

func TestDecodeCredential_EmptyPayloadRejected(t *testing.T) {
	for _, p := range [][]byte{nil, []byte(""), []byte("   "), []byte("{}")} {
		if _, err := DecodeCredential(p); err == nil {
			t.Errorf("expected error for %q, got nil", p)
		}
	}
}

func TestValidateCredential(t *testing.T) {
	if err := ValidateCredential([]byte("plain-key")); err != nil {
		t.Errorf("ValidateCredential plain key: %v", err)
	}
	if err := ValidateCredential([]byte("enc:v1:x")); err == nil {
		t.Errorf("ValidateCredential sealed should fail")
	}
}

func TestEncodeRoundtrip(t *testing.T) {
	original := Credential{
		AuthMode: AuthModeAPIKey,
		Provider: RegionZAI,
		APIKey:   "abc.def",
	}
	raw, err := original.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	decoded, err := DecodeCredential(raw)
	if err != nil {
		t.Fatalf("DecodeCredential(encoded): %v", err)
	}
	if decoded.Format != CredentialFormat {
		t.Errorf("Format=%q", decoded.Format)
	}
	if decoded.APIKey != original.APIKey {
		t.Errorf("APIKey=%q", decoded.APIKey)
	}
	if decoded.AuthMode != AuthModeAPIKey {
		t.Errorf("AuthMode=%q", decoded.AuthMode)
	}
}

func TestPrepareImport(t *testing.T) {
	codec := credentialCodec{}
	got, err := codec.PrepareImport([]byte("plain-key"))
	if err != nil {
		t.Fatalf("PrepareImport: %v", err)
	}
	if !got.Ready {
		t.Errorf("Ready=false for usable key")
	}
	if len(got.Payload) == 0 {
		t.Errorf("Payload empty")
	}
	// A sealed payload fails PrepareImport with the actionable message.
	if _, err := codec.PrepareImport([]byte("enc:v1:x")); err == nil {
		t.Errorf("PrepareImport sealed should fail")
	}
}

func TestIsJWTToken(t *testing.T) {
	if !IsJWTToken("a.b.c") {
		t.Errorf("a.b.c should be JWT")
	}
	if IsJWTToken("a.b") {
		t.Errorf("a.b should not be JWT")
	}
	if IsJWTToken("plain") {
		t.Errorf("plain should not be JWT")
	}
}

// TestDecodeCredential_ArrayExport covers account-manager exports that wrap the
// account in a JSON array (cockpit-tools and similar).
func TestDecodeCredential_ArrayExport(t *testing.T) {
	jwt := jwtForTest(t, map[string]any{"user_id": "u-1", "provider": "zai"})
	single := `[{"auth_mode":"oauth","provider":"zai","email":"a@b.c","user_id":"u1",` +
		`"zcode_jwt_token":"` + jwt + `","plan_type":"ZCode Trust Build","quota_remaining":100000000}]`
	credential, err := DecodeCredential([]byte(single))
	if err != nil {
		t.Fatalf("single-element array export rejected: %v", err)
	}
	if credential.ZCodeJWT != jwt || !credential.IsOAuth() || credential.Provider != RegionZAI {
		t.Errorf("decoded = %+v", credential)
	}
	if !credential.hasUsableCredential() {
		t.Errorf("decoded credential is not usable")
	}

	// Several elements: the first element with usable material wins.
	multi := `[{"auth_mode":"oauth","provider":"zai"},` +
		`{"auth_mode":"api_key","provider":"zai","api_key":"zai-key"}]`
	credential, err = DecodeCredential([]byte(multi))
	if err != nil {
		t.Fatalf("multi-element array export rejected: %v", err)
	}
	if credential.APIKey != "zai-key" || credential.Provider != RegionZAI {
		t.Errorf("multi-element pick = %+v", credential)
	}

	if _, err := DecodeCredential([]byte(`[]`)); err == nil {
		t.Errorf("empty array should fail")
	}
}
