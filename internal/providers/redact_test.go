package providers

import (
	"strings"
	"testing"
)

func TestRedactSecretsMasksEchoedCredentials(t *testing.T) {
	cases := []struct {
		name string
		in   string
		// mustGone are substrings that must not survive redaction.
		mustGone []string
		// mustStay are substrings the reader still needs.
		mustStay []string
	}{
		{
			name:     "authorization header echo",
			in:       `{"error":"unauthorized: Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.abcdefghij sent"}`,
			mustGone: []string{"eyJhbGciOiJIUzI1NiJ9", "abcdefghij"},
			mustStay: []string{"unauthorized"},
		},
		{
			name:     "openai style key",
			in:       `Incorrect API key provided: sk-abcdefghijklmnopqrstuvwx. Check https://help`,
			mustGone: []string{"sk-abcdefghijklmnopqrstuvwx"},
			mustStay: []string{"Incorrect API key provided", "https://help"},
		},
		{
			name:     "json api_key field",
			in:       `{"error":{"code":"1004","message":"bad key","api_key":"AKIDexample0123456789"}}`,
			mustGone: []string{"AKIDexample0123456789"},
			mustStay: []string{"1004", "bad key"},
		},
		{
			name:     "huawei dpop pair",
			in:       `{"error":"signature rejected: Access=QWEASD123&Signature=9f8e7d6c5b4a"}`,
			mustGone: []string{"QWEASD123", "9f8e7d6c5b4a"},
			mustStay: []string{"signature rejected"},
		},
		{
			name:     "x-auth-token header",
			in:       "upstream said x-auth-token: 1a2b3c4d5e6f7g8h rejected",
			mustGone: []string{"1a2b3c4d5e6f7g8h"},
			mustStay: []string{"upstream said", "rejected"},
		},
		{
			name:     "bare jwt",
			in:       `token eyJhbGciOiJub25lIn0.eyJzdWIiOiJhZG1pbiJ9.c2lnbmF0dXJl is expired`,
			mustGone: []string{"eyJhbGciOiJub25lIn0"},
			mustStay: []string{"is expired"},
		},
		{
			name:     "refresh token json",
			in:       `{"error":"refresh_token=\"1//0gAbCdEfGhIjKl\" is revoked"}`,
			mustGone: []string{"1//0gAbCdEfGhIjKl"},
			mustStay: []string{"is revoked"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RedactSecrets(tc.in)
			for _, secret := range tc.mustGone {
				if strings.Contains(got, secret) {
					t.Fatalf("secret survived: %q -> %q", tc.in, got)
				}
			}
			for _, keep := range tc.mustStay {
				if !strings.Contains(got, keep) {
					t.Fatalf("reader lost context %q: %q", keep, got)
				}
			}
			if !strings.Contains(got, "[redacted]") {
				t.Fatalf("no redaction marker: %q", got)
			}
		})
	}
}

func TestRedactSecretsLeavesOrdinaryErrorsAlone(t *testing.T) {
	// An operator reads these to decide what to fix; rewriting them would be a
	// regression in observability.
	safe := []string{
		`{"error":"model not found"}`,
		`{"code":1005,"message":"exceed quota limit"}`,
		`rate limit exceeded, retry after 3s`,
		`invalid request body: messages[0].role is required`,
		`all accounts are cooling down`,
		`{"error":"invalid api key"}`,
		`max_tokens=4096 is greater than the model limit`,
		`context length 200000 exceeded`,
	}
	for _, text := range safe {
		if got := RedactSecrets(text); got != text {
			t.Fatalf("over-redacted %q -> %q", text, got)
		}
	}
}

func TestRedactSecretsEmpty(t *testing.T) {
	if got := RedactSecrets(""); got != "" {
		t.Fatalf("empty = %q", got)
	}
}
