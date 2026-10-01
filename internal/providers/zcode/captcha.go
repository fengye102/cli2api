package zcode

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The plan (OAuth) channel gates every chat request behind Aliyun's traceless
// verification: without a fresh X-Aliyun-Captcha-Verify-Param the gateway
// answers 405/code 3012 "unusual activity". The token is minted by a real
// browser running Aliyun's SDK (a simulated DOM is scored as a bot and never
// passes), so it is solved out-of-process by the bundled Node solver and kept
// in a small prefetch pool: the chat hot path never waits on a browser launch,
// and a token that the upstream rejects empties the pool.
const (
	defaultCaptchaDir     = "/app/captcha_node"
	defaultCaptchaScene   = "11xygtvd"
	defaultCaptchaPrefix  = "no8xfe"
	defaultCaptchaRegion  = "sgp"
	captchaClientConfigs  = "https://zcode.z.ai/api/v1/client/configs"
	captchaConfigCacheTTL = 30 * time.Minute
)

type captchaConfig struct {
	Enabled bool   `json:"enabled"`
	Prefix  string `json:"prefix"`
	Region  string `json:"region"`
	SceneID string `json:"sceneId"`
}

type captchaToken struct {
	param  string
	region string
	born   time.Time
}

func (t captchaToken) expired(ttl time.Duration) bool {
	return time.Since(t.born) >= ttl
}

// captchaPool solves and caches plan-channel verification tokens.
type captchaPool struct {
	http    *http.Client
	node    string
	dir     string
	timeout time.Duration
	ttl     time.Duration
	min     int
	max     int

	solveMu sync.Mutex

	mu      sync.Mutex
	tokens  []captchaToken
	cfg     captchaConfig
	cfgAt   time.Time
	lastErr string
	// lastUse gates the idle refills: an unused server must not keep solving
	// captchas, both to save memory and to stay under Aliyun's radar.
	lastUse time.Time

	startOnce sync.Once
	stopOnce  sync.Once
	stopCh    chan struct{}
}

func newCaptchaPool(client *http.Client) *captchaPool {
	return &captchaPool{
		http:    client,
		node:    envOr("ZCODE_NODE_BINARY", envOr("QODER_NODE_BINARY", "node")),
		dir:     envOr("ZCODE_CAPTCHA_DIR", defaultCaptchaDir),
		timeout: time.Duration(envInt("ZCODE_CAPTCHA_TIMEOUT", 240)) * time.Second,
		ttl:     time.Duration(envInt("ZCODE_CAPTCHA_TTL", 60)) * time.Second,
		min:     envInt("ZCODE_CAPTCHA_POOL_MIN", 1),
		max:     envInt("ZCODE_CAPTCHA_POOL_MAX", 2),
		stopCh:  make(chan struct{}),
	}
}

func envInt(key string, fallback int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

// enabled reports whether the bundled solver is usable. A deployment without
// the solver assets degrades to today's behaviour (request sent unsigned,
// upstream answers 3012) instead of failing the chat path outright.
func (p *captchaPool) enabled() bool {
	if p == nil {
		return false
	}
	if v := strings.TrimSpace(os.Getenv("ZCODE_CAPTCHA_DISABLED")); v == "1" || strings.EqualFold(v, "true") {
		return false
	}
	_, err := os.Stat(p.solverScript())
	return err == nil
}

func (p *captchaPool) solverScript() string {
	if custom := strings.TrimSpace(os.Getenv("ZCODE_CAPTCHA_SOLVER")); custom != "" {
		return custom
	}
	preferred := filepath.Join(p.dir, "solver.js")
	if _, err := os.Stat(preferred); err == nil {
		return preferred
	}
	return filepath.Join(p.dir, "solver_pw.js")
}

// param returns a usable token, solving one inline when the pool is empty.
func (p *captchaPool) param(ctx context.Context) (string, string, error) {
	p.startOnce.Do(func() { go p.refillLoop() })

	if token, ok := p.take(); ok {
		return token.param, token.region, nil
	}

	p.solveMu.Lock()
	defer p.solveMu.Unlock()
	// Another caller may have solved while we waited for the lock.
	if token, ok := p.take(); ok {
		return token.param, token.region, nil
	}
	cfg := p.config(ctx)
	token, err := p.solve(ctx, cfg)
	if err != nil {
		p.setErr(err.Error())
		return "", "", err
	}
	p.setErr("")
	p.mu.Lock()
	p.lastUse = time.Now()
	p.mu.Unlock()
	return token.param, token.region, nil
}

// invalidate drops every cached token: the upstream rejected one, so the batch
// it came from is suspect and reusing it only repeats the failure.
func (p *captchaPool) invalidate() {
	p.mu.Lock()
	p.tokens = nil
	p.mu.Unlock()
}

func (p *captchaPool) take() (captchaToken, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for len(p.tokens) > 0 {
		token := p.tokens[0]
		p.tokens = p.tokens[1:]
		if !token.expired(p.ttl) {
			return token, true
		}
	}
	return captchaToken{}, false
}

func (p *captchaPool) noteUse() {
	p.mu.Lock()
	p.lastUse = time.Now()
	p.mu.Unlock()
	p.kickRefill()
}

// kickRefill tops the pool back up in the background so the next request does
// not pay for a browser launch.
func (p *captchaPool) kickRefill() {
	if !p.enabled() {
		return
	}
	p.mu.Lock()
	pending := len(p.tokens)
	p.mu.Unlock()
	if pending >= p.min {
		return
	}
	go func() {
		p.solveMu.Lock()
		defer p.solveMu.Unlock()
		p.mu.Lock()
		now := len(p.tokens)
		p.mu.Unlock()
		if now >= p.min {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), p.timeout+30*time.Second)
		defer cancel()
		token, err := p.solve(ctx, p.config(ctx))
		if err != nil {
			p.setErr(err.Error())
			return
		}
		p.setErr("")
		p.put(token)
	}()
}

func (p *captchaPool) put(token captchaToken) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.tokens) >= p.max {
		return
	}
	p.tokens = append(p.tokens, token)
}

func (p *captchaPool) setErr(message string) {
	p.mu.Lock()
	p.lastErr = message
	p.mu.Unlock()
}

// lastError reports the most recent solver failure (console diagnostics).
func (p *captchaPool) lastError() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastErr
}

func (p *captchaPool) stop() {
	p.stopOnce.Do(func() { close(p.stopCh) })
}

func (p *captchaPool) refillLoop() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-p.stopCh:
			return
		case <-ticker.C:
		}
		if !p.enabled() {
			continue
		}
		p.mu.Lock()
		pending := len(p.tokens)
		lastUse := p.lastUse
		p.mu.Unlock()
		if pending >= p.min {
			continue
		}
		// Only keep a token warm while the channel is actually in use.
		if lastUse.IsZero() || time.Since(lastUse) > 10*time.Minute {
			continue
		}
		p.solveMu.Lock()
		ctx, cancel := context.WithTimeout(context.Background(), p.timeout+30*time.Second)
		token, err := p.solve(ctx, p.config(ctx))
		cancel()
		p.solveMu.Unlock()
		if err != nil {
			p.setErr(err.Error())
			continue
		}
		p.setErr("")
		p.put(token)
	}
}

// config reads the captcha scene the upstream advertises; the values are
// version-sensitive but stable enough to cache for half an hour.
func (p *captchaPool) config(ctx context.Context) captchaConfig {
	p.mu.Lock()
	cached, at := p.cfg, p.cfgAt
	p.mu.Unlock()
	if cached.SceneID != "" && time.Since(at) < captchaConfigCacheTTL {
		return cached
	}

	cfg := captchaConfig{Enabled: true, Prefix: defaultCaptchaPrefix, Region: defaultCaptchaRegion, SceneID: defaultCaptchaScene}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, captchaClientConfigs+"?app_version="+Version, nil)
	if err == nil {
		for k, v := range map[string]string{"User-Agent": userAgent(), "X-ZCode-App-Version": Version} {
			req.Header.Set(k, v)
		}
		resp, err := p.http.Do(req)
		if err == nil {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			var envelope struct {
				Data struct {
					Configs struct {
						Captcha captchaConfig `json:"captcha"`
					} `json:"configs"`
				} `json:"data"`
			}
			if json.Unmarshal(body, &envelope) == nil {
				live := envelope.Data.Configs.Captcha
				if strings.TrimSpace(live.SceneID) != "" {
					cfg = live
				}
			}
		}
	}

	p.mu.Lock()
	p.cfg, p.cfgAt = cfg, time.Now()
	p.mu.Unlock()
	return cfg
}

// solve runs the Node solver once and returns the first token it prints. The
// token is read off the pipe instead of waiting for the process to exit: the
// solver kills its browser as soon as a token exists, and a token that arrived
// late is worthless anyway (it is only valid for a couple of minutes).
func (p *captchaPool) solve(ctx context.Context, cfg captchaConfig) (captchaToken, error) {
	script := p.solverScript()
	if _, err := os.Stat(script); err != nil {
		return captchaToken{}, fmt.Errorf("zcode captcha solver not found at %s", script)
	}
	solveCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	cmd := exec.CommandContext(solveCtx, p.node, script, cfg.SceneID, cfg.Region, cfg.Prefix)
	cmd.Dir = p.dir
	runDir := filepath.Join(os.TempDir(), fmt.Sprintf("cli2api-captcha-%d-%d", os.Getpid(), time.Now().UnixNano()))
	cmd.Env = append(os.Environ(), "ZCODE_CAPTCHA_RUN_DIR="+runDir)
	isolateProcess(cmd)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return captchaToken{}, fmt.Errorf("zcode captcha solver stdout: %w", err)
	}
	var errOut bytes.Buffer
	cmd.Stderr = &errOut
	if err := cmd.Start(); err != nil {
		return captchaToken{}, fmt.Errorf("zcode captcha solver start: %w", err)
	}

	tokens := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for scanner.Scan() {
			if param := parseVerifyParam(scanner.Text()); param != "" {
				tokens <- param
				return
			}
		}
	}()
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	cleanup := func() {
		killProcessGroup(cmd)
		<-exited
		reapLeftovers(runDir)
		_ = os.RemoveAll(runDir)
	}

	select {
	case param := <-tokens:
		cleanup()
		return captchaToken{param: param, region: cfg.Region, born: time.Now()}, nil
	case err := <-exited:
		detail := lastNonEmptyLine(errOut.String())
		if err != nil {
			return captchaToken{}, fmt.Errorf("zcode captcha solve failed: %v%s", err, suffix(detail))
		}
		return captchaToken{}, fmt.Errorf("zcode captcha solve produced no token%s", suffix(detail))
	case <-solveCtx.Done():
		cleanup()
		if ctx.Err() != nil {
			return captchaToken{}, ctx.Err()
		}
		return captchaToken{}, fmt.Errorf("zcode captcha solve timed out after %s", p.timeout)
	}
}

func parseVerifyParam(stdout string) string {
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "VERIFY_PARAM=") {
			return strings.TrimSpace(strings.TrimPrefix(line, "VERIFY_PARAM="))
		}
	}
	return ""
}

func lastNonEmptyLine(text string) string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if line := strings.TrimSpace(lines[i]); line != "" {
			return line
		}
	}
	return ""
}

func suffix(detail string) string {
	if detail == "" {
		return ""
	}
	return ": " + detail
}
