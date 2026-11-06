package providers

import (
	"errors"
	"strings"
)

// ErrNeedsHumanVerification marks an upstream answer the gateway cannot clear on
// its own: the vendor wants an interactive captcha, slider or risk check before
// it will proceed.
//
// Retrying is pointless — the same request answers the same way until a person
// steps in — so every scheduler that would otherwise re-post on a ladder must
// stop the clock for that account instead of burning a claim call every ten
// minutes until the window shuts. Adapters wrap this sentinel into the error
// they return (see zcode's 3007 branch); the message that lands on the account
// record carries the same words, so a scheduler that only has the recorded
// string can still recognise it (MessageNeedsHumanVerification).
var ErrNeedsHumanVerification = errors.New("需人工验证")

// verificationMarkers are the words upstream uses when it wants a human. They
// are matched case-insensitively, and deliberately narrow: a bare 验证 would
// also catch 凭证校验失败-style messages that are worth retrying, so only the
// phrases that name an interactive check are listed.
var verificationMarkers = []string{
	"需人工验证",
	"人工验证",
	"人机验证",
	"验证码",
	"滑块",
	"风控拦截",
	"captcha",
	"verification required",
	"verify param",
	"risk control",
}

// NeedsHumanVerification reports whether err is (or wraps) a call that needs a
// human to clear it. It works on the error chain first and falls back to the
// text, so adapters that build plain fmt.Errorf messages still classify.
func NeedsHumanVerification(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrNeedsHumanVerification) {
		return true
	}
	return MessageNeedsHumanVerification(err.Error())
}

// MessageNeedsHumanVerification reports whether a recorded message (the string
// the schedulers see on account.LastCheckinMsg, not an error chain) says a
// human is needed.
func MessageNeedsHumanVerification(message string) bool {
	if strings.TrimSpace(message) == "" {
		return false
	}
	lower := strings.ToLower(message)
	for _, marker := range verificationMarkers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}
