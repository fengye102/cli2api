package executor

import (
	"strings"
	"testing"
)

// TestTruncateErrRedactsBeforeCut pins the log/client boundary rule: every
// provider error passes through truncateErr, so a credential an upstream echoed
// back must be gone before the text is stored in request_logs, served by
// /api/logs, or returned to the caller.
func TestTruncateErrRedactsBeforeCut(t *testing.T) {
	msg := `upstream 401: {"error":"invalid token","sent":{"Authorization": "Bearer sk-abcdefghijklmnopqrstuvwx"}}`
	got := truncateErr(msg)
	if strings.Contains(got, "sk-abcdefghijklmnopqrstuvwx") {
		t.Fatalf("credential survived: %q", got)
	}
	if !strings.Contains(got, "upstream 401") {
		t.Fatalf("reader lost the status: %q", got)
	}
	if !strings.Contains(got, "[redacted]") {
		t.Fatalf("no redaction marker: %q", got)
	}
}

// TestTruncateErrStillBoundsLength keeps the original contract: the stored row
// and the response never carry more than 500 bytes, and the cut happens after
// redaction so no half-secret prefix is left behind.
func TestTruncateErrStillBoundsLength(t *testing.T) {
	var b strings.Builder
	b.WriteString("api_key=AKIDexample0123456789 ")
	for b.Len() < 4000 {
		b.WriteString("padding ")
	}
	got := truncateErr(b.String())
	if len(got) > 500 {
		t.Fatalf("len=%d want <=500", len(got))
	}
	if strings.Contains(got, "AKIDexample0123456789") {
		t.Fatalf("credential survived: %q", got)
	}
}

func TestTruncateErrTrimsWhitespace(t *testing.T) {
	if got := truncateErr("  salsa  "); got != "salsa" {
		t.Fatalf("got %q", got)
	}
}
