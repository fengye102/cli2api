package zcode

import (
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// Client-identity constants mirrored from the official ZCode desktop build.
// The plan gateway fingerprints the companion header set: a request missing
// part of it, or mixing desktop values with server-shaped ones, escalates to
// the 3012 "unusual activity" block long before the captcha is even checked.
const (
	identityTitle          = "Z Code@electron"
	identityReleaseChannel = "stable"
	identityClientLanguage = "zh-CN"
	identityClientTimezone = "Asia/Shanghai"
	identityPlatform       = "darwin-arm64"
	identityOSCategory     = "macos"
	identityOSVersion      = "25.5.0"

	// CaptchaHeader carries the Aliyun traceless-verification token the plan
	// channel requires on every OAuth chat request.
	CaptchaHeader = "X-Aliyun-Captcha-Verify-Param"
	// CaptchaRegionHeader pairs with it: a token solved in one region is
	// rejected when the header names another.
	CaptchaRegionHeader = "X-Aliyun-Captcha-Verify-Region"
)

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

// deviceMid returns the stable per-account X-Device-Mid. The upstream treats a
// device id that changes per request as a fingerprinting signal, so it is
// derived deterministically from the account id (one account = one device)
// unless ZCODE_DEVICE_MID pins an explicit value.
func deviceMid(accountID string) string {
	if v := strings.TrimSpace(os.Getenv("ZCODE_DEVICE_MID")); v != "" {
		return v
	}
	if strings.TrimSpace(accountID) == "" {
		return ""
	}
	sum := sha1.Sum([]byte("cli2api/zcode/device-mid/" + accountID))
	b := sum[:16]
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// uuidShape matches a bare UUID, the form x-session-id and
// metadata.user_id.session_id take.
var uuidShape = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// seededUUID is a v4-shaped UUID made from a seed: the same seed gives the same
// UUID on every process, so a conversation keeps one session id without any
// state to persist.
func seededUUID(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	h := hex.EncodeToString(sum[:])
	// Follow the client's rendering: the version nibble is 4 and the variant
	// nibble carries 0b10, so the value is indistinguishable from a random v4.
	variant := (hexNibble(h[16])&0x3 | 0x8)
	return h[0:8] + "-" + h[8:12] + "-4" + h[13:16] + "-" +
		fmt.Sprintf("%x", variant) + h[17:20] + "-" + h[20:32]
}

func hexNibble(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	}
	return 0
}

// sessionOf renders the conversation id ZCode's client names on every
// Anthropic-format request: metadata.user_id.session_id, and x-session-id on
// the coding-plan endpoints. It is a bare UUID, the same on every turn of a
// conversation, and the vendor's prompt cache follows it (magpie plugins#387:
// BigModel's plan endpoint answered Pi's sessionless requests with a cache hit
// 1 time in 19). A caller-supplied conversation that already is a UUID goes as
// it is; anything else the gateway names (its own hex session key, an agent's
// session, a content fingerprint) is hashed into one, so the vendor only ever
// sees ZCode's format. With no conversation at all the device stands in, which
// at least keeps one process's turns together.
func sessionOf(hint, device string) string {
	hint = strings.TrimSpace(hint)
	if uuidShape.MatchString(hint) {
		return strings.ToLower(hint)
	}
	if hint != "" {
		return seededUUID("zcode-session\x00" + hint)
	}
	device = strings.TrimSpace(device)
	if device == "" {
		return ""
	}
	return seededUUID("zcode-session-device\x00" + device)
}

// randomUUID returns a v4 UUID for the per-request trace headers.
func randomUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// identityHeaders is the companion header set every official client request
// carries. Values may be overridden per deployment through ZCODE_IDENTITY_* so
// a Windows-shaped account can be simulated without a rebuild.
func identityHeaders(accountID string) map[string]string {
	platform := envOr("ZCODE_IDENTITY_PLATFORM", identityPlatform)
	headers := map[string]string{
		"User-Agent":          userAgent(),
		"HTTP-Referer":        "https://zcode.z.ai/",
		"X-Title":             identityTitle,
		"X-ZCode-App-Version": Version,
		"X-ZCode-Agent":       "glm",
		"X-Platform":          platform,
		"X-Release-Channel":   envOr("ZCODE_IDENTITY_RELEASE_CHANNEL", identityReleaseChannel),
		"X-Client-Language":   envOr("ZCODE_IDENTITY_CLIENT_LANGUAGE", identityClientLanguage),
		"X-Client-Timezone":   envOr("ZCODE_IDENTITY_CLIENT_TIMEZONE", identityClientTimezone),
		"X-Os-Category":       osCategory(platform),
		"X-Os-Version":        envOr("ZCODE_IDENTITY_OS_VERSION", identityOSVersion),
	}
	if mid := deviceMid(accountID); mid != "" {
		headers["X-Device-Mid"] = mid
	}
	return headers
}

func osCategory(platform string) string {
	switch {
	case strings.HasPrefix(platform, "darwin"), strings.HasPrefix(platform, "macos"):
		return "macos"
	case strings.HasPrefix(platform, "win32"), strings.HasPrefix(platform, "windows"):
		return "windows"
	default:
		return "linux"
	}
}

// traceHeaders are regenerated per request. The plan (JWT) channel takes only
// these: sending x-query-id / x-session-id on it — the coding-plan shape — is
// itself a 3012 trigger. x-zcode-query-source names the client's turn kind and
// is what the desktop sends on a normal chat turn.
func traceHeaders() map[string]string {
	return map[string]string{
		"x-request-id":         randomUUID(),
		"x-zcode-session-type": "main",
		"x-zcode-trace-id":     randomUUID(),
		"x-zcode-query-source": "main_turn",
	}
}
