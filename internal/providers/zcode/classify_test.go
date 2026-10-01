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
