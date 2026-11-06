package minimax

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/caigee-cmd/cli2api/internal/accounts"
	"github.com/caigee-cmd/cli2api/internal/providers"
	"github.com/caigee-cmd/cli2api/internal/providers/anthropicwire"
	"github.com/caigee-cmd/cli2api/internal/translate"
)

// applyAuthHeaders sets the credential's headers the way the official client
// does. The gateway wants all four: a Bearer token, a placeholder x-api-key, the
// agent id and the Messages protocol version. A request missing the agent id is
// treated as an unknown client.
func applyAuthHeaders(h http.Header, cred Credential) {
	if token := strings.TrimSpace(cred.AccessToken); token != "" {
		scheme := strings.TrimSpace(cred.TokenType)
		if scheme == "" {
			scheme = "Bearer"
		}
		h.Set("Authorization", scheme+" "+token)
	}
	h.Set("x-api-key", PlaceholderAPIKey)
	h.Set("anthropic-version", AnthropicVersion)
	h.Set("User-Agent", UserAgent)
	h.Set("x-mavis-agent-id", AgentID)
}

// buildBody serialises the internal chat request as an Anthropic Messages body.
// The shared builder owns the OpenAI→Anthropic conversion (system extraction,
// tool_use / tool_result blocks, image blocks); this adapter only fills in the
// fields the agent gateway needs on top.
func (c *Client) buildBody(ctx context.Context, accountID string, req translate.ChatRequest) map[string]any {
	caps := c.modelCapabilities(ctx, accountID, req.Model)
	body := anthropicwire.Body(req, caps.MaxOutput)
	body["model"] = req.Model
	// Always stream: a non-stream caller collects the same stream locally, and
	// one wire path means usage accounting lives in one place.
	body["stream"] = true
	if _, ok := body["max_tokens"]; !ok {
		limit := caps.MaxOutput
		if limit <= 0 {
			limit = defaultMaxTokens
		}
		body["max_tokens"] = limit
	}
	return body
}

// postMessage sends one Messages request with a usable access token. It returns
// the raw response so the caller can decide whether a 401 is worth a refresh.
func (c *Client) postMessage(ctx context.Context, accountID string, cred Credential, body map[string]any) (*http.Response, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	site := c.site(cred.Region)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, site.AgentHost+MessagesPath, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	applyAuthHeaders(req.Header, cred)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	client, err := c.httpClient(ctx, accountID)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, &providers.Error{
			Kind:    "unavailable",
			Message: "minimax 上游不可达：" + err.Error(),
			// Keep the chain so a dial/TLS failure is recognised by shape and
			// does not park an otherwise healthy account.
			Err: err,
		}
	}
	return resp, nil
}

// doChat is the shared chat path: one refresh-and-retry on 401, classification
// for every refusing status, and a refusal that arrives as a 2xx JSON envelope.
func (c *Client) doChat(ctx context.Context, accountID string, req translate.ChatRequest) (*http.Response, error) {
	cred, err := c.freshCredential(ctx, accountID)
	if err != nil {
		return nil, err
	}
	body := c.buildBody(ctx, accountID, req)
	for attempt := 0; attempt < 2; attempt++ {
		resp, err := c.postMessage(ctx, accountID, cred, body)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			if attempt == 0 {
				// The pair may simply be old. A refresh is cheap; a false
				// "logged out" is not.
				renewed, refreshErr := c.refresh(ctx, accountID, cred)
				if refreshErr == nil {
					cred = renewed
					continue
				}
			}
			return nil, newProviderError(resp.StatusCode, string(raw))
		}
		if resp.StatusCode >= 300 || resp.StatusCode < 200 {
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			return nil, newProviderError(resp.StatusCode, string(raw))
		}
		if !strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
			// A 2xx that is not a stream is an error envelope: the gateway
			// reports quota and risk refusals this way, with HTTP 200.
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			if jsonEnvelopeIsError(resp.Header.Get("Content-Type"), raw) {
				return nil, newProviderError(resp.StatusCode, string(raw))
			}
			resp.Body = io.NopCloser(bytes.NewReader(raw))
		}
		return resp, nil
	}
	return nil, &providers.Error{Kind: accounts.KindAuth, Status: http.StatusUnauthorized,
		Message: "minimax 鉴权失败，刷新后仍被拒绝，请重新登录"}
}

// ChatStream relays the upstream Messages stream as OpenAI chat.completion
// chunks: the shared rewriter owns the dialect change.
func (c *Client) ChatStream(ctx context.Context, accountID string, req translate.ChatRequest) (*http.Response, providers.ResolvedChat, error) {
	resp, err := c.doChat(ctx, accountID, req)
	if err != nil {
		return nil, providers.ResolvedChat{}, err
	}
	return anthropicwire.RewriteStream(resp, "minimax"), providers.ResolvedChat{}, nil
}

// ChatNonStream asks for a stream and assembles it, exactly as the streaming
// path does — one wire keeps tool-call assembly in one place.
func (c *Client) ChatNonStream(ctx context.Context, accountID string, req translate.ChatRequest) (providers.ChatOutcome, error) {
	resp, err := c.doChat(ctx, accountID, req)
	if err != nil {
		return providers.ChatOutcome{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return providers.ChatOutcome{}, err
	}
	aggregate, err := anthropicwire.Aggregate(bytes.NewReader(body), "minimax")
	if err != nil {
		return providers.ChatOutcome{}, err
	}
	outcome, err := anthropicwire.OutcomeFromAggregate(aggregate)
	if err != nil {
		return providers.ChatOutcome{}, err
	}
	if outcome.Model == "" {
		outcome.Model = req.Model
	}
	return outcome, nil
}
