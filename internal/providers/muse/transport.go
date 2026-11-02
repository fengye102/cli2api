package muse

import (
	"context"
	"fmt"
	"net/http"

	"github.com/caigee-cmd/cli2api/internal/accounts"
	"github.com/caigee-cmd/cli2api/internal/providers"
	"github.com/caigee-cmd/cli2api/internal/translate"
)

// chatInput is one conversation turn handed to a transport.
type chatInput struct {
	AccountID  string
	Credential Credential
	Request    translate.ChatRequest
}

// ChatTransport is the only seam that touches the muse.ai conversation
// channel. muse.ai drives its agent over a browser session, so the channel is
// either a plain HTTP call (preferred: one implementation, no Chromium in the
// gateway image) or, if the channel turns out to be a private signed RPC, a
// CDP-driven browser. Both live behind this interface so the account pool,
// quota cards and console never learn which one is in use.
//
// Implementations return OpenAI chat-completions deltas, so the API layer
// relays the stream without translation.
type ChatTransport interface {
	// Name identifies the transport in logs ("http" / "cdp").
	Name() string
	// Available reports whether the transport can serve a turn at all. An
	// unavailable transport makes chat fail fast with a clear message instead
	// of hanging on a request it cannot build.
	Available() bool
	Chat(ctx context.Context, in chatInput) (providers.ChatOutcome, error)
	ChatStream(ctx context.Context, in chatInput) (*http.Response, providers.ResolvedChat, error)
}

// HTTPTransport talks to muse.ai over HTTP with the imported cookies. The
// request shape (thread creation, message post, stream framing) is filled in
// from one captured browser session; until then Available() is false so the
// gateway reports "not calibrated" rather than inventing a 500.
type HTTPTransport struct {
	endpoint string
}

// NewHTTPTransport builds the pure-HTTP transport.
func NewHTTPTransport() *HTTPTransport { return &HTTPTransport{} }

func (t *HTTPTransport) Name() string { return "http" }

// Available is false until the captured endpoints are wired.
func (t *HTTPTransport) Available() bool { return t.endpoint != "" }

func (t *HTTPTransport) Chat(ctx context.Context, in chatInput) (providers.ChatOutcome, error) {
	_ = ctx
	_ = in
	return providers.ChatOutcome{}, errTransportPending()
}

func (t *HTTPTransport) ChatStream(ctx context.Context, in chatInput) (*http.Response, providers.ResolvedChat, error) {
	_ = ctx
	_ = in
	return nil, providers.ResolvedChat{}, errTransportPending()
}

// errTransportPending is deliberately a 502 classified as upstream-unavailable:
// the account is healthy (its cookies work), only the conversation channel is
// not wired, so the pool must not cool the account down for it.
func errTransportPending() error {
	return &providers.Error{
		Kind:   accounts.KindUnavailable,
		Status: http.StatusBadGateway,
		Message: fmt.Sprintf("muse: 会话通道尚未校准（cookie 导入与探活可用）。"+
			"需要一次登录态请求抓包：%s", SessionAPI),
	}
}
