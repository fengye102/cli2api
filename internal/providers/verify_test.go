package providers

import (
	"errors"
	"fmt"
	"testing"
)

// TestMessageNeedsHumanVerification pins the phrases that mean "a person has to
// clear this", and — just as important — the ones that only look similar.
func TestMessageNeedsHumanVerification(t *testing.T) {
	cases := []struct {
		message string
		want    bool
		why     string
	}{
		{"zcode claim captcha failed: captcha verify failed: 需人工验证", true, "3007 captcha"},
		{"上游返回 need CAPTCHA token", true, "captcha, any case"},
		{"领取失败：需要人机验证", true, "chinese captcha"},
		{"投递需人工验证后重试", true, "explicit ask for a human"},
		{"滑块未通过", true, "slider"},
		{"风控拦截", true, "risk block"},

		// Not verification: these are ordinary transient refusals and must keep
		// riding the retry ladder.
		{"当前参与用户太多，请稍后再试（9074）", false, "busy upstream"},
		{"今日未见积分入账（发放触发未生效，稍后重试）", false, "raccoon grant not landed yet"},
		{"凭证校验失败", false, "校验 is not 验证"},
		{"", false, "empty"},
	}
	for _, tc := range cases {
		if got := MessageNeedsHumanVerification(tc.message); got != tc.want {
			t.Errorf("MessageNeedsHumanVerification(%q) = %v, want %v (%s)", tc.message, got, tc.want, tc.why)
		}
	}
}

// TestNeedsHumanVerificationReadsTheChain keeps the sentinel authoritative even
// when the wrapper text says nothing recognisable.
func TestNeedsHumanVerificationReadsTheChain(t *testing.T) {
	wrapped := fmt.Errorf("zcode claim failed: %w", ErrNeedsHumanVerification)
	if !NeedsHumanVerification(wrapped) {
		t.Fatal("a wrapped sentinel must classify as needing a human")
	}
	if NeedsHumanVerification(nil) {
		t.Fatal("nil is not a verification failure")
	}
	if NeedsHumanVerification(errors.New("当前参与用户太多，请稍后再试")) {
		t.Fatal("a busy answer must stay retryable")
	}
}
