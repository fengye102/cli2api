package zcode

import (
	"strings"
	"testing"

	"github.com/caigee-cmd/cli2api/internal/accounts"
)

func TestClassify_RiskControlAndQuota(t *testing.T) {
	// The plan gateway blocks an OAuth call it distrusts with HTTP 405 and
	// this envelope. It is a temporary upstream block, never an auth failure:
	// classifying it as auth would flip the account to "登录失败".
	risk := Classify(405, `{"code":3012,"msg":"request has been blocked due to unusual activity.","logid":"20261001200744"}`)
	if risk.Kind != accounts.KindRateLimit {
		t.Errorf("risk kind=%q want %q", risk.Kind, accounts.KindRateLimit)
	}
	if !strings.Contains(risk.Message, "unusual activity") {
		t.Errorf("risk message=%q", risk.Message)
	}
	if got := Classify(405, "captcha verify failed"); got.Kind != accounts.KindRateLimit {
		t.Errorf("captcha kind=%q want %q", got.Kind, accounts.KindRateLimit)
	}
	// The pay-as-you-go endpoint reports an unfunded account as
	// rate_limit_error/1113; quota must win over the envelope's own type.
	unfunded := Classify(429, `{"type":"error","error":{"type":"rate_limit_error","code":"1113","message":"[1113][Insufficient balance or no resource package. Please recharge.]"}}`)
	if unfunded.Kind != accounts.KindQuota {
		t.Errorf("unfunded kind=%q want %q (message=%q)", unfunded.Kind, accounts.KindQuota, unfunded.Message)
	}
	// Plain 401 stays auth.
	if got := Classify(401, `{"error":{"message":"token expired or incorrect","type":"401"}}`); got.Kind != accounts.KindAuth {
		t.Errorf("401 kind=%q want %q", got.Kind, accounts.KindAuth)
	}
}

// TestClassify_EdgeBlockPageIsNotAuth pins the P0 fix: Aliyun's edge answers a
// blocked egress with an HTML (or XML) page under 403/405/502. Reading the
// status first flipped a healthy credential to auth ("登录失败") — the operator
// then deleted a working account instead of changing its proxy.
func TestClassify_EdgeBlockPageIsNotAuth(t *testing.T) {
	aliyunPage := "<html><head><title>405</title></head><body>\n" +
		"<h1>Sorry, your request was blocked</h1>\n" +
		"<p>Reference: errors.aliyun.com/?q=abc</p></body></html>"

	for _, status := range []int{403, 405, 502} {
		got := Classify(status, aliyunPage)
		if got.Kind != accounts.KindUnavailable {
			t.Errorf("status=%d kind=%q want %q", status, got.Kind, accounts.KindUnavailable)
		}
		if !strings.Contains(got.Message, "edge blocked this egress") {
			t.Errorf("status=%d message=%q", status, got.Message)
		}
		if strings.Contains(got.Message, "\n") {
			t.Errorf("status=%d message kept raw newlines: %q", status, got.Message)
		}
		if len(got.Message) > 300 {
			t.Errorf("status=%d message too long (%d): %q", status, len(got.Message), got.Message)
		}
	}

	// A WAF interstitial without any visible markup still carries its own
	// markers, and an OSS-style XML 403 is an edge page too.
	if got := Classify(405, "acw_sc__v2 challenge required"); got.Kind != accounts.KindUnavailable {
		t.Errorf("acw kind=%q", got.Kind)
	}
	xmlErr := `<?xml version="1.0"?><Error><Code>AccessDenied</Code></Error>`
	if got := Classify(403, xmlErr); got.Kind != accounts.KindUnavailable {
		t.Errorf("xml kind=%q want %q", got.Kind, accounts.KindUnavailable)
	}

	// JSON refusals keep their own reading: an API 403 is still auth, and a
	// 3012 risk block still takes the captcha path even though it can arrive
	// wrapped in a page-like body.
	if got := Classify(403, `{"error":{"type":"permission_error","message":"no access"}}`); got.Kind != accounts.KindAuth {
		t.Errorf("json 403 kind=%q want %q", got.Kind, accounts.KindAuth)
	}
	if got := Classify(405, `<html>unusual activity, captcha required</html>`); got.Kind != accounts.KindRateLimit {
		t.Errorf("captcha page kind=%q want %q", got.Kind, accounts.KindRateLimit)
	}
}

func TestEdgeSnippetCollapsesMarkup(t *testing.T) {
	got := edgeSnippet("<html>\n  <body>  blocked\n</body></html>")
	if got != "<html> <body> blocked </body></html>" {
		t.Fatalf("snippet=%q", got)
	}
	long := strings.Repeat("x", 400)
	if got := edgeSnippet(long); len([]rune(got)) != 121 {
		t.Fatalf("len=%d want 121", len([]rune(got)))
	}
}
