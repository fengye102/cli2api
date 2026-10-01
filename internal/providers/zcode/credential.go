// Package zcode implements the ZCode (Z.ai / BigModel) in-process provider.
// Protocol constants live only in this package.
package zcode

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/caigee-cmd/cli2api/internal/providers"
)

const (
	// CredentialFormat is the canonical storage format for provider=zcode.
	CredentialFormat = "zcode-credential-v1"

	AuthModeAPIKey = "api_key"
	AuthModeOAuth  = "oauth"

	RegionZAI      = "zai"
	RegionBigModel = "bigmodel"

	// encV1Prefix marks credentials.json values sealed with the desktop
	// client's machine-bound AES-256-GCM key. They cannot be decrypted
	// here; imports reject them with an actionable message.
	encV1Prefix = "enc:v1:"
)

// Credential is the canonical storage payload for provider=zcode. API-key
// mode carries a plain key (a BigModel key from bigmodel.cn, or a Z.ai
// "id.secret" pair from z.ai) and is sent as x-api-key. OAuth mode carries the
// ZCode JWT (zcodejwttoken, 3 segments) plus the provider access/refresh tokens
// from the plan gateway. Provider names the region the credential belongs to:
// the two ZCode services have different authorize hosts, redirect schemes and
// API bases, so a credential is only ever sent to its own.
type Credential struct {
	Format        string `json:"format"`
	AuthMode      string `json:"auth_mode"`
	Provider      string `json:"provider"` // zai | bigmodel
	APIKey        string `json:"api_key,omitempty"`
	ZCodeJWT      string `json:"zcode_jwt_token,omitempty"`
	AccessToken   string `json:"access_token,omitempty"`
	RefreshToken  string `json:"refresh_token,omitempty"`
	ExpiresAt     int64  `json:"expires_at,omitempty"`
	RefreshExpiry int64  `json:"refresh_expires_at,omitempty"`
	Email         string `json:"email,omitempty"`
	UserID        string `json:"user_id,omitempty"`
	Plan          string `json:"plan,omitempty"`
	BaseURL       string `json:"base_url,omitempty"`
}

// IsOAuth reports whether the credential authenticates via the ZCode JWT.
func (c Credential) IsOAuth() bool {
	return strings.EqualFold(strings.TrimSpace(c.AuthMode), AuthModeOAuth)
}

// isSealed reports whether a token value is machine-bound and not usable.
func isSealed(value string) bool {
	return strings.HasPrefix(value, encV1Prefix)
}

// hasUsableCredential reports whether the credential can serve chat: a
// plaintext API key, or a plaintext ZCode JWT in oauth mode.
func (c Credential) hasUsableCredential() bool {
	if c.IsOAuth() {
		jwt := strings.TrimSpace(c.ZCodeJWT)
		return jwt != "" && !isSealed(jwt)
	}
	key := strings.TrimSpace(c.APIKey)
	return key != "" && !isSealed(key)
}

// IsJWTToken reports whether value is a 3-segment JWT, the shape ZCode
// issues as zcodejwttoken.
func IsJWTToken(value string) bool {
	return strings.Count(strings.TrimSpace(value), ".") == 2
}

// jwtPayload reads identity fields out of a JWT payload segment. The
// signature is not verified: zcodejwttoken is only ever a local cache of
// what the ZCode gateway issued, and import only needs user_id/email.
func jwtPayload(s string) map[string]any {
	parts := strings.Split(strings.TrimSpace(s), ".")
	if len(parts) < 2 {
		return nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		// Some clients pad the segments; tolerate that too.
		raw, err = base64.URLEncoding.DecodeString(parts[1])
		if err != nil {
			return nil
		}
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil
	}
	return payload
}

// fillJWTIdentity caches user_id / email / provider from the JWT payload
// when present.
func fillJWTIdentity(c *Credential, token string) {
	payload := jwtPayload(token)
	if payload == nil {
		return
	}
	if c.UserID == "" {
		c.UserID = stringOrFloat(payload["user_id"])
	}
	if c.Email == "" {
		if email, ok := payload["email"].(string); ok {
			c.Email = email
		}
	}
	if c.Provider == "" {
		if provider, ok := payload["provider"].(string); ok {
			normalized := normalizeRegion(provider)
			if normalized == RegionZAI || normalized == RegionBigModel {
				c.Provider = normalized
			}
		}
	}
}

func stringOrFloat(v any) string {
	switch n := v.(type) {
	case string:
		return n
	case float64:
		return fmt.Sprintf("%.0f", n)
	default:
		return ""
	}
}

// DecodeCredential accepts, in order: (1) our own bundle, (2) a ZCode
// config.json / credentials.json document walked with the shared jswalk
// helpers, (3) a bare API key / JWT string. Documents whose credential
// values are enc:v1: sealed fail with the actionable message from spec
// §1.6; plaintext values in the same document are accepted and sealed
// ones skipped.
func DecodeCredential(payload []byte) (Credential, error) {
	trimmed := strings.TrimSpace(string(payload))
	if isSealed(trimmed) {
		return Credential{}, sealedCredentialError("payload")
	}
	if trimmed != "" {
		if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
			// Account-manager exports (for example cockpit-tools) wrap the
			// account in a JSON array. Unwrap it before the flat-bundle
			// attempt so a single pasted export imports like a bundle.
			if strings.HasPrefix(trimmed, "[") {
				if inner, ok := unwrapCredentialArray(payload); ok {
					payload = inner
					trimmed = strings.TrimSpace(string(inner))
				}
			}
			// Our own bundle first: a flat document that already carries a
			// usable credential is stored verbatim.
			var flat Credential
			if err := json.Unmarshal([]byte(trimmed), &flat); err == nil && flat.hasUsableCredential() {
				normalize(&flat)
				flat.Format = CredentialFormat
				return flat, nil
			}
			// Tolerant walk of a ZCode local document (config.json /
			// credentials.json), unwrapping JSON-stringified objects the
			// way the shared helpers do.
			if value, err := providers.UnwrapJSONValue(payload); err == nil {
				credential, sealedKey, ok := credentialFromLocalDoc(value)
				if sealedKey != "" {
					return Credential{}, sealedCredentialError(sealedKey)
				}
				if ok {
					return credential, nil
				}
			}
		} else {
			// Bare string: a pasted API key or a bare JWT (no JSON wrapper).
			credential := credentialFromBareString(trimmed)
			normalize(&credential)
			credential.Format = CredentialFormat
			return credential, nil
		}
	}
	return Credential{}, fmt.Errorf("zcode credential requires an api_key or oauth tokens")
}

// unwrapCredentialArray unwraps a JSON array export into the element that
// carries usable credential material. A single-element array is returned
// as-is; with several elements the first one that decodes into a credential
// with a plaintext api_key or oauth token wins, so one pasted account list
// still imports.
func unwrapCredentialArray(payload []byte) ([]byte, bool) {
	var elements []json.RawMessage
	if err := json.Unmarshal(payload, &elements); err != nil || len(elements) == 0 {
		return nil, false
	}
	if len(elements) == 1 {
		return elements[0], true
	}
	for _, element := range elements {
		var candidate Credential
		if err := json.Unmarshal(element, &candidate); err == nil && candidate.hasUsableCredential() {
			return element, true
		}
	}
	return elements[0], true
}

// credentialFromBareString turns a pasted key or JWT into a credential. A
// 3-segment JWT is the ZCode OAuth token; anything else is a plain key.
func credentialFromBareString(value string) Credential {
	key := strings.TrimSpace(value)
	if IsJWTToken(key) {
		credential := Credential{AuthMode: AuthModeOAuth, ZCodeJWT: key}
		fillJWTIdentity(&credential, key)
		return credential
	}
	return Credential{AuthMode: AuthModeAPIKey, APIKey: key}
}

// credentialFromLocalDoc walks a ZCode config.json / credentials.json
// document and extracts plaintext credential material.
//
//   - config.json provider sections: any options.apiKey plus a sibling
//     baseURL decides region; a 3-segment JWT apiKey means oauth mode.
//   - credentials.json flat map: "oauth:<provider>:access_token" style
//     keys plus zcodejwttoken.
//
// The sealedKey return value is the first credential-bearing key whose
// value is enc:v1: — set only when no plaintext credential was found, so
// the caller can fail with the actionable import message.
func credentialFromLocalDoc(value any) (Credential, string, bool) {
	credential := Credential{Format: CredentialFormat}
	// ZCode config.json provider sections.
	credential.APIKey = pickLocalString(value, "apiKey", "api_key")
	baseURL := pickLocalString(value, "baseURL", "base_url")
	regionFromBaseURL(&credential, baseURL)
	if IsJWTToken(credential.APIKey) {
		credential.AuthMode = AuthModeOAuth
		credential.ZCodeJWT = credential.APIKey
		credential.APIKey = ""
		fillJWTIdentity(&credential, credential.ZCodeJWT)
	} else if credential.APIKey != "" {
		credential.AuthMode = AuthModeAPIKey
	}
	credential.Email = pickLocalString(value, "email")
	credential.UserID = pickLocalString(value, "user_id", "userId")
	credential.Plan = pickLocalString(value, "plan")
	if strings.TrimSpace(baseURL) != "" {
		credential.BaseURL = strings.TrimSpace(baseURL)
	}
	// credentials.json flat map. The shared auth-object walk finds the
	// innermost blob that carries a token field.
	if auth := providers.DeepFindAuthObject(value); auth != nil {
		credential.AccessToken = pickLocalField(auth, "accessToken", "access_token")
		credential.RefreshToken = pickLocalField(auth, "refreshToken", "refresh_token")
	}
	if credential.AccessToken == "" {
		credential.AccessToken = pickLocalString(value, "access_token", "accessToken")
	}
	if credential.RefreshToken == "" {
		credential.RefreshToken = pickLocalString(value, "refresh_token", "refreshToken")
	}
	if credential.ZCodeJWT == "" {
		credential.ZCodeJWT = pickLocalString(value, "zcodejwttoken")
	}
	if credential.ZCodeJWT != "" {
		credential.AuthMode = AuthModeOAuth
		fillJWTIdentity(&credential, credential.ZCodeJWT)
	}
	if credential.Provider == "" {
		credential.Provider = activeProviderFrom(value)
	}
	// Sealable fields: skip sealed picks so they cannot masquerade as
	// plaintext tokens.
	if isSealed(credential.ZCodeJWT) {
		credential.ZCodeJWT = ""
		credential.AuthMode = ""
	}
	if isSealed(credential.APIKey) {
		credential.APIKey = ""
		credential.AuthMode = ""
	}
	if credential.hasUsableCredential() {
		normalize(&credential)
		return credential, "", true
	}
	// No plaintext credential in the document: surface the first sealed
	// credential key so the import fails with an actionable message.
	return Credential{}, findSealedCredentialKey(value), false
}

// findSealedCredentialKey reports the first credential-bearing key whose
// value is enc:v1: sealed. Plain metadata keys are matched too: when the
// whole document's tokens are machine-bound, any of them proves the import
// is impossible and the message points at the plaintext alternative.
func findSealedCredentialKey(v any) string {
	var found string
	walkJSONObjects(v, func(m map[string]any) bool {
		for key, val := range m {
			s, ok := val.(string)
			if !ok || !isSealed(s) {
				continue
			}
			if isCredentialKey(key) {
				found = key
				return false
			}
		}
		return true
	})
	return found
}

// isCredentialKey names the credential-bearing fields in a ZCode local
// document.
func isCredentialKey(key string) bool {
	k := strings.ToLower(key)
	return strings.Contains(k, "zcodejwttoken") ||
		strings.Contains(k, "apikey") ||
		strings.Contains(k, "api_key") ||
		strings.Contains(k, "access_token") ||
		strings.Contains(k, "refresh_token") ||
		strings.Contains(k, "user_info")
}

func walkJSONObjects(v any, fn func(map[string]any) bool) {
	switch t := v.(type) {
	case map[string]any:
		if !fn(t) {
			return
		}
		for _, val := range t {
			walkJSONObjects(val, fn)
		}
	case []any:
		for _, val := range t {
			walkJSONObjects(val, fn)
		}
	}
}

// pickLocalString picks the first usable value among the shared
// DeepPickString names, skipping enc:v1: sealed values: a machine-bound
// token must never masquerade as a plaintext credential.
func pickLocalString(obj any, names ...string) string {
	for _, name := range names {
		s := providers.DeepPickString(obj, name)
		if s != "" && !isSealed(s) {
			return s
		}
	}
	return ""
}

// pickLocalField reads a field from the object itself or one level down
// (auth.accessToken / session patterns), skipping sealed values.
func pickLocalField(obj map[string]any, names ...string) string {
	for _, name := range names {
		if s, ok := obj[name].(string); ok && s != "" && !isSealed(s) {
			return s
		}
	}
	for _, seg := range []string{"auth", "session", "data"} {
		inner, ok := obj[seg].(map[string]any)
		if !ok {
			continue
		}
		for _, name := range names {
			if s, ok := inner[name].(string); ok && s != "" && !isSealed(s) {
				return s
			}
		}
	}
	return ""
}

// activeProviderFrom reads "oauth:active_provider" or a flat "provider"
// field.
func activeProviderFrom(value any) string {
	if s := pickLocalString(value, "oauth:active_provider", "active_provider", "provider"); s != "" {
		return normalizeRegion(s)
	}
	return ""
}

// regionFromBaseURL maps a provider baseURL onto zai | bigmodel.
func regionFromBaseURL(c *Credential, baseURL string) {
	lower := strings.ToLower(strings.TrimSpace(baseURL))
	switch {
	case lower == "":
		return
	case strings.Contains(lower, "bigmodel.cn"):
		c.Provider = RegionBigModel
	case strings.Contains(lower, "z.ai"):
		c.Provider = RegionZAI
	}
}

func normalizeRegion(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "zai", "z.ai":
		return RegionZAI
	case "bigmodel", "bigmodel.cn":
		return RegionBigModel
	default:
		return strings.ToLower(strings.TrimSpace(s))
	}
}

// normalize applies defaults so encoded payloads are canonical. A credential
// that names no region falls back to the Z.ai service, the one the desktop
// client signs in to by default.
func normalize(c *Credential) {
	if c.AuthMode == "" {
		c.AuthMode = AuthModeAPIKey
	}
	if c.Provider == "" {
		c.Provider = RegionZAI
	}
	c.AuthMode = strings.ToLower(strings.TrimSpace(c.AuthMode))
	c.Provider = normalizeRegion(c.Provider)
}

// ensureSupportedRegion rejects credentials that name neither ZCode service.
// The two realms have different authorize hosts, redirect schemes and API
// bases, so an unrecognised region would authenticate nowhere and only surface
// as a confusing upstream 401 later.
func ensureSupportedRegion(c Credential) error {
	switch normalizeRegion(c.Provider) {
	case RegionZAI, RegionBigModel, "":
		return nil
	default:
		return fmt.Errorf("unknown zcode provider %q: use %s (Z.ai) or %s (BigModel)", c.Provider, RegionZAI, RegionBigModel)
	}
}

// sealedCredentialError is the actionable import failure for machine-bound
// credentials.json values: the operator must use the plaintext apiKey from
// config.json or export a fresh bundle from the console.
func sealedCredentialError(key string) error {
	return fmt.Errorf(
		"zcode credential field %q is sealed (enc:v1: AES-256-GCM, machine-bound key) and cannot be imported here. Paste the plaintext apiKey from the ZCode config.json instead, or export a credential bundle from this console",
		key,
	)
}

// Encode serializes the credential in canonical form.
func (c Credential) Encode() ([]byte, error) {
	c.Format = CredentialFormat
	normalize(&c)
	return json.Marshal(c)
}

// ValidateCredential enforces the import contract: a usable plaintext chat
// credential for a known service must exist; sealed enc:v1: payloads and
// unknown regions fail with the actionable message.
func ValidateCredential(payload []byte) error {
	credential, err := DecodeCredential(payload)
	if err != nil {
		return err
	}
	if err := ensureSupportedRegion(credential); err != nil {
		return err
	}
	if !credential.hasUsableCredential() {
		return fmt.Errorf("zcode credential requires an api_key or oauth tokens")
	}
	return nil
}

// Ready reports whether the credential can serve chat.
func (c Credential) Ready() bool {
	return c.hasUsableCredential()
}
