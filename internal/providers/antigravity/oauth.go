package antigravity

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/caigee-cmd/cli2api/internal/providers"
)

// Google OAuth for Antigravity: authorization-code exchange (browser login) and
// grant_type=refresh_token renewal, both against the same token endpoint.
//
// The refresh token is the account's main credential (it lives until revoked);
// the access token is a roughly one-hour cache. Google only hands a refresh
// token out on the first consent, so a response that omits it must leave the
// stored value alone rather than blanking it.
//
// Error handling follows the sibling providers: a permanent failure becomes a
// recognizable auth error telling the operator to re-authorize, and everything
// else stays retryable. There is no "disable the account" path in this gateway.

const (
	// oauthRequestTimeout bounds one token/userinfo request.
	oauthRequestTimeout = 30 * time.Second
	// userInfoTimeout is shorter: fetching an email must not stall a login.
	userInfoTimeout = 20 * time.Second
	// defaultExpiresInSeconds is the conservative fallback when upstream omits
	// expires_in.
	defaultExpiresInSeconds = 3600
	// maxOAuthErrorChars truncates upstream error text in messages.
	maxOAuthErrorChars = 300
	// maxCodeLength guards against a pasted blob being treated as a code.
	maxCodeLength = 8192
	// maxStateLength bounds the state parameter we accept back.
	maxStateLength = 512
)

// tokenResponse is one token-endpoint reading.
type tokenResponse struct {
	AccessToken  string
	RefreshToken string
	ExpiresIn    int64
	TokenType    string
}

// expiresAtMillis converts expires_in into an absolute millisecond deadline,
// clamping a dirty upstream value to the conservative default.
func (t tokenResponse) expiresAtMillis(now time.Time) int64 {
	seconds := t.ExpiresIn
	if seconds < 1 || seconds > 7*24*3600 {
		seconds = defaultExpiresInSeconds
	}
	return now.UnixMilli() + seconds*1000
}

// truncateText collapses control characters and cuts to a character budget.
func truncateText(text string) string {
	var builder strings.Builder
	count := 0
	for _, ch := range text {
		if ch < 0x20 && ch != '\t' {
			continue
		}
		if count >= maxOAuthErrorChars {
			builder.WriteString("…")
			break
		}
		builder.WriteRune(ch)
		count++
	}
	return builder.String()
}

// upstreamErrorParts reads error / error_description from an upstream body.
func upstreamErrorParts(payload map[string]any) (string, string) {
	code, _ := payload["error"].(string)
	description, _ := payload["error_description"].(string)
	return strings.TrimSpace(code), strings.TrimSpace(description)
}

// decodeJSONObject parses a body leniently; a non-JSON body yields an empty map.
func decodeJSONObject(raw []byte) map[string]any {
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return map[string]any{}
	}
	return payload
}

// readTokenResponse parses a 2xx token payload. missingAccessToken is the
// caller's message for the one shape both flows reject the same way.
func readTokenResponse(payload map[string]any, missingAccessToken string) (tokenResponse, error) {
	accessToken := strings.TrimSpace(stringValueOf(payload["access_token"]))
	if accessToken == "" {
		return tokenResponse{}, &providers.Error{Kind: "upstream", Status: 502, Message: missingAccessToken}
	}
	refreshToken := strings.TrimSpace(stringValueOf(payload["refresh_token"]))
	tokenType := strings.TrimSpace(stringValueOf(payload["token_type"]))
	if tokenType == "" {
		tokenType = "Bearer"
	}
	return tokenResponse{
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		ExpiresIn:    intValueOf(payload["expires_in"]),
		TokenType:    tokenType,
	}, nil
}

// stringValueOf reads a string field, accepting a numeric value as its decimal
// form (upstream occasionally quotes numbers the other way around).
func stringValueOf(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case float64:
		return fmt.Sprintf("%v", typed)
	case json.Number:
		return typed.String()
	default:
		return ""
	}
}

// intValueOf reads an integer field in either numeric or numeric-string form.
func intValueOf(value any) int64 {
	switch typed := value.(type) {
	case float64:
		return int64(typed)
	case json.Number:
		if parsed, err := typed.Int64(); err == nil {
			return parsed
		}
	case string:
		var parsed int64
		if _, err := fmt.Sscanf(strings.TrimSpace(typed), "%d", &parsed); err == nil {
			return parsed
		}
	}
	return 0
}

// classifyRefreshFailure turns a failed refresh into a gateway error. A revoked
// or rejected refresh token is permanent (the operator must re-authorize);
// anything else is retryable.
func classifyRefreshFailure(status int, body string, payload map[string]any) error {
	code, description := upstreamErrorParts(payload)
	detail := description
	if detail == "" {
		detail = truncateText(body)
	}
	switch code {
	case "invalid_grant":
		return &providers.Error{
			Kind:    "auth",
			Status:  401,
			Message: "Antigravity 的 refresh token 已失效或被撤销（Google 返回 invalid_grant）：请在授权页重新登录，或粘贴新的 refresh token",
		}
	case "invalid_client", "unauthorized_client":
		return &providers.Error{
			Kind:    "auth",
			Status:  401,
			Message: "Antigravity 的 OAuth 客户端未被 Google 接受（" + code + "）：请重新登录该账号",
		}
	}
	message := fmt.Sprintf("Antigravity 刷新 token 失败（HTTP %d）", status)
	if detail != "" {
		message += "：" + detail
	}
	return &providers.Error{Kind: "upstream", Status: 502, Message: message}
}

// classifyExchangeFailure turns a failed code exchange into a gateway error.
// invalid_grant here means the code expired or was already used, so the action
// is a fresh login rather than a fresh token paste.
func classifyExchangeFailure(status int, body string, payload map[string]any) error {
	code, description := upstreamErrorParts(payload)
	detail := description
	if detail == "" {
		detail = truncateText(body)
	}
	hint := ""
	if detail != "" {
		hint = "：" + detail
	}
	switch code {
	case "invalid_grant":
		return &providers.Error{Kind: "auth", Status: 400, Message: "Antigravity 授权码已失效或已被使用，请重新发起网页登录" + hint}
	case "redirect_uri_mismatch":
		return &providers.Error{Kind: "auth", Status: 400, Message: "Antigravity 授权回调地址与发起时不一致，请重新发起网页登录" + hint}
	case "invalid_client", "unauthorized_client":
		return &providers.Error{Kind: "auth", Status: 401, Message: "Antigravity 的 OAuth 客户端未被 Google 接受（" + code + "）：请重新发起网页登录" + hint}
	}
	return &providers.Error{Kind: "upstream", Status: 502, Message: fmt.Sprintf("Antigravity 网页登录换取凭证失败（HTTP %d）%s", status, hint)}
}

// postTokenForm sends one form-encoded token request and returns status + body.
func (c *Client) postTokenForm(ctx context.Context, accountID string, form url.Values) (int, []byte, error) {
	client, err := c.httpClient(ctx, accountID)
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, googleTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", oauthUserAgent())
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, &providers.Error{
			Kind:    "upstream",
			Status:  502,
			Message: "Antigravity 访问 Google OAuth 失败：" + err.Error() + "（本机访问 Google 需经代理时，请给该账号配置出网代理）",
		}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, body, nil
}

// refreshAccessToken mints a new access token from the stored refresh token.
// Pure network + parse; persistence belongs to the caller.
func (c *Client) refreshAccessToken(ctx context.Context, accountID, refreshToken string) (tokenResponse, error) {
	token := strings.TrimSpace(refreshToken)
	if token == "" {
		return tokenResponse{}, &providers.Error{
			Kind:    "auth",
			Status:  401,
			Message: "Antigravity 账号缺少 refresh token，请重新登录或粘贴（Google OAuth 的 refresh_token）",
		}
	}
	form := url.Values{}
	form.Set("client_id", oauthClientID)
	form.Set("client_secret", oauthClientSecret)
	form.Set("refresh_token", token)
	form.Set("grant_type", grantTypeRefresh)
	status, body, err := c.postTokenForm(ctx, accountID, form)
	if err != nil {
		return tokenResponse{}, err
	}
	payload := decodeJSONObject(body)
	if status < 200 || status >= 300 {
		return tokenResponse{}, classifyRefreshFailure(status, string(body), payload)
	}
	return readTokenResponse(payload, "Antigravity 刷新响应缺少 access_token，旧凭证未被覆盖")
}

// exchangeAuthorizationCode trades the browser callback's code for tokens.
// redirectURI must be byte-identical to the one used to build the authorize URL.
func (c *Client) exchangeAuthorizationCode(ctx context.Context, code, redirectURI string) (tokenResponse, error) {
	trimmed := strings.TrimSpace(code)
	if trimmed == "" {
		return tokenResponse{}, &providers.Error{Kind: "auth", Status: 400, Message: "缺少授权码，请重新发起网页登录"}
	}
	if len([]rune(trimmed)) > maxCodeLength {
		return tokenResponse{}, &providers.Error{Kind: "auth", Status: 400, Message: "授权码过长，请确认复制的是回调链接里的 code"}
	}
	form := url.Values{}
	form.Set("client_id", oauthClientID)
	form.Set("client_secret", oauthClientSecret)
	form.Set("code", trimmed)
	form.Set("redirect_uri", redirectURI)
	form.Set("grant_type", grantTypeCode)
	status, body, err := c.postTokenForm(ctx, "", form)
	if err != nil {
		return tokenResponse{}, err
	}
	payload := decodeJSONObject(body)
	if status < 200 || status >= 300 {
		return tokenResponse{}, classifyExchangeFailure(status, string(body), payload)
	}
	return readTokenResponse(payload, "Antigravity 换码响应缺少 access_token，账号未保存")
}

// buildAuthorizeURL assembles the Google consent URL. Parameter names and order
// follow the reference implementation verbatim; renaming any of them breaks the
// round trip.
func buildAuthorizeURL(redirectURI, state string) string {
	query := url.Values{}
	query.Set("client_id", oauthClientID)
	query.Set("redirect_uri", redirectURI)
	query.Set("response_type", "code")
	query.Set("scope", strings.Join(oauthScopes, " "))
	query.Set("access_type", "offline")
	query.Set("prompt", "consent")
	query.Set("include_granted_scopes", "true")
	query.Set("state", state)
	return googleAuthURL + "?" + query.Encode()
}

// fetchEmail reads the account email from userinfo (best effort): v2 then v1.
// Callers treat it as display enrichment only and never fail a login over it.
func (c *Client) fetchEmail(ctx context.Context, accessToken string) string {
	token := strings.TrimSpace(accessToken)
	if token == "" {
		return ""
	}
	client, err := c.httpClient(ctx, "")
	if err != nil {
		return ""
	}
	for _, endpoint := range []string{userInfoV2URL, userInfoV1URL} {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			continue
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", oauthUserAgent())
		clientWithTimeout := *client
		clientWithTimeout.Timeout = userInfoTimeout
		resp, err := clientWithTimeout.Do(req)
		if err != nil {
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			continue
		}
		if email := strings.TrimSpace(stringValueOf(decodeJSONObject(body)["email"])); email != "" {
			return email
		}
	}
	return ""
}
