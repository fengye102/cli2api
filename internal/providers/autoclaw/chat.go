package autoclaw

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/caigee-cmd/cli2api/internal/accounts"
	"github.com/caigee-cmd/cli2api/internal/providers"
	"github.com/caigee-cmd/cli2api/internal/translate"
)

// ChatStream posts the chat request and returns the upstream SSE body
// unchanged. On a 401 it refreshes once and retries.
//
// The body is reshaped before send: model split into route id + body id,
// max_tokens normalized up, and the system prompt prefixed/rewritten. Those
// three edits are what stand between an ordinary client request and an empty
// 406/403.
func (c *Client) ChatStream(ctx context.Context, accountID string, req translate.ChatRequest) (*http.Response, providers.ResolvedChat, error) {
	resp, resolved, err := c.doChat(ctx, accountID, req)
	if err != nil {
		return nil, providers.ResolvedChat{}, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		resp.Body.Close()
		cred, credErr := c.credential(ctx, accountID)
		if credErr != nil {
			return nil, providers.ResolvedChat{}, credErr
		}
		// force: a rejected token may still look fresh, so only the upstream
		// can say it is bad.
		if _, refreshErr := c.forceRefresh(ctx, accountID, cred); refreshErr == nil {
			return c.doChat(ctx, accountID, req)
		}
		return nil, providers.ResolvedChat{}, newProviderError(resp.StatusCode, "autoclaw token rejected and refresh failed")
	}
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		return nil, providers.ResolvedChat{}, newProviderError(resp.StatusCode, string(body))
	}
	return resp, resolved, nil
}

// ChatNonStream forces a stream and collects it, so one code path serves both
// response shapes.
func (c *Client) ChatNonStream(ctx context.Context, accountID string, req translate.ChatRequest) (providers.ChatOutcome, error) {
	resp, resolved, err := c.ChatStream(ctx, accountID, req)
	if err != nil {
		return providers.ChatOutcome{}, err
	}
	defer resp.Body.Close()
	outcome, err := collectChatSSE(resp.Body, req.Model)
	if err != nil {
		return providers.ChatOutcome{}, err
	}
	outcome.ReasoningLevel = resolved.ReasoningLevel
	return outcome, nil
}

// doChat builds and sends one chat request.
func (c *Client) doChat(ctx context.Context, accountID string, req translate.ChatRequest) (*http.Response, providers.ResolvedChat, error) {
	cred, err := c.credential(ctx, accountID)
	if err != nil {
		return nil, providers.ResolvedChat{}, err
	}
	if cred.AccessToken == "" {
		return nil, providers.ResolvedChat{}, &providers.Error{
			Kind:    accounts.KindAuth,
			Status:  http.StatusUnauthorized,
			Message: "autoclaw credential has no access token; add the account again",
		}
	}
	route := resolveModelRoute(c.region, stripProviderPrefix(req.Model))
	payload, err := buildChatBody(req, route)
	if err != nil {
		return nil, providers.ResolvedChat{}, err
	}
	endpoint := strings.TrimRight(c.upstreamBase, "/") + c.region.chatPath()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, providers.ResolvedChat{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	// The source sends Accept: */* (the proxy may answer either shape; whether
	// the reply streams is decided by body.stream, not this header).
	httpReq.Header.Set("Accept", "*/*")
	for name, value := range brandHeaders() {
		httpReq.Header.Set(name, value)
	}
	// Auth header name is X-Authorization, NOT Authorization.
	httpReq.Header.Set("X-Authorization", "Bearer "+cred.AccessToken)
	httpReq.Header.Set("X-Request-Model", route.RouteModelID)
	client, err := c.httpClient(ctx, accountID)
	if err != nil {
		return nil, providers.ResolvedChat{}, err
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, providers.ResolvedChat{}, c.transportError(err)
	}
	return resp, providers.ResolvedChat{}, nil
}

// buildChatBody serializes the request and applies the three upstream gates.
//
// Order matters: the prompt normalization runs last so it is the final edit
// before the bytes leave.
func buildChatBody(req translate.ChatRequest, route modelRoute) ([]byte, error) {
	raw, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var body map[string]any
	if err := decoder.Decode(&body); err != nil {
		return nil, err
	}
	// body.model carries the prefix-stripped id; the full route id travels in
	// the X-Request-Model header.
	body["model"] = route.BodyModelID
	body["stream"] = true
	normalizeMaxTokens(body)
	normalizePrompt(body)
	return json.Marshal(body)
}

// stripProviderPrefix removes a "autoclaw/" or "autoclaw-intl/" prefix the
// client may have sent when addressing the provider explicitly.
func stripProviderPrefix(model string) string {
	clean := strings.TrimSpace(model)
	for _, prefix := range []string{IntlProviderID + "/", ProviderID + "/"} {
		if rest, ok := strings.CutPrefix(clean, prefix); ok {
			return strings.TrimSpace(rest)
		}
	}
	return clean
}

// forceRefresh refreshes regardless of the expiry window (401 recovery).
func (c *Client) forceRefresh(ctx context.Context, accountID string, cred Credential) (Credential, error) {
	if !cred.CanRefresh() {
		return Credential{}, &providers.Error{
			Kind:    accounts.KindAuth,
			Status:  http.StatusUnauthorized,
			Message: "autoclaw credential has no refresh token; add the account again",
		}
	}
	lock := c.accountLock(accountID)
	lock.Lock()
	defer lock.Unlock()
	next, err := c.callRefresh(ctx, accountID, cred)
	if err != nil {
		return Credential{}, err
	}
	if err := c.saveCredential(ctx, accountID, next); err != nil {
		return Credential{}, err
	}
	c.markRefreshed(accountID)
	return next, nil
}
