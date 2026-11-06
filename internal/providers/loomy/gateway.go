package loomy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/caigee-cmd/cli2api/internal/accounts"
	"github.com/caigee-cmd/cli2api/internal/providers"
)

// The model gateway reports failures two ways, and only one of them is an HTTP
// error status:
//
//	HTTP 401 {"error":{"message":"Unauthorized"}}
//	HTTP 200 {"code":"100002","desc":"登录已失效，请重新登录","data":{}}
//
// The second shape is the usual one for a dead session (measured against the
// live gateway), so every model-plane response has to be inspected for a
// business envelope before its payload is trusted. Skipping that check turns an
// expired session into "the catalog is malformed", which is how a shipping
// adapter ends up quietly serving a fallback list for an account that cannot
// chat at all.
//
// Business codes: 000000 ok, 100001 bad request, 100002 auth.

const maxEnvelopePeek = 64 << 10

// envelopeError reads the gateway's business envelope and returns a classified
// error when it reports a failure. A body that is not a JSON object, or one
// without a code, is not an envelope and is left alone.
func envelopeError(raw []byte) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil
	}
	var envelope struct {
		Code    json.RawMessage `json:"code"`
		Desc    string          `json:"desc"`
		Message string          `json:"message"`
	}
	if err := json.Unmarshal(trimmed, &envelope); err != nil {
		return nil
	}
	code := strings.Trim(strings.TrimSpace(string(envelope.Code)), `"`)
	if code == "" || code == onboardingOKCode {
		return nil
	}
	message := firstNonEmpty(envelope.Desc, envelope.Message, code)
	kind := accounts.KindUnavailable
	switch code {
	case onboardingAuthCode:
		kind = accounts.KindAuth
	case onboardingBadReqCode:
		kind = accounts.KindInvalidRequest
	}
	return &providers.Error{Kind: kind, Status: http.StatusOK, Message: message, Code: code}
}

// peekJSONError inspects a 200 response that turned out to be JSON instead of
// an SSE stream, and hands back a body the caller can still read.
func peekJSONError(resp *http.Response) error {
	if resp == nil || resp.Body == nil || resp.StatusCode != http.StatusOK {
		return nil
	}
	body := resp.Body
	reader := bufio.NewReaderSize(body, 512)
	head, _ := reader.Peek(512)
	trimmed := bytes.TrimSpace(head)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		resp.Body = struct {
			io.Reader
			io.Closer
		}{io.MultiReader(reader, body), body}
		return nil
	}
	raw, err := io.ReadAll(io.LimitReader(reader, maxEnvelopePeek))
	body.Close()
	if err != nil {
		return err
	}
	if envelopeErr := envelopeError(raw); envelopeErr != nil {
		return envelopeErr
	}
	resp.Body = io.NopCloser(bytes.NewReader(raw))
	return nil
}

// readGatewayJSON reads a JSON response and fails on either an HTTP error or a
// business envelope error.
func readGatewayJSON(resp *http.Response) ([]byte, error) {
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		return nil, newProviderError(resp.StatusCode, string(raw))
	}
	if err := envelopeError(raw); err != nil {
		return nil, err
	}
	return raw, nil
}
