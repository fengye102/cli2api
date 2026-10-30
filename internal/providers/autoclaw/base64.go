package autoclaw

import "encoding/base64"

// base64RawURLDecode decodes a JWT segment, tolerating both padded and
// unpadded input (JWT uses unpadded base64url).
func base64RawURLDecode(value string) ([]byte, error) {
	if raw, err := base64.RawURLEncoding.DecodeString(value); err == nil {
		return raw, nil
	}
	return base64.URLEncoding.DecodeString(value)
}
