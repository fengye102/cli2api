package zcode

import (
	"crypto/rand"
	"crypto/sha1"
	"fmt"
	"os"
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
// these three: sending x-query-id / x-session-id on it — the coding-plan shape —
// is itself a 3012 trigger.
func traceHeaders() map[string]string {
	return map[string]string{
		"x-request-id":         randomUUID(),
		"x-zcode-session-type": "main",
		"x-zcode-trace-id":     randomUUID(),
	}
}

