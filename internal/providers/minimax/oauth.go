package minimax

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// This file is the MiniMax Code sign-in and renewal side of the wire. It is the
// RFC 8628 device flow with two vendor quirks that are easy to get wrong:
//
//  1. The device-code response sometimes carries no device_code at all and the
//     token endpoint is polled with the user_code instead. Both spellings are
//     handled, and the poll remembers which one the round used.
//
//  2. Refresh tokens are single use. A second refresh with a spent token
//     answers invalid_grant, so a refresh has to be single-flight: two chats
//     racing a 401 must not spend two refresh tokens and log the account out.

// deviceAuth is one started device-code round.
type deviceAuth struct {
	DeviceCode     string
	UserCode       string
	VerificationUI string
	ExpiresIn      time.Duration
	Interval       time.Duration
	// PollByUser is set when the response had no device_code: the token
	// endpoint is then polled with the user code itself.
	PollByUser bool
}

// tokenSet is a successful token response.
type tokenSet struct {
	AccessToken  string
	RefreshToken string
	ExpiresIn    int64
	TokenType    string
}

type devicePollKind int

const (
	devicePollPending devicePollKind = iota
	devicePollSlowDown
	devicePollDenied
	devicePollExpired
	devicePollToken
)

type devicePoll struct {
	Kind  devicePollKind
	Token tokenSet
	Note  string
}

// pkce mints the verifier/challenge pair. The verifier is kept in memory for
// the life of the login round only; the challenge travels to the device-code
// endpoint.
func pkce() (verifier, challenge string, err error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", "", fmt.Errorf("minimax pkce: %w", err)
	}
	verifier = base64.RawURLEncoding.EncodeToString(buf)
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

// formPost sends an application/x-www-form-urlencoded request and returns the
// status and the raw body. The OAuth endpoints answer JSON but use non-2xx for
// control flow (authorization_pending is a 400), so the body is parsed by the
// caller, never by status alone.
func (c *Client) formPost(ctx context.Context, accountID, endpoint string, fields url.Values) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(fields.Encode()))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", UserAgent)
	client, err := c.httpClient(ctx, accountID)
	if err != nil {
		return 0, nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("minimax oauth: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("minimax oauth: read response: %w", err)
	}
	return resp.StatusCode, body, nil
}

// requestDeviceCode starts a device-authorization round.
func (c *Client) requestDeviceCode(ctx context.Context, accountID string, site Site) (deviceAuth, string, error) {
	verifier, challenge, err := pkce()
	if err != nil {
		return deviceAuth{}, "", err
	}
	fields := url.Values{
		"client_id":             {PublicClientID},
		"scope":                 {Scope},
		"audience":              {Audience},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
	}
	status, body, err := c.formPost(ctx, accountID, site.AccountHost+DeviceCodePath, fields)
	if err != nil {
		return deviceAuth{}, "", err
	}
	var payload struct {
		DeviceCode              string `json:"device_code"`
		UserCode                string `json:"user_code"`
		VerificationURI         string `json:"verification_uri"`
		VerificationURIComplete string `json:"verification_uri_complete"`
		VerificationURL         string `json:"verification_url"`
		ExpiresIn               int64  `json:"expires_in"`
		ExpiredIn               int64  `json:"expired_in"`
		Interval                int64  `json:"interval"`
		Error                   string `json:"error"`
		ErrorDescription        string `json:"error_description"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return deviceAuth{}, "", fmt.Errorf("minimax device code: HTTP %d: %s", status, clip(string(body), 200))
	}
	userCode := strings.TrimSpace(payload.UserCode)
	verify := firstNonEmpty(payload.VerificationURIComplete, payload.VerificationURI, payload.VerificationURL)
	if status >= 400 || userCode == "" || verify == "" {
		return deviceAuth{}, "", fmt.Errorf("minimax device code: HTTP %d: %s", status,
			firstNonEmpty(payload.Error, payload.ErrorDescription, clip(string(body), 200)))
	}
	deviceCode := strings.TrimSpace(payload.DeviceCode)
	pollByUser := deviceCode == ""
	if pollByUser {
		deviceCode = userCode
	}
	expires := payload.ExpiresIn
	if expires <= 0 {
		expires = payload.ExpiredIn
	}
	seconds := normalizeExpires(expires)
	if seconds <= 0 {
		seconds = 600
	}
	interval := payload.Interval
	if interval <= 0 {
		interval = 2
	}
	// The vendor has shipped the interval in milliseconds; a value that large
	// would make the console look frozen.
	if pollByUser && interval > 100 {
		interval = interval / 1000
	}
	if interval < 1 {
		interval = 1
	}
	return deviceAuth{
		DeviceCode:     deviceCode,
		UserCode:       userCode,
		VerificationUI: verify,
		ExpiresIn:      time.Duration(seconds) * time.Second,
		Interval:       time.Duration(interval) * time.Second,
		PollByUser:     pollByUser,
	}, verifier, nil
}

// pollDeviceToken advances one poll of a device round.
func (c *Client) pollDeviceToken(ctx context.Context, accountID string, site Site, device deviceAuth, verifier string) (devicePoll, error) {
	fields := url.Values{
		"grant_type":    {DeviceGrant},
		"client_id":     {PublicClientID},
		"code_verifier": {verifier},
	}
	if device.PollByUser {
		fields.Set("user_code", device.DeviceCode)
	} else {
		fields.Set("device_code", device.DeviceCode)
	}
	status, body, err := c.formPost(ctx, accountID, site.AccountHost+TokenPath, fields)
	if err != nil {
		return devicePoll{}, err
	}
	var payload struct {
		Status           string `json:"status"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
		AccessToken      string `json:"access_token"`
		RefreshToken     string `json:"refresh_token"`
		ExpiresIn        int64  `json:"expires_in"`
		ExpiredIn        int64  `json:"expired_in"`
		TokenType        string `json:"token_type"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return devicePoll{}, fmt.Errorf("minimax poll: HTTP %d: %s", status, clip(string(body), 200))
	}
	switch strings.ToLower(strings.TrimSpace(payload.Status)) {
	case "pending", "authorization_pending":
		return devicePoll{Kind: devicePollPending}, nil
	case "slow_down":
		return devicePoll{Kind: devicePollSlowDown}, nil
	case "denied", "access_denied":
		return devicePoll{Kind: devicePollDenied, Note: "授权被拒绝"}, nil
	case "expired", "expired_token":
		return devicePoll{Kind: devicePollExpired, Note: "设备码已过期"}, nil
	}
	switch strings.ToLower(strings.TrimSpace(payload.Error)) {
	case "authorization_pending":
		return devicePoll{Kind: devicePollPending}, nil
	case "slow_down":
		return devicePoll{Kind: devicePollSlowDown}, nil
	case "access_denied":
		return devicePoll{Kind: devicePollDenied, Note: "授权被拒绝"}, nil
	case "expired_token":
		return devicePoll{Kind: devicePollExpired, Note: "设备码已过期"}, nil
	case "":
	default:
		return devicePoll{Kind: devicePollDenied, Note: firstNonEmpty(payload.ErrorDescription, payload.Error)}, nil
	}
	if strings.TrimSpace(payload.AccessToken) == "" {
		return devicePoll{}, fmt.Errorf("minimax poll: HTTP %d 没有 access_token: %s", status, clip(string(body), 200))
	}
	return devicePoll{
		Kind: devicePollToken,
		Token: tokenSet{
			AccessToken:  payload.AccessToken,
			RefreshToken: payload.RefreshToken,
			ExpiresIn:    firstNonZero(payload.ExpiresIn, payload.ExpiredIn),
			TokenType:    payload.TokenType,
		},
	}, nil
}

// exchangeRefresh spends the refresh token. The caller must hold the per-account
// refresh slot: this call intentionally has no internal locking.
func (c *Client) exchangeRefresh(ctx context.Context, accountID string, site Site, refreshToken string) (tokenSet, error) {
	fields := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {PublicClientID},
		"scope":         {Scope},
		"audience":      {Audience},
	}
	status, body, err := c.formPost(ctx, accountID, site.AccountHost+TokenPath, fields)
	if err != nil {
		return tokenSet{}, err
	}
	var payload struct {
		AccessToken      string `json:"access_token"`
		RefreshToken     string `json:"refresh_token"`
		ExpiresIn        int64  `json:"expires_in"`
		ExpiredIn        int64  `json:"expired_in"`
		TokenType        string `json:"token_type"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return tokenSet{}, fmt.Errorf("minimax refresh: HTTP %d: %s", status, clip(string(body), 200))
	}
	if strings.TrimSpace(payload.AccessToken) == "" {
		reason := firstNonEmpty(payload.Error, payload.ErrorDescription, clip(string(body), 200))
		if status >= 400 || reason != "" {
			return tokenSet{}, &refreshError{Status: status, Reason: reason}
		}
		return tokenSet{}, fmt.Errorf("minimax refresh: HTTP %d 没有 access_token", status)
	}
	return tokenSet{
		AccessToken:  payload.AccessToken,
		RefreshToken: payload.RefreshToken,
		ExpiresIn:    firstNonZero(payload.ExpiresIn, payload.ExpiredIn),
		TokenType:    payload.TokenType,
	}, nil
}

// refreshError marks a refusal the account cannot recover from without a human:
// invalid_grant means the refresh token was already spent or revoked.
type refreshError struct {
	Status int
	Reason string
}

func (e *refreshError) Error() string {
	return fmt.Sprintf("minimax refresh 被拒绝 (HTTP %d)：%s", e.Status, e.Reason)
}

// normalizeExpires reduces a duration-or-epoch field to seconds.
func normalizeExpires(value int64) int64 {
	switch {
	case value <= 0:
		return 0
	case value > 1e15:
		return value / 1e6
	case value > 1e11:
		return value / 1e3
	case value > 1e9:
		return max(1, value-time.Now().Unix())
	default:
		return value
	}
}

func clip(text string, limit int) string {
	text = strings.TrimSpace(text)
	if len(text) <= limit {
		return text
	}
	return text[:limit] + "…"
}
