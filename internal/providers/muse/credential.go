package muse

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Credential is one logged-in muse.ai web session. muse.ai has no key to mint:
// the browser cookies ARE the credential, which is why this provider imports
// them instead of running an OAuth round.
type Credential struct {
	Format  string            `json:"format"`
	Cookies map[string]string `json:"cookies"`
	// Email / UID are filled from /api/session after the first successful
	// probe so the console can label the account.
	Email   string    `json:"email,omitempty"`
	UID     string    `json:"uid,omitempty"`
	SavedAt time.Time `json:"saved_at,omitempty"`
}

// Ready reports whether the credential carries the cookies a session needs.
func (c Credential) Ready() bool {
	for _, name := range requiredCookies {
		if strings.TrimSpace(c.Cookies[name]) == "" {
			return false
		}
	}
	return true
}

// MissingCookies lists required and recommended cookies that are absent.
func (c Credential) MissingCookies() []string {
	var missing []string
	for _, name := range append(append([]string{}, requiredCookies...), recommendedCookies...) {
		if strings.TrimSpace(c.Cookies[name]) == "" {
			missing = append(missing, name)
		}
	}
	return missing
}

// CookieHeader renders the Cookie request header in a stable order so two
// runs of the same credential produce byte-identical requests.
func (c Credential) CookieHeader() string {
	names := make([]string, 0, len(c.Cookies))
	for name, value := range c.Cookies {
		if strings.TrimSpace(value) == "" {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, name := range names {
		parts = append(parts, name+"="+c.Cookies[name])
	}
	return strings.Join(parts, "; ")
}

// Encode serialises the credential for storage.
func (c Credential) Encode() ([]byte, error) {
	if c.Format == "" {
		c.Format = CredentialFormat
	}
	return json.Marshal(c)
}

// EncodeForImport re-encodes a parsed credential for the account store.
func (c Credential) EncodeForImport() ([]byte, error) { return c.Encode() }

// DecodeCredential reads a stored payload.
func DecodeCredential(payload []byte) (Credential, error) {
	var c Credential
	if err := json.Unmarshal(payload, &c); err != nil {
		return Credential{}, fmt.Errorf("muse: credential is not valid json: %w", err)
	}
	if c.Cookies == nil {
		c.Cookies = map[string]string{}
	}
	if c.Format == "" {
		c.Format = CredentialFormat
	}
	return c, nil
}

// ValidateCredential is the store's gate before an account is created.
func ValidateCredential(payload []byte) error {
	credential, err := ParseImport(payload)
	if err != nil {
		return err
	}
	return RequireSessionCookies(credential)
}

// RequireSessionCookies rejects a credential that cannot hold a session.
func RequireSessionCookies(credential Credential) error {
	var missing []string
	for _, name := range requiredCookies {
		if strings.TrimSpace(credential.Cookies[name]) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("muse: cookie %s is missing; export the cookies of a logged-in muse.ai tab (the %q cookie is HttpOnly, so document.cookie cannot see it)",
			strings.Join(missing, ", "), missing[0])
	}
	return nil
}

// ParseImport accepts every shape an operator can realistically paste and
// normalises it to a Credential. Supported inputs:
//
//  1. our own muse-cookie-v1 JSON
//  2. EditThisCookie / Cookie-Editor array: [{"name":"hatch_sess","value":"…"}]
//  3. a flat or nested object: {"cookies":{"hatch_sess":"…"}} / {"hatch_sess":"…"}
//  4. a raw Cookie header: hatch_sess=…; hatch_gw=…
//  5. a "Copy as cURL" command, whose -b/--cookie or Cookie header is read
//  6. a Netscape cookies.txt export
func ParseImport(raw []byte) (Credential, error) {
	text := strings.TrimSpace(string(raw))
	if text == "" {
		return Credential{}, fmt.Errorf("muse: empty credential payload")
	}
	lower := strings.ToLower(text)
	switch {
	case strings.HasPrefix(lower, "curl "):
		return parseCurl(text)
	case strings.HasPrefix(text, "["):
		return parseCookieArray([]byte(text))
	case strings.HasPrefix(text, "{"):
		return parseCookieObject([]byte(text))
	case looksLikeNetscape(text):
		return parseNetscape(text)
	default:
		cookies := parseCookieHeader(text)
		if len(cookies) == 0 {
			return Credential{}, fmt.Errorf("muse: could not find any cookies in the pasted payload; paste an EditThisCookie JSON export, a Cookie request header, or a Copy-as-cURL command")
		}
		return newCredential(cookies, "", ""), nil
	}
}

// isMuseCookieName reports whether a cookie belongs to a muse.ai session.
func isMuseCookieName(name string) bool {
	lower := strings.ToLower(strings.TrimSpace(name))
	if strings.HasPrefix(lower, "hatch_") {
		return true
	}
	for _, known := range append(append(append([]string{}, requiredCookies...), recommendedCookies...), optionalCookies...) {
		if lower == known {
			return true
		}
	}
	return false
}

func newCredential(cookies map[string]string, email, uid string) Credential {
	cleaned := make(map[string]string, len(cookies))
	for name, value := range cookies {
		name = strings.TrimSpace(name)
		value = strings.TrimSpace(value)
		if name == "" || value == "" {
			continue
		}
		cleaned[name] = value
	}
	return Credential{
		Format:  CredentialFormat,
		Cookies: cleaned,
		Email:   strings.TrimSpace(email),
		UID:     strings.TrimSpace(uid),
		SavedAt: time.Now().UTC(),
	}
}

// parseCookieObject handles both our own payload and loose JSON objects.
func parseCookieObject(raw []byte) (Credential, error) {
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return Credential{}, fmt.Errorf("muse: credential json is malformed: %w", err)
	}
	cookies := map[string]string{}
	var email, uid string
	for key, value := range generic {
		switch strings.ToLower(key) {
		case "cookies":
			switch typed := value.(type) {
			case map[string]any:
				for name, v := range typed {
					if s, ok := v.(string); ok {
						cookies[name] = s
					}
				}
			case []any:
				for _, item := range typed {
					entry, ok := item.(map[string]any)
					if !ok {
						continue
					}
					name, _ := entry["name"].(string)
					val, _ := entry["value"].(string)
					if name != "" {
						cookies[name] = val
					}
				}
			case string:
				for name, v := range parseCookieHeader(typed) {
					cookies[name] = v
				}
			}
		case "email":
			email, _ = value.(string)
		case "uid", "user_id", "userid":
			uid, _ = value.(string)
		case "format", "saved_at", "savedat":
			// metadata, not a cookie
		default:
			// A flat object is accepted, but only for names that are actually
			// muse session cookies: a stray {"foo":"bar"} must not import as a
			// credential, and unrelated cookies must not be replayed upstream.
			if s, ok := value.(string); ok && isMuseCookieName(key) {
				cookies[key] = s
			}
		}
	}
	if len(cookies) == 0 {
		return Credential{}, fmt.Errorf("muse: credential json contains no cookies")
	}
	return newCredential(cookies, email, uid), nil
}

// parseCookieArray handles EditThisCookie / Cookie-Editor exports.
func parseCookieArray(raw []byte) (Credential, error) {
	var entries []map[string]any
	if err := json.Unmarshal(raw, &entries); err != nil {
		return Credential{}, fmt.Errorf("muse: cookie array is malformed: %w", err)
	}
	cookies := map[string]string{}
	for _, entry := range entries {
		name, _ := entry["name"].(string)
		value, _ := entry["value"].(string)
		if strings.TrimSpace(name) == "" {
			continue
		}
		// Respect the export's own domain when present: a browser export of
		// "all cookies" mixes muse.ai with other sites.
		if domain, ok := entry["domain"].(string); ok && domain != "" {
			if !strings.Contains(strings.ToLower(domain), "muse.ai") {
				continue
			}
		}
		cookies[name] = value
	}
	if len(cookies) == 0 {
		return Credential{}, fmt.Errorf("muse: the cookie export has no muse.ai cookies (check that you exported while on a muse.ai tab)")
	}
	return newCredential(cookies, "", ""), nil
}

// parseCookieHeader reads "a=1; b=2".
func parseCookieHeader(text string) map[string]string {
	text = strings.TrimSpace(text)
	text = strings.TrimPrefix(strings.TrimPrefix(text, "Cookie:"), "cookie:")
	text = strings.TrimSpace(text)
	cookies := map[string]string{}
	for _, part := range strings.Split(text, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, value, found := strings.Cut(part, "=")
		if !found {
			continue
		}
		name = strings.TrimSpace(name)
		if name == "" || strings.ContainsAny(name, " \t'\"") {
			continue
		}
		cookies[name] = strings.TrimSpace(value)
	}
	return cookies
}

// parseCurl extracts the Cookie header from a "Copy as cURL" command.
func parseCurl(text string) (Credential, error) {
	text = strings.ReplaceAll(text, "\\\r\n", " ")
	text = strings.ReplaceAll(text, "\\\n", " ")
	args := splitCurlArgs(text)
	cookies := map[string]string{}
	var collected []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "-b" || arg == "--cookie":
			if i+1 < len(args) {
				collected = append(collected, args[i+1])
				i++
			}
		case strings.HasPrefix(arg, "--cookie="):
			collected = append(collected, strings.TrimPrefix(arg, "--cookie="))
		case arg == "-H" || arg == "--header":
			if i+1 < len(args) {
				if header, ok := cutCookieHeader(args[i+1]); ok {
					collected = append(collected, header)
				}
				i++
			}
		case strings.HasPrefix(arg, "-H"):
			if header, ok := cutCookieHeader(strings.TrimPrefix(arg, "-H")); ok {
				collected = append(collected, header)
			}
		}
	}
	for _, raw := range collected {
		for name, value := range parseCookieHeader(raw) {
			cookies[name] = value
		}
	}
	if len(cookies) == 0 {
		return Credential{}, fmt.Errorf("muse: the cURL command carries no Cookie header; copy a muse.ai API request, not the page document")
	}
	return newCredential(cookies, "", ""), nil
}

func cutCookieHeader(header string) (string, bool) {
	name, value, found := strings.Cut(header, ":")
	if !found {
		return "", false
	}
	if !strings.EqualFold(strings.TrimSpace(name), "cookie") {
		return "", false
	}
	return strings.TrimSpace(value), true
}

// splitCurlArgs splits a shell command on whitespace while keeping quoted
// runs intact. It is deliberately small: cURL commands we accept are single
// line and use either single or double quotes.
func splitCurlArgs(text string) []string {
	var args []string
	var current strings.Builder
	var quote rune
	flush := func() {
		if current.Len() > 0 {
			args = append(args, current.String())
			current.Reset()
		}
	}
	for _, r := range text {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
				continue
			}
			current.WriteRune(r)
		case r == '\'' || r == '"':
			quote = r
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			flush()
		default:
			current.WriteRune(r)
		}
	}
	flush()
	return args
}

func looksLikeNetscape(text string) bool {
	if strings.HasPrefix(text, "# Netscape HTTP Cookie File") {
		return true
	}
	lines := strings.Split(text, "\n")
	if len(lines) < 2 {
		return false
	}
	fields := strings.Split(strings.TrimRight(lines[0], "\r"), "\t")
	return len(fields) >= 7 && strings.Contains(fields[0], ".")
}

// parseNetscape reads a cookies.txt export (browser extension or curl -c).
func parseNetscape(text string) (Credential, error) {
	cookies := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) < 7 {
			continue
		}
		domain := strings.ToLower(fields[0])
		if !strings.Contains(domain, "muse.ai") {
			continue
		}
		name := strings.TrimSpace(fields[5])
		value := strings.TrimSpace(fields[6])
		if name == "" {
			continue
		}
		cookies[name] = value
	}
	if len(cookies) == 0 {
		return Credential{}, fmt.Errorf("muse: the cookies.txt export has no muse.ai entries")
	}
	return newCredential(cookies, "", ""), nil
}
