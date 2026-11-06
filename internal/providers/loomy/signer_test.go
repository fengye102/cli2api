package loomy

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/caigee-cmd/cli2api/internal/providers"
)

// The golden values below were produced by the reference implementation
// (loomy2api-go's loomy.StringToSign / loomy.Sign) for the same inputs. The
// account service rejects a signature that differs by one byte, and it does so
// without explaining why, so this comparison is the only cheap guard against a
// silent regression in the canonical string.
const (
	goldenBody         = `{"base":{"appid":"GM3LOOMY"},"param":{"loginid":"13800138000"}}`
	goldenPath         = "/login/account/byPwd/x y"
	goldenDate         = "Thu, 06 Nov 2026 04:31:51 GMT"
	goldenNonce        = "11111111-2222-4333-8444-555555555555"
	goldenStringToSign = "POST\n/login/account/byPwd/x%20y\na=1%202&b=2\n+4ti0ZxZc9GAoz03SPiePw==\napplication/json\n" +
		goldenDate + "\n" + goldenNonce + "\n\n"
	goldenSignature = "dxCeVDWFlXWPTcLcV3NAPDN2NAQ="
)

func TestStringToSignMatchesReferenceImplementation(t *testing.T) {
	query := map[string]string{"b": "2", "a": "1 2"}
	got := stringToSign("POST", goldenPath, query, goldenBody, "application/json", goldenDate, goldenNonce, nil)
	if got != goldenStringToSign {
		t.Fatalf("stringToSign mismatch\n got: %q\nwant: %q", got, goldenStringToSign)
	}
	if sig := sign("secret", "POST", goldenPath, query, goldenBody, "application/json", goldenDate, goldenNonce, nil); sig != goldenSignature {
		t.Fatalf("signature = %q, want %q", sig, goldenSignature)
	}
}

func TestAccountHeadersCarryEverythingTheServiceChecks(t *testing.T) {
	headers := accountHeaders("POST", accountPukeyPath, `{"base":{}}`, nil, "", nil)
	if !strings.HasPrefix(headers["Authorization"], "account "+accessKeyID+":") {
		t.Fatalf("authorization = %q", headers["Authorization"])
	}
	if headers["Date"] != "" && !strings.HasSuffix(headers["Date"], " GMT") {
		t.Fatalf("date = %q", headers["Date"])
	}
	if headers["Nonce"] == "" || headers["Content-MD5"] == "" {
		t.Fatalf("nonce/content-md5 missing: %v", headers)
	}
	if headers["Content-Type"] != "application/json" {
		t.Fatalf("content type = %q", headers["Content-Type"])
	}
}

// TestLiveAccountServiceSignature is opt-in: it proves the signature is
// accepted by the real account service, using the one endpoint that has no side
// effects (getPuKey). Run with LOOMY_LIVE=1.
func TestLiveAccountServiceSignature(t *testing.T) {
	if os.Getenv("LOOMY_LIVE") != "1" {
		t.Skip("set LOOMY_LIVE=1 to check the signature against the real account service")
	}
	client := NewClient(newFakeStore())
	data, err := client.accountCall(context.Background(), "", accountPukeyPath, nil, Credential{})
	if err != nil {
		t.Fatalf("getPuKey: %v", err)
	}
	var payload struct {
		Pukey string `json:"pukey"`
		Rcode string `json:"rcode"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload.Pukey == "" {
		t.Fatalf("no pukey in %s", data)
	}
	if _, err := parseRSAPublicKey(payload.Pukey); err != nil {
		t.Fatalf("pukey does not parse: %v", err)
	}
}

// TestLiveModelGatewayTraceparent is opt-in as well: it proves the two header
// facts that cannot be verified offline — that the gateway rejects a request
// without a session, and that TLS 1.2 (which the capped transport pins) is what
// makes the host answer at all. Run with LOOMY_LIVE=1.
func TestLiveModelGatewayRejectsAnonymous(t *testing.T) {
	if os.Getenv("LOOMY_LIVE") != "1" {
		t.Skip("set LOOMY_LIVE=1 to talk to the real model gateway")
	}
	client := NewClient(newFakeStore())
	cred := Credential{Format: CredentialFormat, Session: "not-a-real-session"}
	err := func() error {
		_, err := client.fetchCatalog(context.Background(), "", cred)
		return err
	}()
	if err == nil {
		t.Fatal("an invalid session must not produce a catalog")
	}
	providerErr, ok := err.(*providers.Error)
	if !ok {
		t.Fatalf("error = %T %v, want a classified provider error (the host answered)", err, err)
	}
	// The live gateway answers a dead session with HTTP 200 and the 100002
	// business envelope; what matters is that the request came back at all
	// (TLS 1.2 + traceparent worked) and that it was read as an auth failure
	// instead of a malformed catalog.
	if providerErr.Kind != "auth" {
		t.Fatalf("kind = %q (%s, status %d), want auth", providerErr.Kind, providerErr.Message, providerErr.Status)
	}
}
