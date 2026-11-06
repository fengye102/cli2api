package loomy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/caigee-cmd/cli2api/internal/accounts"
	"github.com/caigee-cmd/cli2api/internal/providers"
)

// Interactive login.
//
// Loomy has no browser round: the account service takes a phone number and
// either a password or an SMS code. The password path needs no operator at all
// (import the credential, the adapter logs in), so the interactive flow exists
// for accounts that have no password to store — a phone number plus one SMS.
//
// It runs in two pastes through the console's existing "paste a value" box,
// exactly like AutoClaw's domestic round:
//
//	start  → nothing to open; the console asks for the phone number
//	paste 11-digit phone → the service texts a code, the round remembers msgid
//	paste 6-digit code  → session stored, the account is ready
//
// The round lives in memory only. A restart mid-round costs one extra SMS.

const loginRoundTTL = 10 * time.Minute

type loginRound struct {
	Phone string
	MsgID string
	At    time.Time
}

func (c *Client) rememberRound(accountID string, round loginRound) {
	round.At = time.Now()
	c.roundsMu.Lock()
	defer c.roundsMu.Unlock()
	if c.rounds == nil {
		c.rounds = map[string]*loginRound{}
	}
	c.rounds[accountID] = &round
}

func (c *Client) round(accountID string) (loginRound, bool) {
	c.roundsMu.Lock()
	defer c.roundsMu.Unlock()
	round, ok := c.rounds[accountID]
	if !ok {
		return loginRound{}, false
	}
	if time.Since(round.At) > loginRoundTTL {
		delete(c.rounds, accountID)
		return loginRound{}, false
	}
	return *round, true
}

func (c *Client) forgetRound(accountID string) {
	c.roundsMu.Lock()
	defer c.roundsMu.Unlock()
	delete(c.rounds, accountID)
}

// loginFlow implements providers.LoginSessionProvider and LoginCompleter.
type loginFlow struct{ client *Client }

// StartLogin opens the SMS round. When the console already collected a phone
// number (LoginInput.Phone) the code goes out immediately; otherwise the round
// starts empty and waits for the operator to paste it.
func (f loginFlow) StartLogin(ctx context.Context, accountID string) (providers.LoginSession, error) {
	c := f.client
	if _, err := c.store.Get(ctx, accountID); err != nil {
		return providers.LoginSession{}, err
	}
	phone := normalizePhone(providers.LoginInputFromContext(ctx).Phone)
	if phone == "" {
		c.rememberRound(accountID, loginRound{})
		return providers.LoginSession{State: "sms"}, nil
	}
	cred, err := c.credentials(ctx, accountID)
	if err != nil && !isMissingCredential(err) {
		return providers.LoginSession{}, err
	}
	cred.LoginID = phone
	msgID, err := c.sendSMSCode(ctx, accountID, cred)
	if err != nil {
		return providers.LoginSession{}, err
	}
	c.rememberRound(accountID, loginRound{Phone: phone, MsgID: msgID})
	return providers.LoginSession{State: "sms"}, nil
}

// PollLogin tells the operator what to paste next. It never finishes a round by
// itself.
func (f loginFlow) PollLogin(ctx context.Context, accountID string) (bool, string, error) {
	if _, err := f.client.store.Get(ctx, accountID); err != nil {
		return false, "", err
	}
	round, ok := f.client.round(accountID)
	if !ok {
		return false, "", nil
	}
	if round.Phone == "" {
		return false, "Loomy 走手机验证码：把 11 位手机号粘贴到下面的框，点“完成登录”后会给你发验证码", nil
	}
	return false, fmt.Sprintf("验证码已发到 %s：把收到的 6 位数字粘贴到下面的框再点“完成登录”", maskPhone(round.Phone)), nil
}

// CompleteLogin takes the operator's paste. Both a phone number and an SMS code
// are digits, so the pending round decides: before it knows the phone, digits
// are the phone; after, they are the code.
func (f loginFlow) CompleteLogin(ctx context.Context, accountID, pasted string) error {
	c := f.client
	if _, err := c.store.Get(ctx, accountID); err != nil {
		return err
	}
	value := strings.TrimSpace(pasted)
	if value == "" {
		return &providers.Error{Kind: accounts.KindAuth, Message: "先粘贴手机号或验证码"}
	}

	round, ok := c.round(accountID)
	if !ok {
		round = loginRound{}
	}

	// A password pair is also accepted here: "手机号 密码" (or the two on
	// separate lines) stores the credentials and logs in without an SMS, which
	// is what an operator wants when they already know the password.
	if round.Phone == "" {
		if phone, password, ok := splitCredentialPaste(value); ok {
			cred, err := c.credentials(ctx, accountID)
			if err != nil && !isMissingCredential(err) {
				return err
			}
			cred.LoginID = phone
			cred.Password = password
			if _, err := c.loginAndStore(ctx, accountID, cred); err != nil {
				return err
			}
			c.forgetRound(accountID)
			return nil
		}
	}

	if round.Phone == "" {
		phone := normalizePhone(value)
		if len(phone) != 11 {
			return &providers.Error{Kind: accounts.KindAuth, Message: "这不是 11 位手机号：粘贴手机号，或“手机号 密码”两个值"}
		}
		cred, err := c.credentials(ctx, accountID)
		if err != nil && !isMissingCredential(err) {
			return err
		}
		cred.LoginID = phone
		msgID, err := c.sendSMSCode(ctx, accountID, cred)
		if err != nil {
			return err
		}
		c.rememberRound(accountID, loginRound{Phone: phone, MsgID: msgID})
		return nil
	}

	code := digitsOnly(value)
	if len(code) != 6 {
		return &providers.Error{Kind: accounts.KindAuth, Message: "这不是 6 位验证码"}
	}
	cred, err := c.credentials(ctx, accountID)
	if err != nil && !isMissingCredential(err) {
		return err
	}
	cred.LoginID = round.Phone
	result, err := c.smsLogin(ctx, accountID, cred, code, round.MsgID)
	if err != nil {
		return err
	}
	if cred.DeviceID == "" {
		cred.DeviceID = "web-" + randomHex(16)
	}
	cred.Session = result.Session
	if result.UserID != "" {
		cred.UserID = result.UserID
	}
	cred.ExpiresAt = time.Now().Add(sessionExpireSeconds * time.Second).Unix()
	cred.Format = CredentialFormat
	if err := c.save(ctx, accountID, cred); err != nil {
		return err
	}
	c.forgetRound(accountID)
	return nil
}

// isMissingCredential reports whether the store simply has nothing stored yet,
// which is the normal state right after an account row is created for a login.
func isMissingCredential(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, accounts.ErrAccountNotFound) {
		return true
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "not found") ||
		strings.Contains(message, "no credential") ||
		strings.Contains(message, "record not found") ||
		strings.Contains(message, "does not exist")
}

// splitCredentialPaste reads "13800000000 password" (space, tab, comma or
// newline separated).
func splitCredentialPaste(value string) (phone, password string, ok bool) {
	fields := strings.FieldsFunc(value, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == ',' || r == ':'
	})
	if len(fields) < 2 {
		return "", "", false
	}
	candidate := normalizePhone(fields[0])
	if len(candidate) != 11 {
		return "", "", false
	}
	password = strings.TrimSpace(strings.Join(fields[1:], " "))
	if password == "" {
		return "", "", false
	}
	return candidate, password, true
}

func digitsOnly(value string) string {
	var out strings.Builder
	for _, r := range value {
		if r >= '0' && r <= '9' {
			out.WriteRune(r)
		}
	}
	return out.String()
}

func maskPhone(phone string) string {
	if len(phone) != 11 {
		return phone
	}
	return phone[:3] + "****" + phone[7:]
}
