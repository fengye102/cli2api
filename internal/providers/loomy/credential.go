package loomy

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Credential is the canonical storage payload for provider=loomy.
//
// Three shapes are accepted on import, because all three are things an operator
// can actually get hold of:
//
//	{"loginid":"138…","password":"…"}          → the adapter logs in itself, and
//	                                             re-logs in for the whole life of
//	                                             the account (preferred)
//	{"session":"…"}                             → a 14-day session token lifted
//	                                             from a running client; works
//	                                             until it expires, cannot renew
//	{"name":"…","loginid":"…","password":"…",
//	 "session":"…","expire_at":…,"identity":{…}}→ one entry of loomy2api's
//	                                             accounts.json, imported verbatim
//
// The identity block is what makes an account look like one stable device to
// the account service; it is minted on first login and reused forever after
// (mintIdentity), so it has to be persisted next to the session.
type Credential struct {
	Format string `json:"format"`

	// LoginID is the phone number the account signs in with.
	LoginID string `json:"loginid,omitempty"`
	// Password is kept only so the adapter can renew a 14-day session by
	// itself; there is no refresh token to lean on. It sits in the account
	// credential payload, which is exactly as protected as every other
	// provider's token in this store.
	Password string `json:"password,omitempty"`

	// Session is the bearer the model gateway accepts.
	Session string `json:"session,omitempty"`
	// UserID is the account-service user id, for the console only.
	UserID string `json:"userid,omitempty"`
	// ExpiresAt is when the session dies (Unix seconds, 0 = unknown).
	ExpiresAt int64 `json:"expires_at,omitempty"`

	// Device identity. devid is what the account service treats as the
	// device; the rest are the client's own values.
	DeviceID       string `json:"devid,omitempty"`
	CampusDeviceID string `json:"campus_device_id,omitempty"`
}

// identityAlias mirrors Credential plus every spelling an operator's export may
// use. Loomy 2 API writes snake_case (`loginid`, `expire_at`) and nests the
// device identity under `identity`.
type identityAlias struct {
	Format           string `json:"format"`
	LoginID          string `json:"loginid"`
	LoginIDSnake     string `json:"login_id"`
	LoginIDCamel     string `json:"loginId"`
	Phone            string `json:"phone"`
	Password         string `json:"password"`
	Passwd           string `json:"passwd"`
	Session          string `json:"session"`
	Token            string `json:"token"`
	AccessToken      string `json:"access_token"`
	AccessTokenCamel string `json:"accessToken"`
	APIKey           string `json:"api_key"`
	UserID           string `json:"userid"`
	UserIDCamel      string `json:"userId"`
	ExpiresAt        int64  `json:"expires_at"`
	ExpiresAtCamel   int64  `json:"expiresAt"`
	ExpireAt         int64  `json:"expire_at"`
	DeviceID         string `json:"devid"`
	DeviceIDSnake    string `json:"device_id"`
	Campus           string `json:"campus_device_id"`
	Identity         struct {
		DeviceID  string `json:"devid"`
		Campus    string `json:"campus_device_id"`
		UserAgent string `json:"ua"`
		ModelID   string `json:"modelid"`
		Version   string `json:"version"`
	} `json:"identity"`
}

// DecodeCredential accepts the canonical form or the aliases above. A bare
// string payload is read as a session token, which is what the console's
// "paste a token" box produces.
func DecodeCredential(payload []byte) (Credential, error) {
	var alias identityAlias
	if err := json.Unmarshal(payload, &alias); err != nil {
		var bare string
		if stringErr := json.Unmarshal(payload, &bare); stringErr == nil {
			bare = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(bare), "Bearer "))
			if bare == "" {
				return Credential{}, fmt.Errorf("loomy credential: empty session token")
			}
			return Credential{Format: CredentialFormat, Session: bare}, nil
		}
		raw := strings.TrimSpace(string(payload))
		if raw == "" || strings.ContainsAny(raw, "{}[]") {
			return Credential{}, fmt.Errorf("loomy credential: %w", err)
		}
		return Credential{Format: CredentialFormat, Session: raw}, nil
	}

	cred := Credential{
		Format:         alias.Format,
		LoginID:        firstNonEmpty(alias.LoginID, alias.LoginIDSnake, alias.LoginIDCamel, alias.Phone),
		Password:       firstNonEmpty(alias.Password, alias.Passwd),
		Session:        firstNonEmpty(alias.Session, alias.Token, alias.AccessToken, alias.AccessTokenCamel, alias.APIKey),
		UserID:         firstNonEmpty(alias.UserID, alias.UserIDCamel),
		DeviceID:       firstNonEmpty(alias.DeviceID, alias.DeviceIDSnake, alias.Identity.DeviceID),
		CampusDeviceID: firstNonEmpty(alias.Campus, alias.Identity.Campus),
	}
	cred.ExpiresAt = firstNonZero(alias.ExpiresAt, alias.ExpiresAtCamel, alias.ExpireAt)
	if cred.Format == "" {
		cred.Format = CredentialFormat
	}
	cred.Session = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(cred.Session), "Bearer "))
	cred.LoginID = normalizePhone(cred.LoginID)
	cred.Password = strings.TrimSpace(cred.Password)
	return cred, nil
}

// EncodeCredential serializes the canonical payload.
func EncodeCredential(cred Credential) ([]byte, error) {
	if cred.Format == "" {
		cred.Format = CredentialFormat
	}
	return json.Marshal(cred)
}

// ValidateCredential checks that the payload can serve a request, now or after
// a login the adapter is able to perform by itself.
func ValidateCredential(payload []byte) error {
	cred, err := DecodeCredential(payload)
	if err != nil {
		return err
	}
	if strings.TrimSpace(cred.Session) != "" {
		return nil
	}
	if cred.LoginID != "" && cred.Password != "" {
		return nil
	}
	return fmt.Errorf("loomy credential needs a session token, or a login id and password so the adapter can obtain one")
}

// Ready reports whether an imported account can serve traffic immediately: a
// session does, and so does a password the adapter can log in with.
func (cred Credential) Ready() bool {
	return strings.TrimSpace(cred.Session) != "" || (cred.LoginID != "" && cred.Password != "")
}

// CanRenew reports whether the adapter can replace an expired session.
func (cred Credential) CanRenew() bool {
	return cred.LoginID != "" && cred.Password != ""
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func firstNonZero(values ...int64) int64 {
	for _, value := range values {
		if value > 0 {
			return value
		}
	}
	return 0
}

// normalizePhone strips the separators an operator's export tends to carry and
// the +86 / 86 country prefix the service does not want.
func normalizePhone(raw string) string {
	digits := strings.Builder{}
	for _, r := range strings.TrimSpace(raw) {
		if r >= '0' && r <= '9' {
			digits.WriteRune(r)
		}
	}
	out := digits.String()
	if len(out) == 13 && strings.HasPrefix(out, "86") {
		out = out[2:]
	}
	return out
}
