package loomy

import (
	"crypto/hmac"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Account-service request signing.
//
// The 讯飞 account service only answers requests that carry a signature built
// the way the official Electron client builds it (electron/xfyun/sign.js):
//
//	stringToSign = "\n".join([METHOD, PATH, QUERY, Content-MD5, Content-Type,
//	                          Date, Nonce, SignedHeaders, CanonicalizedHeaders])
//	signature    = base64(hmac_sha1(accessKeySecret, stringToSign))
//	Authorization = "account <accessKeyID>:<signature>"
//
// With no custom x-* headers the last two lines are empty strings, but the
// nine-line shape is fixed (the string ends with "\n\n"): dropping them yields a
// signature the service rejects without saying why.

// signerEscape percent-encodes everything outside A-Za-z0-9-_.~ as uppercase
// %XX over UTF-8 bytes, matching Python's quote(s, safe="-_.~").
func signerEscape(value string) string {
	const upperHex = "0123456789ABCDEF"
	var out strings.Builder
	out.Grow(len(value))
	for i := 0; i < len(value); i++ {
		c := value[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == '~':
			out.WriteByte(c)
		default:
			out.WriteByte('%')
			out.WriteByte(upperHex[c>>4])
			out.WriteByte(upperHex[c&0x0f])
		}
	}
	return out.String()
}

// contentMD5 is base64(md5(body)); an empty body signs as an empty string.
func contentMD5(body string) string {
	if body == "" {
		return ""
	}
	sum := md5.Sum([]byte(body))
	return base64.StdEncoding.EncodeToString(sum[:])
}

// signerEscapedPath adds a leading slash, drops a trailing slash, and escapes
// each segment.
func signerEscapedPath(path string) string {
	p := path
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	if len(p) > 1 && strings.HasSuffix(p, "/") {
		p = p[:len(p)-1]
	}
	segments := strings.Split(p, "/")
	for i, segment := range segments {
		if segment != "" {
			segments[i] = signerEscape(segment)
		}
	}
	return strings.Join(segments, "/")
}

// signerEscapedQuery renders the query line. Keys are sorted: Go map iteration
// is unordered and a signature has to be reproducible.
func signerEscapedQuery(params map[string]string) string {
	if len(params) == 0 {
		return ""
	}
	keys := make([]string, 0, len(params))
	for key := range params {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, signerEscape(key)+"="+signerEscape(params[key]))
	}
	return strings.Join(parts, "&")
}

// signerCanonicalized picks the x-* headers and returns (SignedHeaders,
// CanonicalizedHeaders). Both are empty strings when there are none.
func signerCanonicalized(headers map[string]string, prefix string) (string, string) {
	if len(headers) == 0 {
		return "", ""
	}
	rawKeys := make([]string, 0, len(headers))
	for key := range headers {
		rawKeys = append(rawKeys, key)
	}
	sort.Strings(rawKeys)

	picked := make(map[string]string, len(headers))
	for _, key := range rawKeys {
		lower := strings.ToLower(key)
		if strings.HasPrefix(lower, prefix) {
			picked[lower] = strings.TrimSpace(headers[key])
		}
	}
	if len(picked) == 0 {
		return "", ""
	}

	keys := make([]string, 0, len(picked))
	for key := range picked {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	var canonical strings.Builder
	for _, key := range keys {
		canonical.WriteString(key)
		canonical.WriteByte(':')
		canonical.WriteString(picked[key])
		canonical.WriteByte('\n')
	}
	return strings.Join(keys, ";"), canonical.String()
}

// stringToSign builds the nine-line payload.
func stringToSign(method, path string, query map[string]string, body, contentType, date, nonce string, customHeaders map[string]string) string {
	signedHeaders, canonical := signerCanonicalized(customHeaders, "x-")
	return strings.Join([]string{
		strings.ToUpper(method),
		signerEscapedPath(path),
		signerEscapedQuery(query),
		contentMD5(body),
		contentType,
		date,
		nonce,
		signedHeaders,
		strings.TrimRight(canonical, "\n"),
	}, "\n")
}

// sign computes base64(hmac_sha1(secret, stringToSign)).
func sign(secret, method, path string, query map[string]string, body, contentType, date, nonce string, customHeaders map[string]string) string {
	mac := hmac.New(sha1.New, []byte(secret))
	mac.Write([]byte(stringToSign(method, path, query, body, contentType, date, nonce, customHeaders)))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// randomUUID returns a lowercase v4 UUID.
func randomUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand cannot fail on a healthy host; fall back to a
		// timestamp-derived value rather than panicking mid-request.
		return fmt.Sprintf("00000000-0000-4000-8000-%012x", time.Now().UnixNano()&0xffffffffffff)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// randomHex is uuid4().hex[:n]: the value the client puts in traceparent and in
// a minted device id.
func randomHex(n int) string {
	raw := strings.ReplaceAll(randomUUID(), "-", "")
	if n < 0 {
		n = 0
	}
	if n > len(raw) {
		n = len(raw)
	}
	return raw[:n]
}

// accountHeaders builds the signed header set for one account-service call.
// `Date` is UTC in RFC1123-with-GMT and `Nonce` is a fresh UUID, exactly as the
// client sends them.
func accountHeaders(method, path, body string, query map[string]string, contentType string, customHeaders map[string]string) map[string]string {
	if contentType == "" {
		contentType = "application/json"
	}
	date := time.Now().UTC().Format("Mon, 02 Jan 2006 15:04:05 GMT")
	nonce := randomUUID()
	signature := sign(accessKeySecret, method, path, query, body, contentType, date, nonce, customHeaders)

	headers := map[string]string{
		"Authorization": "account " + accessKeyID + ":" + signature,
		"Date":          date,
		"Nonce":         nonce,
		"Content-Type":  contentType,
	}
	if digest := contentMD5(body); digest != "" {
		headers["Content-MD5"] = digest
	}
	for key, value := range customHeaders {
		headers[key] = value
	}
	return headers
}
