package providers

import (
	"context"
	"strings"
)

// sessionHintKey carries the conversation a request belongs to. The executor
// keys its routing affinity on it; providers that talk to an upstream keeping a
// per-conversation prompt cache need the same value on the wire, and they
// cannot import the executor. The hint is the executor's opaque session key
// (a hex digest, not secret material): providers only render it into whatever
// shape their upstream expects.
type sessionHintKey struct{}

// WithSessionHint records the conversation on the request context.
func WithSessionHint(ctx context.Context, hint string) context.Context {
	trimmed := strings.TrimSpace(hint)
	if trimmed == "" || ctx == nil {
		return ctx
	}
	return context.WithValue(ctx, sessionHintKey{}, trimmed)
}

// SessionHintFromContext returns the conversation the request belongs to, or
// "" when the caller did not identify one.
func SessionHintFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	hint, _ := ctx.Value(sessionHintKey{}).(string)
	return strings.TrimSpace(hint)
}
