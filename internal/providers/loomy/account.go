package loomy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/caigee-cmd/cli2api/internal/accounts"
	"github.com/caigee-cmd/cli2api/internal/providers"
)

// Account plane: login on account.xfinfr.com.
//
// Every call is POST with a JSON envelope
//
//	{"base": {appid, modelid, version, devid, ua, traceid}, "param": {...}}
//
// and every call is signed (signer.go). Responses are
//
//	{"code":"000000","desc":"…","data":{…}}
//
// with 100001 = bad request and 100002 = auth problem. The session the login
// endpoints return under data.session is the bearer the model gateway accepts.

const maxAccountBody = 1 << 20

// accountEnvelope is the body shape the service expects.
type accountEnvelope struct {
	Base  map[string]string `json:"base"`
	Param map[string]any    `json:"param"`
}

// accountReply is the reply envelope. `data` is kept raw so each endpoint can
// decode its own shape.
type accountReply struct {
	Code    json.RawMessage `json:"code"`
	Desc    string          `json:"desc"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

func (reply accountReply) code() string {
	raw := strings.TrimSpace(string(reply.Code))
	raw = strings.Trim(raw, `"`)
	return raw
}

// base builds the per-request envelope header. `traceid` is regenerated on
// every request, exactly like the client: the service treats it as a per-call
// trace, not as part of the device identity.
func (c *Client) accountHeader(cred Credential) map[string]string {
	devid := firstNonEmpty(cred.DeviceID, clientDeviceID)
	return map[string]string{
		"appid":   accountAppID,
		"modelid": clientWebModelID,
		"version": clientVersion,
		"devid":   devid,
		"ua":      clientUA,
		"traceid": randomHex(32),
	}
}

// accountCall performs one signed account-service POST.
func (c *Client) accountCall(ctx context.Context, accountID, path string, param map[string]any, cred Credential) (json.RawMessage, error) {
	envelope := accountEnvelope{Base: c.accountHeader(cred), Param: param}
	body, err := json.Marshal(envelope)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, accountTimeout)
	defer cancel()

	endpoint := strings.TrimRight(c.accountBase, "/") + path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for key, value := range accountHeaders(http.MethodPost, path, string(body), nil, "", nil) {
		req.Header.Set(key, value)
	}
	req.Header.Set("Accept", "application/json")

	client, err := c.httpClient(ctx, accountID)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, &providers.Error{Kind: accounts.KindUnavailable, Message: "讯飞账号服务不可达：" + err.Error()}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxAccountBody))

	var reply accountReply
	if err := json.Unmarshal(raw, &reply); err != nil {
		return nil, &providers.Error{
			Kind:    accounts.KindUnavailable,
			Status:  resp.StatusCode,
			Message: fmt.Sprintf("讯飞账号服务返回 HTTP %d：%s", resp.StatusCode, trimBody(string(raw))),
		}
	}
	if code := reply.code(); code != "" && code != onboardingOKCode {
		message := firstNonEmpty(reply.Desc, reply.Message, code)
		kind := accounts.KindAuth
		switch code {
		case "100001":
			kind = accounts.KindInvalidRequest
		case onboardingAuthCode:
			kind = accounts.KindAuth
		}
		return nil, &providers.Error{Kind: kind, Status: resp.StatusCode, Message: path + "：" + message, Code: code}
	}
	if resp.StatusCode >= 400 {
		message := firstNonEmpty(reply.Message, reply.Desc, trimBody(string(raw)))
		return nil, &providers.Error{
			Kind:    accounts.KindUnavailable,
			Status:  resp.StatusCode,
			Message: fmt.Sprintf("%s HTTP %d：%s", path, resp.StatusCode, message),
		}
	}
	return reply.Data, nil
}

// loginSession is what a successful login yields.
type loginSession struct {
	Session string
	UserID  string
	Phone   string
}

// passwordLogin runs the two-step password round: fetch the RSA key + rcode,
// encrypt, then submit.
func (c *Client) passwordLogin(ctx context.Context, accountID string, cred Credential) (loginSession, error) {
	if cred.LoginID == "" || cred.Password == "" {
		return loginSession{}, &providers.Error{Kind: accounts.KindAuth, Message: "loomy 账号缺少手机号或密码，无法登录"}
	}
	data, err := c.accountCall(ctx, accountID, accountPukeyPath, nil, cred)
	if err != nil {
		return loginSession{}, err
	}
	var pukey struct {
		Pukey string `json:"pukey"`
		Rcode string `json:"rcode"`
	}
	if err := json.Unmarshal(data, &pukey); err != nil || strings.TrimSpace(pukey.Pukey) == "" {
		return loginSession{}, &providers.Error{Kind: accounts.KindUnavailable, Message: "讯飞账号服务未返回公钥"}
	}
	encrypted, err := encryptPassword(pukey.Pukey, cred.Password)
	if err != nil {
		return loginSession{}, &providers.Error{Kind: accounts.KindUnavailable, Message: "密码加密失败：" + err.Error()}
	}

	data, err = c.accountCall(ctx, accountID, accountPwdLoginPath, map[string]any{
		"loginid":  cred.LoginID,
		"password": encrypted,
		"rcode":    pukey.Rcode,
		"type":     1,
		"expire":   sessionExpireSeconds,
	}, cred)
	if err != nil {
		return loginSession{}, err
	}
	return decodeLoginSession(data)
}

// sendSMSCode asks the service to text a login code. The reply carries the
// msgid that the verification step has to quote back.
func (c *Client) sendSMSCode(ctx context.Context, accountID string, cred Credential) (string, error) {
	data, err := c.accountCall(ctx, accountID, accountSendMsgPath, map[string]any{
		"ccode":  "86",
		"phone":  cred.LoginID,
		"expire": 300,
	}, cred)
	if err != nil {
		return "", err
	}
	var reply struct {
		MsgID     string `json:"msgid"`
		MsgIDCaps string `json:"msgId"`
	}
	_ = json.Unmarshal(data, &reply)
	return firstNonEmpty(reply.MsgID, reply.MsgIDCaps), nil
}

// smsLogin verifies the code and returns the session.
func (c *Client) smsLogin(ctx context.Context, accountID string, cred Credential, code, msgID string) (loginSession, error) {
	data, err := c.accountCall(ctx, accountID, accountCheckCodePath, map[string]any{
		"ccode":  "86",
		"phone":  cred.LoginID,
		"mcode":  code,
		"msgid":  msgID,
		"expire": sessionExpireSeconds,
	}, cred)
	if err != nil {
		return loginSession{}, err
	}
	return decodeLoginSession(data)
}

// decodeLoginSession digs session/userid out of whatever wrapper the endpoint
// used (`data` object, or the payload itself).
func decodeLoginSession(data json.RawMessage) (loginSession, error) {
	var node map[string]json.RawMessage
	if err := json.Unmarshal(data, &node); err != nil {
		return loginSession{}, &providers.Error{Kind: accounts.KindUnavailable, Message: "登录响应无法解析"}
	}
	if inner, ok := node["data"]; ok && len(inner) > 0 && string(inner) != "null" {
		var nested map[string]json.RawMessage
		if json.Unmarshal(inner, &nested) == nil {
			node = nested
		}
	}
	value := func(keys ...string) string {
		for _, key := range keys {
			raw, ok := node[key]
			if !ok {
				continue
			}
			var text string
			if json.Unmarshal(raw, &text) == nil && strings.TrimSpace(text) != "" {
				return strings.TrimSpace(text)
			}
			var number json.Number
			if json.Unmarshal(raw, &number) == nil && number.String() != "" {
				return number.String()
			}
		}
		return ""
	}
	session := value("session")
	if session == "" {
		return loginSession{}, &providers.Error{Kind: accounts.KindAuth, Message: "登录成功但未返回 session"}
	}
	return loginSession{
		Session: session,
		UserID:  firstNonEmpty(value("userid"), value("userId")),
		Phone:   firstNonEmpty(value("phone"), value("loginid")),
	}, nil
}

// loginAndStore performs a password login and persists the credential with the
// new session and its expiry.
func (c *Client) loginAndStore(ctx context.Context, accountID string, cred Credential) (Credential, error) {
	result, err := c.passwordLogin(ctx, accountID, cred)
	if err != nil {
		return cred, err
	}
	if cred.DeviceID == "" {
		// One device per account, minted once and then reused forever: the
		// service keys sessions to it.
		cred.DeviceID = "web-" + randomHex(16)
	}
	cred.Session = result.Session
	if result.UserID != "" {
		cred.UserID = result.UserID
	}
	if result.Phone != "" && cred.LoginID == "" {
		cred.LoginID = result.Phone
	}
	cred.ExpiresAt = time.Now().Add(sessionExpireSeconds * time.Second).Unix()
	if err := c.save(ctx, accountID, cred); err != nil {
		return cred, err
	}
	return cred, nil
}

// RenewSession mints a fresh 14-day session from the stored phone number and
// password. The maintenance loop calls it every hour for every enabled account,
// so the gate has to be cheap and silent: it returns nil — doing nothing — when
// the session is still fresh, when the account has no password to renew with
// (SMS-only or imported-session accounts), or when there is no stored credential
// yet (an account row created by the add-account dialog before its first login).
func (c *Client) RenewSession(ctx context.Context, accountID string) error {
	cred, err := c.credentials(ctx, accountID)
	if err != nil {
		if isMissingCredential(err) {
			return nil
		}
		return err
	}
	if strings.TrimSpace(cred.Password) == "" || len(normalizePhone(cred.LoginID)) != 11 {
		// Nothing to renew with: the operator has to supply a code (or a new
		// session) on day 14, exactly as before.
		return nil
	}
	if cred.ExpiresAt > 0 {
		remaining := time.Until(time.Unix(cred.ExpiresAt, 0))
		// sessionRenewLeadSeconds is seconds; unsigned comparison against a
		// Duration would compare nanoseconds and never renew.
		if remaining > sessionRenewLeadSeconds*time.Second {
			return nil
		}
		// An expiry the service handed back in the past means the session is
		// already dead; renewing is the only way back.
	}
	if _, err := c.loginAndStore(ctx, accountID, cred); err != nil {
		return fmt.Errorf("renew loomy session: %w", err)
	}
	return nil
}
