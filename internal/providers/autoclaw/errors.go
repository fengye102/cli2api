package autoclaw

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/caigee-cmd/cli2api/internal/accounts"
	"github.com/caigee-cmd/cli2api/internal/providers"
)

// newProviderError maps an upstream status + body to the internal taxonomy.
//
// AutoClaw has no structured quota code: the source proxy greps zero hits for
// 429 anywhere, and non-2xx handling is just "read message, throw". So the
// classification is by HTTP status only, same tier as raccoon.
//
// 406 gets an explanatory suffix because it arrives with an EMPTY body: without
// it the user cannot tell whether the gateway or their client is at fault, and
// the three known causes are each unguessable on their own.
func newProviderError(status int, body string) error {
	message := upstreamMessage(body)
	switch status {
	case http.StatusUnauthorized:
		return &providers.Error{
			Kind:    accounts.KindAuth,
			Status:  http.StatusUnauthorized,
			Message: "autoclaw token rejected: " + message,
		}
	case http.StatusTooManyRequests:
		return &providers.Error{
			Kind:    accounts.KindQuota,
			Status:  http.StatusTooManyRequests,
			Message: "autoclaw quota limited: " + message,
		}
	case http.StatusForbidden:
		return &providers.Error{
			Kind:    accounts.KindUnavailable,
			Status:  http.StatusForbidden,
			Message: "autoclaw forbidden: " + message + forbiddenHint,
		}
	case http.StatusNotAcceptable:
		return &providers.Error{
			Kind:    accounts.KindUnavailable,
			Status:  http.StatusNotAcceptable,
			Message: "autoclaw rejected the request shape (406): " + notAcceptableHint,
		}
	default:
		return &providers.Error{
			Kind:    accounts.KindUnavailable,
			Status:  status,
			Message: fmt.Sprintf("autoclaw upstream returned %d: %s", status, message),
		}
	}
}

// forbiddenHint explains the 403 pay-view case, which is a header/prompt gate
// rather than a permissions problem.
const forbiddenHint = "；上游拒收（403）：实测成因是请求头 X-Harness-Type 或 system 首句指纹，网关已按官方客户端指纹出站，若仍复现请把请求体发到项目 issue"

// notAcceptableHint enumerates the measured causes of an empty 406.
//
// Three independent directions, all verified by report authors; listing only
// one sends people down the wrong path for the other two.
const notAcceptableHint = "；上游未给出原因，实测方向：" +
	"① max_tokens 取小值（如 1024/2048/4096）被预校验拦截 —— 网关已归一到 10000；" +
	"② system 首句命中逐字指纹拦截 —— 网关已加 OpenClaw 身份前缀并改写外来身份句；" +
	"③ 请求形态被风控拦截（①②排除时）"

// upstreamMessage extracts a readable message from an error body, falling back
// to the raw text when the body is not JSON.
func upstreamMessage(body string) string {
	trimmed := strings.TrimSpace(body)
	if trimmed == "" {
		return "(empty body)"
	}
	var envelope struct {
		Msg     string `json:"msg"`
		Message string `json:"message"`
		Code    any    `json:"code"`
	}
	if err := json.Unmarshal([]byte(trimmed), &envelope); err == nil {
		if text := firstNonEmpty(envelope.Message, envelope.Msg); text != "" {
			return truncateText(text, 200)
		}
	}
	return truncateText(trimmed, 200)
}

// classifier implements providers.ErrorClassifier for this provider.
type classifier struct{}

// Classify maps a status + body without wrapping it in a providers.Error, which
// is the shape the executor's taxonomy wants.
func (classifier) Classify(status int, body string) providers.ClassifiedError {
	switch status {
	case http.StatusUnauthorized:
		return providers.ClassifiedError{Kind: accounts.KindAuth, Status: status, Message: "autoclaw token rejected: " + upstreamMessage(body)}
	case http.StatusTooManyRequests:
		return providers.ClassifiedError{Kind: accounts.KindQuota, Status: status, Message: "autoclaw quota limited: " + upstreamMessage(body)}
	case http.StatusNotAcceptable, http.StatusForbidden:
		return providers.ClassifiedError{Kind: accounts.KindUnavailable, Status: status, Message: fmt.Sprintf("autoclaw rejected the request (status %d): %s", status, upstreamMessage(body))}
	default:
		return providers.ClassifiedError{Kind: accounts.KindUnavailable, Status: status, Message: upstreamMessage(body)}
	}
}
