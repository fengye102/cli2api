package minimax

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/caigee-cmd/cli2api/internal/providers"
)

// This is the console's "browser login" path for MiniMax Code. It is a device
// authorization, not a loopback redirect: the console shows the verification URL
// and the user code, the operator approves in any browser (including a phone),
// and PollLogin picks the token up. That is why BrowserLogin is advertised —
// there is a URL to open — while there is no callback URL to paste.

// StartLogin opens one device-authorization round and returns the URL to open.
func (c *Client) StartLogin(ctx context.Context, accountID string) (providers.LoginSession, error) {
	site := c.site(regionOfAccount(ctx, c.store, accountID))
	device, verifier, err := c.requestDeviceCode(ctx, accountID, site)
	if err != nil {
		return providers.LoginSession{}, err
	}
	now := c.clock()
	c.mu.Lock()
	for id, pending := range c.logins {
		if now.Sub(pending.startedAt) > loginStateTTL {
			delete(c.logins, id)
		}
	}
	c.logins[accountID] = &loginState{
		site:      site,
		device:    device,
		verifier:  verifier,
		startedAt: now,
	}
	c.mu.Unlock()
	return providers.LoginSession{AuthURL: device.VerificationUI, State: device.UserCode}, nil
}

// PollLogin advances one poll of the device round. It is called by the console
// on a timer; the interval reported by the upstream is honoured here so a fast
// poll does not trip slow_down.
func (c *Client) PollLogin(ctx context.Context, accountID string) (bool, string, error) {
	c.mu.Lock()
	state := c.logins[accountID]
	c.mu.Unlock()
	if state == nil {
		return false, "", fmt.Errorf("minimax 登录尚未开始，请先点击浏览器登录")
	}
	now := c.clock()
	if now.Sub(state.startedAt) > loginStateTTL {
		c.clearLogin(accountID)
		return false, "", fmt.Errorf("minimax 登录已超时，请重新发起")
	}
	c.mu.Lock()
	ready := state.lastPolled.IsZero() || now.Sub(state.lastPolled) >= state.device.Interval
	if !ready {
		message := firstNonEmpty(state.lastNote, waitingNote(state.device.UserCode))
		c.mu.Unlock()
		return false, message, nil
	}
	state.lastPolled = now
	c.mu.Unlock()

	result, err := c.pollDeviceToken(ctx, accountID, state.site, state.device, state.verifier)
	if err != nil {
		return false, "", err
	}
	switch result.Kind {
	case devicePollPending:
		message := waitingNote(state.device.UserCode)
		c.setLoginNote(accountID, message)
		return false, message, nil
	case devicePollSlowDown:
		c.slowDownLogin(accountID)
		message := waitingNote(state.device.UserCode)
		c.setLoginNote(accountID, message)
		return false, message, nil
	case devicePollDenied:
		c.clearLogin(accountID)
		return false, "", fmt.Errorf("minimax 授权被拒绝：%s", firstNonEmpty(result.Note, "access_denied"))
	case devicePollExpired:
		c.clearLogin(accountID)
		return false, "", fmt.Errorf("minimax 设备码已过期，请重新发起登录")
	}

	cred := Credential{
		Format:       CredentialFormat,
		AccessToken:  formatToken(result.Token.AccessToken),
		RefreshToken: strings.TrimSpace(result.Token.RefreshToken),
		ExpiresAt:    deadlineFrom(result.Token.ExpiresIn, c.clock()),
		Region:       state.site.Region,
		TokenType:    result.Token.TokenType,
	}
	if err := c.saveCredential(ctx, accountID, cred); err != nil {
		return false, "", err
	}
	c.dropCatalog(accountID)
	_ = c.store.Observe(ctx, accountID, cred.Subject, "ready", "", "")
	c.clearLogin(accountID)
	return true, "登录完成", nil
}

func waitingNote(userCode string) string {
	if userCode == "" {
		return "等待你在浏览器里完成授权"
	}
	return fmt.Sprintf("等待授权：请在浏览器打开链接并确认代码 %s", userCode)
}

func (c *Client) clearLogin(accountID string) {
	c.mu.Lock()
	delete(c.logins, accountID)
	c.mu.Unlock()
}

func (c *Client) setLoginNote(accountID, message string) {
	c.mu.Lock()
	if state := c.logins[accountID]; state != nil {
		state.lastNote = message
	}
	c.mu.Unlock()
}

// slowDownLogin backs the poll interval off, as RFC 8628 requires after a
// slow_down response.
func (c *Client) slowDownLogin(accountID string) {
	c.mu.Lock()
	if state := c.logins[accountID]; state != nil {
		state.device.Interval += 2 * time.Second
		if state.device.Interval > 15*time.Second {
			state.device.Interval = 15 * time.Second
		}
	}
	c.mu.Unlock()
}
