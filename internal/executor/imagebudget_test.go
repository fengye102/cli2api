package executor

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"

	"github.com/caigee-cmd/cli2api/internal/translate"
)

// setTestImageBudget pins the cached budgets for one test, since loadImageBudget
// is sync.Once-guarded per process.
func setTestImageBudget(t *testing.T, cfg imageBudget) {
	t.Helper()
	imageBudgetOnce.Do(func() {})
	previous := imageBudgetCfg
	imageBudgetCfg = cfg
	t.Cleanup(func() { imageBudgetCfg = previous })
}

// noisyPNG builds an incompressible PNG: real screenshots never compress to a
// few KB, and a flat fill would make the size assertions meaningless.
func noisyPNG(t *testing.T, width, height int, alpha bool) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	seed := uint32(12345)
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			seed = seed*1664525 + 1013904223
			a := uint8(255)
			if alpha && x < width/4 {
				a = 0
			}
			img.SetNRGBA(x, y, color.NRGBA{R: uint8(seed >> 24), G: uint8(seed >> 16), B: uint8(seed >> 8), A: a})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode test png: %v", err)
	}
	return buf.Bytes()
}

func imageMessage(url string) translate.ChatMessage {
	return translate.ChatMessage{
		Role: "user",
		Content: []any{
			map[string]any{"type": "text", "text": "what is in this screenshot?"},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}},
		},
	}
}

func dataURL(payload []byte) string {
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(payload)
}

func partURL(t *testing.T, message translate.ChatMessage) string {
	t.Helper()
	parts, ok := message.Content.([]any)
	if !ok {
		t.Fatalf("content is not parts: %T", message.Content)
	}
	part := parts[1].(map[string]any)
	nested, ok := part["image_url"].(map[string]any)
	if !ok {
		t.Fatalf("image_url envelope shape changed: %T", part["image_url"])
	}
	return nested["url"].(string)
}

func decodeDataURL(t *testing.T, url string) []byte {
	t.Helper()
	comma := strings.IndexByte(url, ',')
	if comma < 0 {
		t.Fatalf("not a data url: %.40s", url)
	}
	data, err := base64.StdEncoding.DecodeString(url[comma+1:])
	if err != nil {
		t.Fatalf("decode data url: %v", err)
	}
	return data
}

// TestShrinkInlineImagesLeavesSmallImagesAlone: an already-small upload must not
// be re-encoded, or every screenshot would lose quality for nothing.
func TestShrinkInlineImagesLeavesSmallImagesAlone(t *testing.T) {
	setTestImageBudget(t, imageBudget{enabled: true, maxEdge: 1568, maxBytes: 700 * 1024})
	original := dataURL(noisyPNG(t, 320, 200, false))
	message := imageMessage(original)

	changed, _ := ShrinkInlineImages([]translate.ChatMessage{message})
	if changed != 0 {
		t.Fatalf("small image was rewritten")
	}
	if got := partURL(t, message); got != original {
		t.Fatal("small image bytes changed")
	}
}

// TestShrinkInlineImagesDownscalesOversizedScreenshot covers the case this
// exists for: a 2400x1600 screenshot.
func TestShrinkInlineImagesDownscalesOversizedScreenshot(t *testing.T) {
	setTestImageBudget(t, imageBudget{enabled: true, maxEdge: 1568, maxBytes: 700 * 1024})
	original := dataURL(noisyPNG(t, 2400, 1600, false))
	message := imageMessage(original)

	changed, saved := ShrinkInlineImages([]translate.ChatMessage{message})
	if changed != 1 {
		t.Fatalf("expected one rewritten image, got %d", changed)
	}
	if saved <= 0 {
		t.Fatalf("expected bytes to be shed, got %d", saved)
	}
	shrunk := partURL(t, message)
	if shrunk == original {
		t.Fatal("oversized image was not rewritten")
	}
	if !strings.HasPrefix(shrunk, "data:image/jpeg;base64,") {
		t.Fatalf("opaque screenshot should become jpeg, got %.30s", shrunk)
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(decodeDataURL(t, shrunk)))
	if err != nil {
		t.Fatalf("decode shrunk image: %v", err)
	}
	if cfg.Width > 1568 || cfg.Height > 1568 {
		t.Fatalf("shrunk image still %dx%d", cfg.Width, cfg.Height)
	}
	if len(decodeDataURL(t, shrunk)) > 700*1024 {
		t.Fatal("shrunk image still over the byte budget")
	}
}

// TestShrinkInlineImagesRespectsAlpha: transparency must survive, which means
// staying PNG instead of becoming a black-backed JPEG.
func TestShrinkInlineImagesRespectsAlpha(t *testing.T) {
	setTestImageBudget(t, imageBudget{enabled: true, maxEdge: 800, maxBytes: 700 * 1024})
	message := imageMessage(dataURL(noisyPNG(t, 1600, 1200, true)))

	if changed, _ := ShrinkInlineImages([]translate.ChatMessage{message}); changed != 1 {
		t.Fatalf("expected the transparent image to be rewritten, got %d", changed)
	}
	if got := partURL(t, message); !strings.HasPrefix(got, "data:image/png;base64,") {
		t.Fatalf("transparent image must stay png, got %.30s", got)
	}
}

// TestShrinkInlineImagesIgnoresNonDataURLs: remote URLs and malformed payloads
// are passed through untouched.
func TestShrinkInlineImagesIgnoresNonDataURLs(t *testing.T) {
	setTestImageBudget(t, imageBudget{enabled: true, maxEdge: 128, maxBytes: 16 * 1024})
	cases := []string{
		"https://example.com/huge.png",
		"data:image/png;base64,not-base64!!!",
		"data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("definitely not an image")),
	}
	for _, url := range cases {
		message := imageMessage(url)
		if changed, _ := ShrinkInlineImages([]translate.ChatMessage{message}); changed != 0 {
			t.Fatalf("rewrote untouched payload %.40s", url)
		}
		if got := partURL(t, message); got != url {
			t.Fatalf("payload changed: %.40s", url)
		}
	}
}

// TestShrinkInlineImagesHandlesPlainStringContent: text-only conversations and
// string content must be skipped without touching them.
func TestShrinkInlineImagesHandlesPlainStringContent(t *testing.T) {
	setTestImageBudget(t, imageBudget{enabled: true, maxEdge: 1568, maxBytes: 700 * 1024})
	messages := []translate.ChatMessage{
		{Role: "system", Content: "be brief"},
		{Role: "user", Content: "no images here"},
	}
	if changed, saved := ShrinkInlineImages(messages); changed != 0 || saved != 0 {
		t.Fatalf("text-only request was modified")
	}
	if messages[1].Content != "no images here" {
		t.Fatal("string content was rewritten")
	}
}

// TestShrinkInlineImagesDisabled: the env kill switch must win.
func TestShrinkInlineImagesDisabled(t *testing.T) {
	setTestImageBudget(t, imageBudget{enabled: false, maxEdge: 256, maxBytes: 16 * 1024})
	original := dataURL(noisyPNG(t, 1200, 900, false))
	message := imageMessage(original)
	if changed, _ := ShrinkInlineImages([]translate.ChatMessage{message}); changed != 0 {
		t.Fatal("compression ran while disabled")
	}
	if got := partURL(t, message); got != original {
		t.Fatal("image changed while compression was disabled")
	}
}
