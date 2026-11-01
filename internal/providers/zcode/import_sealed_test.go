package zcode

// Imports from a ZCode installation's own files: which provider section carries
// the key (3.15 moved it into config.json, magpie#2130), and how a sealed
// credentials.json opens (ZCode encrypts every value under a key made from the
// machine, so the operator has to hand over the seed). 3.16 keeps no key at
// all, only its location (magpie#410) — that case has its own message.

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

// testJWT is a ZCode session token: three segments, the middle one a JSON
// payload with the identity the importer reads.
const testJWT = "eyJhbGciOiJIUzI1NiJ9.eyJ1c2VyX2lkIjoiNDI0MiIsImVtYWlsIjoidUBleGFtcGxlLmNvbSJ9.c2ln"

// sealForTest writes one value the way ZCode's client does.
func sealForTest(t *testing.T, plaintext, seed string) string {
	t.Helper()
	key := sha256.Sum256([]byte(seed))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatalf("gcm: %v", err)
	}
	iv := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(iv); err != nil {
		t.Fatalf("nonce: %v", err)
	}
	sealed := gcm.Seal(nil, iv, []byte(plaintext), nil)
	data, tag := sealed[:len(sealed)-gcm.Overhead()], sealed[len(sealed)-gcm.Overhead():]
	enc := base64.RawURLEncoding.EncodeToString
	return sealedValuePrefix + enc(iv) + "." + enc(tag) + "." + enc(data)
}

// TestImportPicksTheZCodeProviderSection is the 3.15 shape: the coding plan key
// sits in config.json's provider table, beside whatever other agent the user
// pointed at a gateway. Picking the first apiKey found imported a foreign key,
// and Go's map order made the choice random between imports.
func TestImportPicksTheZCodeProviderSection(t *testing.T) {
	config := map[string]any{
		"provider": map[string]any{
			"magpie": map[string]any{
				"options": map[string]any{
					"apiKey":  "magpie-gateway-key",
					"baseURL": "http://127.0.0.1:8080/v1",
				},
			},
			"zai-coding-plan": map[string]any{
				"options": map[string]any{
					"apiKey":  "keyid.secret",
					"baseURL": "https://api.z.ai/api/anthropic",
				},
			},
			"aaa-other": map[string]any{
				"options": map[string]any{
					"apiKey":  "someone-elses",
					"baseURL": "https://example.test/v1",
				},
			},
		},
	}
	payload, err := json.Marshal(config)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for i := 0; i < 20; i++ { // map order must not decide the import
		credential, err := DecodeCredential(payload)
		if err != nil {
			t.Fatalf("DecodeCredential: %v", err)
		}
		if credential.APIKey != "keyid.secret" {
			t.Fatalf("imported key %q, want the ZCode provider section's", credential.APIKey)
		}
		if credential.AuthMode != AuthModeAPIKey {
			t.Errorf("AuthMode=%q", credential.AuthMode)
		}
	}
}

// TestImportOpensASealedCredentialFile is the 3.14/3.15 credentials.json: the
// Start Plan session is sealed, and once opened the file imports as an OAuth
// account with its identity.
func TestImportOpensASealedCredentialFile(t *testing.T) {
	const seed = "zcode-credential-fallback:win32:C:\\Users\\27199:27199"
	t.Setenv("ZCODE_CREDENTIAL_SEED", seed)

	store := map[string]any{
		"zcodejwttoken":          sealForTest(t, testJWT, seed),
		"oauth:zai:access_token": sealForTest(t, testJWT, seed),
		"oauth:zai:user_info":    sealForTest(t, `{"email":"u@example.com","id":"4242"}`, seed),
		"account-provider:coding-plan:account:zai-individual-coding-plan:account:4242:api-key": sealForTest(t, "keyid.secret", seed),
	}
	payload, err := json.Marshal(store)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	credential, err := DecodeCredential(payload)
	if err != nil {
		t.Fatalf("DecodeCredential: %v", err)
	}
	if credential.AuthMode != AuthModeOAuth {
		t.Errorf("AuthMode=%q want %q", credential.AuthMode, AuthModeOAuth)
	}
	if credential.ZCodeJWT != testJWT {
		t.Errorf("zcodejwttoken=%q, want the opened session", credential.ZCodeJWT)
	}
	if credential.Email != "u@example.com" {
		t.Errorf("email=%q", credential.Email)
	}
}

// TestImportOpensWithTheSecretForm: the same file opens when the operator hands
// over the secret itself rather than the fallback seed string.
func TestImportOpensWithTheSecretForm(t *testing.T) {
	const secret = "a-per-machine-secret"
	t.Setenv("ZCODE_CREDENTIAL_SECRET", secret)
	payload, err := json.Marshal(map[string]any{
		"zcodejwttoken": sealForTest(t, testJWT, secret),
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	credential, err := DecodeCredential(payload)
	if err != nil {
		t.Fatalf("DecodeCredential: %v", err)
	}
	if credential.ZCodeJWT != testJWT {
		t.Errorf("zcodejwttoken=%q", credential.ZCodeJWT)
	}
}

// TestImportBareSealedTokenOpens: pasting just the sealed value works too.
func TestImportBareSealedTokenOpens(t *testing.T) {
	const secret = "another-secret"
	t.Setenv("ZCODE_CREDENTIAL_SECRET", secret)
	credential, err := DecodeCredential([]byte(sealForTest(t, testJWT, secret)))
	if err != nil {
		t.Fatalf("DecodeCredential: %v", err)
	}
	if credential.ZCodeJWT != testJWT || credential.AuthMode != AuthModeOAuth {
		t.Errorf("credential=%+v", credential)
	}
}

// TestSealedImportWithoutSeedSaysWhatToDo: the failure has to name the knob,
// because nothing else tells an operator why a file that works locally does not
// import here.
func TestSealedImportWithoutSeedSaysWhatToDo(t *testing.T) {
	t.Setenv("ZCODE_CREDENTIAL_SECRET", "")
	t.Setenv("ZCODE_CREDENTIAL_SEED", "")
	payload, err := json.Marshal(map[string]any{
		"zcodejwttoken": sealForTest(t, testJWT, "the-machines-seed"),
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	_, err = DecodeCredential(payload)
	if err == nil {
		t.Fatal("a sealed file must not import without the seed")
	}
	for _, want := range []string{"ZCODE_CREDENTIAL_SECRET", "ZCODE_CREDENTIAL_SEED", "zcodejwttoken"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %s", err, want)
		}
	}

	// A wrong seed must fail the same way rather than produce garbage.
	t.Setenv("ZCODE_CREDENTIAL_SECRET", "not-the-machines-secret")
	if _, err := DecodeCredential(payload); err == nil {
		t.Fatal("the wrong seed must not open the file")
	}
}

// TestImport3_16LocationOnlyExplainsItself: 3.16 names where the key is and
// keeps none, so an operator pasting that file needs to be told what it is.
func TestImport3_16LocationOnlyExplainsItself(t *testing.T) {
	location := projectKeyLocationPrefix + `%5B%22https%3A%2F%2Fz.ai%22%2C%22zai%22%2C%224242%22%2Cnull%2C1%2C%22sel%22%5D`
	payload, err := json.Marshal(map[string]any{
		location: `{"organizationId":"o1","projectId":"p1","apiKeyId":"k1"}`,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	_, err = DecodeCredential(payload)
	if err == nil {
		t.Fatal("a location-only file carries no key")
	}
	for _, want := range []string{"3.16", "project-key-location", "zcodejwttoken"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %s", err, want)
		}
	}
}

// TestImport3_16FileWithSessionStillWorks: the same file does import once its
// session is readable — the JWT is what this gateway runs on.
func TestImport3_16FileWithSessionStillWorks(t *testing.T) {
	const seed = "seed-for-316"
	t.Setenv("ZCODE_CREDENTIAL_SECRET", seed)
	location := projectKeyLocationPrefix + `%5B%22https%3A%2F%2Fz.ai%22%2C%22zai%22%2C%224242%22%2Cnull%2C1%2C%22sel%22%5D`
	payload, err := json.Marshal(map[string]any{
		"zcodejwttoken": sealForTest(t, testJWT, seed),
		location:        sealForTest(t, `{"organizationId":"o1","projectId":"p1","apiKeyId":"k1"}`, seed),
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	credential, err := DecodeCredential(payload)
	if err != nil {
		t.Fatalf("DecodeCredential: %v", err)
	}
	if credential.ZCodeJWT != testJWT {
		t.Errorf("zcodejwttoken=%q", credential.ZCodeJWT)
	}
}

// TestUnsealLeavesTheInputAlone: an import must not half-modify its input.
func TestUnsealLeavesTheInputAlone(t *testing.T) {
	const seed = "leave-it-alone"
	t.Setenv("ZCODE_CREDENTIAL_SECRET", seed)
	sealed := sealForTest(t, "plain", seed)
	doc := map[string]any{"a": sealed, "nested": map[string]any{"b": sealed}}
	out, left := unsealDocument(doc)
	if left != 0 {
		t.Errorf("sealed values left=%d", left)
	}
	if doc["a"] != sealed || doc["nested"].(map[string]any)["b"] != sealed {
		t.Error("unsealDocument modified its input")
	}
	opened := out.(map[string]any)
	if opened["a"] != "plain" || opened["nested"].(map[string]any)["b"] != "plain" {
		t.Errorf("unsealed=%v", opened)
	}
}

// TestResolveClientVersion pins the reported build and its override.
func TestResolveClientVersion(t *testing.T) {
	if got := resolveClientVersion(""); got != defaultClientVersion {
		t.Errorf("default=%q want %q", got, defaultClientVersion)
	}
	if defaultClientVersion != "3.16.3" {
		t.Errorf("tracked client version=%q: bump it with the official release", defaultClientVersion)
	}
	if got := resolveClientVersion(" 3.14.3 "); got != "3.14.3" {
		t.Errorf("override=%q", got)
	}
	if Version == "" || userAgent() != "ZCode/"+Version {
		t.Errorf("Version=%q userAgent=%q", Version, userAgent())
	}
}

// TestOpenSealedValueRejectsGarbage keeps the opener from panicking on the
// shapes a hand-edited file can hold.
func TestOpenSealedValueRejectsGarbage(t *testing.T) {
	for _, value := range []string{
		"enc:v1:",
		"enc:v1:aaaa",
		"enc:v1:..",
		"enc:v1:!!!.@@@.###",
		"plain-value",
	} {
		if _, ok := openSealedValue(value, "seed"); ok {
			t.Errorf("%q opened", value)
		}
	}
}
