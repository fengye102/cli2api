package autoclaw

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/caigee-cmd/cli2api/internal/accounts"
	"github.com/caigee-cmd/cli2api/internal/providers"
)

// classifyAutoclaw maps an upstream status + body to the internal taxonomy.
//
// AutoClaw has no structured quota code: the source proxy greps zero hits for
// 429 anywhere, and non-2xx handling is just "read message, throw". So the
// classification starts from the HTTP status and then refines it with the
// message, because the two measured 403 bodies carry completely different
// meanings:
//
//   - "GLM-5.3 免费额度已用完,开通连续包月会员…" is quota exhaustion. Treating it
//     as a generic failure kept the request failing on every retry while the
//     pool believed the account was perfectly healthy.
//   - "当前使用人数较多，系统繁忙，请稍后重试" is upstream congestion: the account
//     is fine and the same request succeeds seconds later.
//
// Request-shape rejections (406 with an empty body, 400/422) are the caller's
// fault, so they must not cool the account: this provider often runs a single
// account, and cooling it turns one bad request into a dead channel
// ("no autoclaw accounts available" for every caller behind it).
//
// 406 keeps an explanatory suffix because it arrives with an EMPTY body: without
// it the user cannot tell whether the gateway or their client is at fault, and
// the three known causes are each unguessable on their own.
func classifyAutoclaw(status int, body string) providers.ClassifiedError {
	message := upstreamMessage(body)
	switch status {
	case http.StatusUnauthorized:
		return providers.ClassifiedError{
			Kind:    accounts.KindAuth,
			Status:  http.StatusUnauthorized,
			Message: "autoclaw token rejected: " + message,
		}
	case http.StatusTooManyRequests:
		// Upstream never emits a structured quota code, so a 429 here is the
		// quota signal by convention (see the old classifier this replaced).
		return providers.ClassifiedError{
			Kind:    accounts.KindQuota,
			Status:  http.StatusTooManyRequests,
			Message: "autoclaw quota limited: " + message,
		}
	case http.StatusForbidden:
		if autoclawQuotaText(message) {
			return providers.ClassifiedError{
				Kind:    accounts.KindQuota,
				Status:  http.StatusForbidden,
				Message: "autoclaw quota exhausted: " + message,
			}
		}
		if autoclawBusyText(message) {
			return providers.ClassifiedError{
				Kind:    accounts.KindRateLimit,
				Status:  http.StatusForbidden,
				Message: "autoclaw upstream busy: " + message,
			}
		}
		return providers.ClassifiedError{
			Kind:    accounts.KindUnavailable,
			Status:  http.StatusForbidden,
			Message: "autoclaw forbidden: " + message + forbiddenHint,
		}
	case http.StatusNotAcceptable:
		return providers.ClassifiedError{
			Kind:    accounts.KindInvalidRequest,
			Status:  http.StatusNotAcceptable,
			Message: "autoclaw rejected the request shape (406): " + notAcceptableHint,
		}
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return providers.ClassifiedError{
			Kind:    accounts.KindInvalidRequest,
			Status:  status,
			Message: fmt.Sprintf("autoclaw rejected the request (status %d): %s", status, message),
		}
	default:
		return providers.ClassifiedError{
			Kind:    accounts.KindUnavailable,
			Status:  status,
			Message: fmt.Sprintf("autoclaw upstream returned %d: %s", status, message),
		}
	}
}

// newProviderError wraps classifyAutoclaw in the error type the adapter returns.
func newProviderError(status int, body string) error {
	classified := classifyAutoclaw(status, body)
	return &providers.Error{
		Kind:    classified.Kind,
		Status:  classified.Status,
		Message: classified.Message,
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

// autoclawQuotaText reports whether an upstream message is about exhausted
// quota rather than a passing rate limit.
func autoclawQuotaText(message string) bool {
	lower := strings.ToLower(message)
	if strings.Contains(lower, "insufficient_quota") ||
		strings.Contains(lower, "quota limit") ||
		strings.Contains(lower, "quota exceeded") ||
		strings.Contains(lower, "out of quota") {
		return true
	}
	for _, needle := range []string{"额度", "额度已用完", "余额不足", "购买加量包", "开通连续包月", "升级为连续包月"} {
		if strings.Contains(message, needle) {
			return true
		}
	}
	return false
}

// autoclawBusyText reports whether an upstream message is upstream congestion
// that clears by itself.
func autoclawBusyText(message string) bool {
	for _, needle := range []string{"繁忙", "稍后重试", "稍後重試", "请稍候", "人数较多", "too busy", "try again later"} {
		if strings.Contains(message, needle) {
			return true
		}
	}
	return false
}

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
	return classifyAutoclaw(status, body)
}
