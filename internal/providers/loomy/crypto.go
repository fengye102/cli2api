package loomy

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"strings"
)

// encryptPassword wraps the account password the way the client does:
// RSA/PKCS#1 v1.5 over the DER public key that /login/account/getPuKey hands
// out, base64-encoded, with the `rcode` from the same response sent alongside as
// a server nonce (not a captcha).
func encryptPassword(publicKeyB64, plaintext string) (string, error) {
	pub, err := parseRSAPublicKey(publicKeyB64)
	if err != nil {
		return "", err
	}
	ciphertext, err := rsa.EncryptPKCS1v15(rand.Reader, pub, []byte(plaintext))
	if err != nil {
		return "", fmt.Errorf("loomy password encrypt: %w", err)
	}
	return base64.StdEncoding.EncodeToString(ciphertext), nil
}

// parseRSAPublicKey accepts the two encodings a service might hand back: a
// SubjectPublicKeyInfo (what getPuKey returns today) and a bare PKCS#1
// RSAPublicKey. Anything else is reported as unusable rather than decrypted into
// a login attempt that cannot succeed.
func parseRSAPublicKey(publicKeyB64 string) (*rsa.PublicKey, error) {
	raw := strings.TrimSpace(publicKeyB64)
	raw = strings.TrimSpace(strings.TrimPrefix(raw, "-----BEGIN PUBLIC KEY-----"))
	raw = strings.TrimSpace(strings.TrimSuffix(raw, "-----END PUBLIC KEY-----"))
	raw = strings.Join(strings.Fields(raw), "")

	der, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		if der, err = base64.RawStdEncoding.DecodeString(raw); err != nil {
			return nil, fmt.Errorf("loomy public key is not base64: %w", err)
		}
	}

	if parsed, err := x509.ParsePKIXPublicKey(der); err == nil {
		if key, ok := parsed.(*rsa.PublicKey); ok {
			return key, nil
		}
		return nil, fmt.Errorf("loomy public key is not RSA (%T)", parsed)
	}
	if key, err := x509.ParsePKCS1PublicKey(der); err == nil {
		return key, nil
	}
	return nil, fmt.Errorf("loomy public key: unrecognised DER encoding")
}
