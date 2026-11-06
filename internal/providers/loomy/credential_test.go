package loomy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/caigee-cmd/cli2api/internal/providers"
)

func TestDecodeCredentialAcceptsEveryOperatorShape(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		check   func(t *testing.T, cred Credential)
	}{
		{
			name:    "loginid and password",
			payload: `{"loginid":"13800138000","password":"s3cret"}`,
			check: func(t *testing.T, cred Credential) {
				if cred.LoginID != "13800138000" || cred.Password != "s3cret" {
					t.Fatalf("cred = %+v", cred)
				}
				if !cred.CanRenew() {
					t.Fatal("loginid+password must be renewable")
				}
			},
		},
		{
			name:    "phone plus country code is normalized",
			payload: `{"phone":"+86 138-0013-8000","password":"x"}`,
			check: func(t *testing.T, cred Credential) {
				if cred.LoginID != "13800138000" {
					t.Fatalf("loginid = %q", cred.LoginID)
				}
			},
		},
		{
			name:    "loomy2api accounts.json entry",
			payload: `{"name":"a1","loginid":"13800138001","password":"pw","session":"sess-1","expire_at":1800000000,"identity":{"devid":"web-abc","campus_device_id":"loomy-campus-fp-1"}}`,
			check: func(t *testing.T, cred Credential) {
				if cred.Session != "sess-1" || cred.ExpiresAt != 1800000000 {
					t.Fatalf("cred = %+v", cred)
				}
				if cred.DeviceID != "web-abc" || cred.CampusDeviceID != "loomy-campus-fp-1" {
					t.Fatalf("identity = %+v", cred)
				}
			},
		},
		{
			name:    "bare token string",
			payload: `"Bearer abc.def"`,
			check: func(t *testing.T, cred Credential) {
				if cred.Session != "abc.def" {
					t.Fatalf("session = %q", cred.Session)
				}
			},
		},
		{
			name:    "raw token without JSON",
			payload: `tok-raw`,
			check: func(t *testing.T, cred Credential) {
				if cred.Session != "tok-raw" {
					t.Fatalf("session = %q", cred.Session)
				}
			},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			cred, err := DecodeCredential([]byte(testCase.payload))
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			testCase.check(t, cred)
		})
	}
}

func TestValidateCredentialRejectsIncomplete(t *testing.T) {
	if err := ValidateCredential([]byte(`{"loginid":"13800138000"}`)); err == nil {
		t.Fatal("a login id without password or session must be rejected")
	}
	if err := ValidateCredential([]byte(`{"session":"ok"}`)); err != nil {
		t.Fatalf("session-only payload must validate: %v", err)
	}
}

func TestSplitImportUnwrapsAccountsJSON(t *testing.T) {
	payload := `{"accounts":[{"loginid":"13800138000","password":"a"},{"session":"s2"}]}`
	var codec credentialCodec
	chunks, err := codec.SplitImport([]byte(payload))
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	if len(chunks) != 2 {
		t.Fatalf("chunks = %d", len(chunks))
	}
	var first map[string]string
	if err := json.Unmarshal(chunks[0], &first); err != nil {
		t.Fatalf("chunk decode: %v", err)
	}
	if first["loginid"] != "13800138000" {
		t.Fatalf("first chunk = %v", first)
	}
}

func TestPrepareImportReportsReady(t *testing.T) {
	var codec credentialCodec
	ready, err := codec.PrepareImport([]byte(`{"loginid":"13800138000","password":"pw"}`))
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if !ready.Ready {
		t.Fatal("a password account is immediately usable")
	}
	if len(ready.Payload) == 0 {
		t.Fatal("payload must be normalized")
	}
}

func TestEncryptPasswordProducesVerifiableCiphertext(t *testing.T) {
	key := newTestRSAKey(t)
	ciphertext, err := encryptPassword(key.publicB64, "hunter2")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	raw, err := base64.StdEncoding.DecodeString(ciphertext)
	if err != nil {
		t.Fatalf("ciphertext is not base64: %v", err)
	}
	if len(raw) != key.size() {
		t.Fatalf("ciphertext length = %d, want %d", len(raw), key.size())
	}
	if plain := key.decrypt(t, raw); plain != "hunter2" {
		t.Fatalf("decrypted = %q", plain)
	}
}

func TestPublicModelPrefixRoundTrip(t *testing.T) {
	if got := publicModel("qwen3.8-flash"); got != "loomy/qwen3.8-flash" {
		t.Fatalf("publicModel = %q", got)
	}
	for _, input := range []string{"loomy/qwen3.8-flash", "qwen3.8-flash", " loomy:qwen3.8-flash "} {
		if got := resolveModel(input); got != "qwen3.8-flash" {
			t.Fatalf("resolveModel(%q) = %q", input, got)
		}
	}
}

func TestAdapterAdvertisesClaimAndLogin(t *testing.T) {
	client := NewClient(newFakeStore())
	adapter := client.Adapter()
	if adapter.ID != "loomy" {
		t.Fatalf("adapter id = %q", adapter.ID)
	}
	for _, capability := range []string{"credential", "chat", "models", "classifier", "prober", "claim", "login", "import_export"} {
		if !adapter.Supports(capability) {
			t.Fatalf("adapter does not support %s", capability)
		}
	}
	if _, ok := adapter.Login.(providers.LoginCompleter); !ok {
		t.Fatal("login must also complete a round from a pasted value")
	}
	_ = context.Background()
}

func TestClassifyReadsQuotaWording(t *testing.T) {
	cls := Classify(400, `{"error":{"message":"积分不足，请充值"}}`)
	if cls.Kind != "quota" {
		t.Fatalf("kind = %q, want quota", cls.Kind)
	}
	if cls := Classify(401, ""); cls.Kind != "auth" {
		t.Fatalf("401 kind = %q", cls.Kind)
	}
	if cls := Classify(429, ""); cls.Kind != "rate_limit" {
		t.Fatalf("429 kind = %q", cls.Kind)
	}
	if cls := Classify(404, ""); cls.Kind != "model_not_available" {
		t.Fatalf("404 kind = %q", cls.Kind)
	}
	if cls := Classify(503, "bad gateway"); cls.Kind != "unavailable" {
		t.Fatalf("503 kind = %q", cls.Kind)
	}
}

func TestNormalizePhoneStripsSeparators(t *testing.T) {
	if got := normalizePhone("+86 138 0013 8000"); got != "13800138000" {
		t.Fatalf("normalizePhone = %q", got)
	}
	if got := digitsOnly("code: 123456"); got != "123456" {
		t.Fatalf("digitsOnly = %q", got)
	}
	phone, password, ok := splitCredentialPaste("13800138000 my pass")
	if !ok || phone != "13800138000" || password != "my pass" {
		t.Fatalf("splitCredentialPaste = %q %q %v", phone, password, ok)
	}
	if strings.Contains(maskPhone("13800138000"), "0013") {
		t.Fatalf("maskPhone leaked digits: %q", maskPhone("13800138000"))
	}
}
