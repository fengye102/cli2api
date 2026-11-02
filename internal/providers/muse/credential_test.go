package muse

import (
	"strings"
	"testing"
)

func TestParseImportShapes(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want map[string]string
	}{
		{
			name: "canonical payload",
			in:   `{"format":"muse-cookie-v1","cookies":{"hatch_sess":"s1","hatch_native_auth_device":"d1"}}`,
			want: map[string]string{"hatch_sess": "s1", "hatch_native_auth_device": "d1"},
		},
		{
			name: "editthiscookie array",
			in:   `[{"domain":".muse.ai","name":"hatch_sess","value":"s2"},{"domain":"muse.ai","name":"hatch_gw","value":"g2"},{"domain":".google.com","name":"NID","value":"nope"}]`,
			want: map[string]string{"hatch_sess": "s2", "hatch_gw": "g2"},
		},
		{
			name: "nested cookies object",
			in:   `{"cookies":{"hatch_sess":"s3","hatch_native_auth_device":"d3"},"email":"a@b.c"}`,
			want: map[string]string{"hatch_sess": "s3", "hatch_native_auth_device": "d3"},
		},
		{
			name: "flat object",
			in:   `{"hatch_sess":"s4"}`,
			want: map[string]string{"hatch_sess": "s4"},
		},
		{
			name: "raw cookie header",
			in:   "Cookie: hatch_sess=s5; hatch_native_auth_device=d5",
			want: map[string]string{"hatch_sess": "s5", "hatch_native_auth_device": "d5"},
		},
		{
			name: "copy as curl with -b",
			in:   `curl 'https://muse.ai/api/session' -b 'hatch_sess=s6; hatch_gw=g6' -H 'accept: */*'`,
			want: map[string]string{"hatch_sess": "s6", "hatch_gw": "g6"},
		},
		{
			name: "copy as curl with cookie header",
			in:   `curl 'https://muse.ai/api/session' -H "Cookie: hatch_sess=s7" -H "accept: application/json"`,
			want: map[string]string{"hatch_sess": "s7"},
		},
		{
			name: "netscape cookies.txt",
			in:   "# Netscape HTTP Cookie File\n.muse.ai\tTRUE\t/\tTRUE\t0\thatch_sess\ts8\n.muse.ai\tTRUE\t/\tFALSE\t0\thatch_native_auth_device\td8\n",
			want: map[string]string{"hatch_sess": "s8", "hatch_native_auth_device": "d8"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			credential, err := ParseImport([]byte(tc.in))
			if err != nil {
				t.Fatalf("ParseImport: %v", err)
			}
			for name, want := range tc.want {
				if got := credential.Cookies[name]; got != want {
					t.Fatalf("cookie %s = %q, want %q", name, got, want)
				}
			}
			if len(credential.Cookies) != len(tc.want) {
				t.Fatalf("parsed %d cookies %v, want exactly %v", len(credential.Cookies), credential.Cookies, tc.want)
			}
			if credential.Format != CredentialFormat {
				t.Fatalf("format = %q, want %q", credential.Format, CredentialFormat)
			}
		})
	}
}

func TestParseImportRejectsJunk(t *testing.T) {
	for _, in := range []string{"", "   ", "hello world", `{"foo":"bar"}`, `[{"domain":".google.com","name":"NID","value":"x"}]`} {
		if _, err := ParseImport([]byte(in)); err == nil {
			t.Fatalf("ParseImport(%q) accepted junk", in)
		}
	}
}

func TestValidateCredentialRequiresSession(t *testing.T) {
	ok := []byte(`{"cookies":{"hatch_sess":"s"}}`)
	if err := ValidateCredential(ok); err != nil {
		t.Fatalf("ValidateCredential(session only) = %v, want nil", err)
	}
	bad := []byte(`{"cookies":{"hatch_gw":"g"}}`)
	err := ValidateCredential(bad)
	if err == nil || !strings.Contains(err.Error(), "hatch_sess") {
		t.Fatalf("ValidateCredential(missing session) = %v, want a message naming hatch_sess", err)
	}
}

func TestMissingCookiesReportsRecommended(t *testing.T) {
	credential, err := ParseImport([]byte(`{"cookies":{"hatch_sess":"s"}}`))
	if err != nil {
		t.Fatalf("ParseImport: %v", err)
	}
	if !credential.Ready() {
		t.Fatal("a session cookie alone must be usable")
	}
	missing := credential.MissingCookies()
	if len(missing) != 1 || missing[0] != "hatch_native_auth_device" {
		t.Fatalf("MissingCookies() = %v, want [hatch_native_auth_device]", missing)
	}
}

func TestCookieHeaderIsStable(t *testing.T) {
	credential, err := ParseImport([]byte(`{"cookies":{"hatch_sess":"s","hatch_gw":"g","hatch_vml":"v"}}`))
	if err != nil {
		t.Fatalf("ParseImport: %v", err)
	}
	const want = "hatch_gw=g; hatch_sess=s; hatch_vml=v"
	for i := 0; i < 20; i++ {
		if got := credential.CookieHeader(); got != want {
			t.Fatalf("CookieHeader() = %q, want %q (must be stable across runs)", got, want)
		}
	}
}

func TestResolveModel(t *testing.T) {
	cases := map[string]string{
		"":                  ModelChat,
		"gpt-4o":            ModelChat,
		"Claude-Sonnet-4":   ModelChat,
		"muse-image":        ModelImage,
		"dall-e-3":          ModelImage,
		"sora":              ModelVideo,
		"something-unknown": ModelChat,
	}
	for in, want := range cases {
		if got := ResolveModel(in); got != want {
			t.Fatalf("ResolveModel(%q) = %q, want %q", in, got, want)
		}
	}
}
