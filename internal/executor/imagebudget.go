package executor

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/caigee-cmd/cli2api/internal/translate"
)

// Inline screenshot uploads are the one payload a client cannot judge for
// itself: a single 2560x1600 PNG pasted into a conversation becomes ~2.7MB of
// base64, which dominates the request, burns vision tokens on pixels the model
// downscales anyway, and can push the conversation past the model's window. The
// gateway knows which model will actually serve the request, so it is the right
// place to shrink the image once, before dispatch.
//
// Only images that exceed the budgets are touched; everything else stays
// byte-identical, so an already-small upload is never re-encoded (which would
// cost quality and CPU for nothing).
const (
	// imageMaxEdgeDefault caps the longest side. 1568px is what the major vision
	// models resize to internally, so anything larger is discarded upstream.
	imageMaxEdgeDefault = 1568
	// imageMaxBytesDefault caps one decoded image at ~700KB, which becomes
	// roughly 950k base64 characters on the wire.
	imageMaxBytesDefault = 700 * 1024
	// imageMaxDecodePixels refuses absurd uploads outright: 40MP is far past any
	// screenshot and would make decoding itself the expensive part.
	imageMaxDecodePixels = 40_000_000
	// imageJPEGQuality keeps text edges readable for screenshots and code.
	imageJPEGQuality = 85
)

type imageBudget struct {
	enabled  bool
	maxEdge  int
	maxBytes int
}

var (
	imageBudgetOnce sync.Once
	imageBudgetCfg  imageBudget
)

// loadImageBudget reads the budgets once per process. They are env-driven like
// the other knobs (log retention, ports) so an operator can tune them per
// deployment without a config round trip.
func loadImageBudget() imageBudget {
	imageBudgetOnce.Do(func() {
		cfg := imageBudget{enabled: true, maxEdge: imageMaxEdgeDefault, maxBytes: imageMaxBytesDefault}
		switch strings.ToLower(strings.TrimSpace(os.Getenv("CLI2API_IMAGE_COMPRESS"))) {
		case "0", "off", "false", "no", "disable", "disabled":
			cfg.enabled = false
		}
		if v := strings.TrimSpace(os.Getenv("CLI2API_IMAGE_MAX_EDGE")); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n >= 128 {
				cfg.maxEdge = n
			}
		}
		if v := strings.TrimSpace(os.Getenv("CLI2API_IMAGE_MAX_BYTES")); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n >= 16*1024 {
				cfg.maxBytes = n
			}
		}
		imageBudgetCfg = cfg
	})
	return imageBudgetCfg
}

// ShrinkInlineImages rewrites the base64 data-URL images inside a conversation
// so each one fits the edge and byte budgets. It returns how many images were
// rewritten and how many base64 bytes the request shed.
//
// It is a no-op costing one type switch per message when the request carries no
// inline image, which is the common case.
func ShrinkInlineImages(messages []translate.ChatMessage) (changed int, saved int64) {
	cfg := loadImageBudget()
	if !cfg.enabled || len(messages) == 0 {
		return 0, 0
	}
	for i := range messages {
		parts, ok := messages[i].Content.([]any)
		if !ok {
			continue
		}
		for _, raw := range parts {
			part, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			url, nested, ok := imagePartURL(part)
			if !ok {
				continue
			}
			shrunk, didChange, delta := shrinkDataURL(url, cfg)
			if !didChange {
				continue
			}
			if nested {
				part["image_url"] = map[string]any{"url": shrunk}
			} else {
				part["image_url"] = shrunk
			}
			changed++
			saved += delta
		}
	}
	if changed > 0 {
		log.Printf("image budget: shrunk %d inline image(s), saved %d base64 byte(s)", changed, saved)
	}
	return changed, saved
}

// imagePartURL pulls the URL out of an image content part and reports whether it
// was wrapped in the OpenAI-shaped {"image_url": {"url": ...}} envelope.
func imagePartURL(part map[string]any) (url string, nested bool, ok bool) {
	kind, _ := part["type"].(string)
	if kind != "image_url" && kind != "input_image" && kind != "image" {
		return "", false, false
	}
	switch value := part["image_url"].(type) {
	case string:
		return value, false, true
	case map[string]any:
		if s, _ := value["url"].(string); s != "" {
			return s, true, true
		}
	case map[string]string:
		if s := value["url"]; s != "" {
			return s, true, true
		}
	}
	return "", false, false
}

// shrinkDataURL re-encodes one data-URL image when it exceeds a budget. The
// returned delta is the base64 length removed from the request.
func shrinkDataURL(rawURL string, cfg imageBudget) (string, bool, int64) {
	if !strings.HasPrefix(rawURL, "data:") {
		// Remote http(s) images are left alone: fetching them here would make the
		// gateway depend on the caller's URL being reachable from this host.
		return rawURL, false, 0
	}
	comma := strings.IndexByte(rawURL, ',')
	if comma < 0 || !strings.Contains(rawURL[:comma], "base64") {
		return rawURL, false, 0
	}
	payload := rawURL[comma+1:]
	data, err := decodeBase64Loose(payload)
	if err != nil || len(data) == 0 {
		return rawURL, false, 0
	}

	header, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return rawURL, false, 0
	}
	if header.Width*header.Height > imageMaxDecodePixels {
		log.Printf("image budget: refusing %dx%d image (%d bytes), too large to decode",
			header.Width, header.Height, len(data))
		return rawURL, false, 0
	}
	edge := header.Width
	if header.Height > edge {
		edge = header.Height
	}
	if edge <= cfg.maxEdge && len(data) <= cfg.maxBytes {
		return rawURL, false, 0
	}

	src, format, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return rawURL, false, 0
	}
	opaque := format == "jpeg" || !hasAlpha(src)
	mime := "image/jpeg"
	if !opaque {
		mime = "image/png"
	}

	// Tighten until both budgets hold. A wide screenshot can sit inside the edge
	// budget and still be heavy, so the loop measures encoded size, not pixels.
	scale := 1.0
	if edge > cfg.maxEdge {
		scale = float64(cfg.maxEdge) / float64(edge)
	}
	srcBounds := src.Bounds()
	var encoded []byte
	for attempt := 0; attempt < 6; attempt++ {
		candidate := src
		if scale < 1 {
			candidate = boxResize(src, int(float64(srcBounds.Dx())*scale), int(float64(srcBounds.Dy())*scale))
		}
		encoded = encodeImage(candidate, mime)
		if len(encoded) == 0 {
			return rawURL, false, 0
		}
		if len(encoded) <= cfg.maxBytes || scale < 0.25 {
			break
		}
		scale *= 0.8
	}
	if len(encoded) == 0 || len(encoded) >= len(data) {
		// Re-encoding grew the payload (already-optimised PNG, tiny JPEG): keep
		// the original bytes rather than shipping something larger.
		return rawURL, false, 0
	}

	out := "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(encoded)
	return out, true, int64(len(payload) - (len(out) - comma - 1))
}

func decodeBase64Loose(payload string) ([]byte, error) {
	if data, err := base64.StdEncoding.DecodeString(payload); err == nil {
		return data, nil
	}
	return base64.RawStdEncoding.DecodeString(strings.TrimRight(payload, "="))
}

func encodeImage(img image.Image, mime string) []byte {
	var buf bytes.Buffer
	var err error
	if mime == "image/png" {
		err = png.Encode(&buf, img)
	} else {
		err = jpeg.Encode(&buf, img, &jpeg.Options{Quality: imageJPEGQuality})
	}
	if err != nil {
		return nil
	}
	return buf.Bytes()
}

// hasAlpha samples for any pixel that is not fully opaque. Sampling on a stride
// keeps this cheap on large screenshots while still catching the transparent
// regions that matter.
func hasAlpha(img image.Image) bool {
	bounds := img.Bounds()
	stepX := bounds.Dx() / 32
	if stepX < 1 {
		stepX = 1
	}
	stepY := bounds.Dy() / 32
	if stepY < 1 {
		stepY = 1
	}
	for y := bounds.Min.Y; y < bounds.Max.Y; y += stepY {
		for x := bounds.Min.X; x < bounds.Max.X; x += stepX {
			if _, _, _, a := img.At(x, y).RGBA(); a < 0xffff {
				return true
			}
		}
	}
	return false
}

// boxResize averages source pixels into the destination grid. Downscale only,
// which is all the budget ever asks for: averaging keeps screenshot text sharp
// and avoids the aliasing a nearest-neighbour shrink would introduce.
func boxResize(src image.Image, width, height int) *image.NRGBA {
	if width < 1 {
		width = 1
	}
	if height < 1 {
		height = 1
	}
	bounds := src.Bounds()
	dst := image.NewNRGBA(image.Rect(0, 0, width, height))
	for dy := 0; dy < height; dy++ {
		y0 := bounds.Min.Y + dy*bounds.Dy()/height
		y1 := bounds.Min.Y + (dy+1)*bounds.Dy()/height
		if y1 <= y0 {
			y1 = y0 + 1
		}
		for dx := 0; dx < width; dx++ {
			x0 := bounds.Min.X + dx*bounds.Dx()/width
			x1 := bounds.Min.X + (dx+1)*bounds.Dx()/width
			if x1 <= x0 {
				x1 = x0 + 1
			}
			var r, g, b, a, n uint64
			for y := y0; y < y1; y++ {
				for x := x0; x < x1; x++ {
					pr, pg, pb, pa := src.At(x, y).RGBA()
					r += uint64(pr)
					g += uint64(pg)
					b += uint64(pb)
					a += uint64(pa)
					n++
				}
			}
			if n == 0 {
				continue
			}
			dst.SetNRGBA(dx, dy, color.NRGBA{
				R: uint8(r / n >> 8),
				G: uint8(g / n >> 8),
				B: uint8(b / n >> 8),
				A: uint8(a / n >> 8),
			})
		}
	}
	return dst
}
