package raccoon

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/caigee-cmd/cli2api/internal/providers"
)

// Egress reachability.
//
// xiaohuanxiong.com sits behind a WAF that drops TCP from this deployment's own
// egress IP: from the VPS the connect never completes, while ICMP still answers
// and the very same address over the socks egress the accounts carry returns
// 200 at once. It is a block on one source address, not a policy of the site -
// another egress reaches it - but from this host it is a fact, and it used to
// surface in the worst possible place: the operator finished the browser
// authorization, pasted the 127.0.0.1 callback, and then the exchange sat there
// for thirty seconds before failing with "context deadline exceeded" - which
// says nothing about the route.
//
// So a round started with no proxy configured pre-flights the dial: four
// seconds, and the answer is either "go ahead" or a message that names the
// blocked route and the two settings that fix it. A round that does have a
// proxy pays nothing - the proxy does the talking and its health is its own
// business. Successful checks are cached briefly so a burst of rounds does not
// re-dial; failures never are, so once the proxy is configured the next attempt
// works immediately.
const (
	reachDialTimeout = 4 * time.Second
	reachCacheTTL    = 2 * time.Minute
)

// egressReachable reports whether a direct connection to the auth host is
// possible, or explains what to do about it.
func (c *Client) egressReachable(ctx context.Context, accountID string) error {
	// The probe is advice, not a gate: an operator who has measured the route
	// themselves - or who is waiting out a block that may lift - says so and
	// proceeds.
	if providers.LoginInputFromContext(ctx).SkipEgressProbe {
		return nil
	}
	proxy, err := c.effectiveProxy(ctx, accountID)
	if err != nil {
		return err
	}
	if strings.TrimSpace(proxy) != "" {
		return nil
	}
	host, port, ok := authHostPort(c.authBase)
	if !ok {
		return nil
	}
	if c.reached(host) {
		return nil
	}
	dial := c.dialFunc()
	dialCtx, cancel := context.WithTimeout(ctx, reachDialTimeout)
	defer cancel()
	conn, err := dial(dialCtx, "tcp", net.JoinHostPort(host, port))
	if err != nil {
		// Name a value that is already known to work on this deployment, when
		// there is one: "configure a proxy" is advice, "this proxy" is a fix.
		hint := ""
		if known := c.knownEgress(ctx); known != "" {
			hint = fmt.Sprintf("（这个部署上已经在用 %s 作为出口）", known)
		}
		// Say what was measured, not what it might mean: this deployment's
		// egress address is blocked, and the evidence is that the site is
		// reachable from elsewhere. Claiming the site refuses all direct
		// connections would be wrong, and an operator who knows it works from
		// their own machine would rightly stop trusting the check.
		return fmt.Errorf(
			"本机直连 %s:%s 的 TCP 握手被丢弃（%v）：这台机器当前的出口 IP 被站点 WAF 拦了，"+
				"换个出口就正常（同一地址从香港出口是 200，从本机 ICMP 也通、只有 TCP 不通），"+
				"所以不是站点「只接受代理」。当前账号既没有配 proxy_url，也没有全局代理。"+
				"给账号填上 proxy_url%s，或在设置里配置全局代理，然后重新发起登录；"+
				"如果你确认这台机器现在能直连，重新发起时带上 skip_egress_probe 跳过这次探测",
			host, port, err, hint)
	}
	_ = conn.Close()
	c.markReached(host)
	return nil
}

// knownEgress returns an egress this deployment is already known to use.
//
// It is only ever used to word an error message - never to route traffic. An
// operator who already configured an egress somewhere else on this host should
// not have to guess what to type.
func (c *Client) knownEgress(ctx context.Context) string {
	store, ok := c.store.(SecretReader)
	if !ok {
		return ""
	}
	value, found, err := store.GetSecret(ctx, otherEgressSecret)
	if err != nil || !found {
		return ""
	}
	return strings.TrimSpace(value)
}

// otherEgressSecret is where the deployment keeps the egress its other
// channels use. Read-only here, and only for the hint above.
const otherEgressSecret = "antigravity_region_egress"

// dialFunc returns the dialer, overridable from tests.
func (c *Client) dialFunc() func(context.Context, string, string) (net.Conn, error) {
	if c.dial != nil {
		return c.dial
	}
	dialer := &net.Dialer{Timeout: reachDialTimeout}
	return dialer.DialContext
}

// reached reports whether the host answered recently.
func (c *Client) reached(host string) bool {
	c.reachMu.Lock()
	defer c.reachMu.Unlock()
	at, ok := c.reachedAt[host]
	return ok && time.Since(at) < reachCacheTTL
}

// markReached remembers a successful dial.
func (c *Client) markReached(host string) {
	c.reachMu.Lock()
	defer c.reachMu.Unlock()
	if c.reachedAt == nil {
		c.reachedAt = map[string]time.Time{}
	}
	c.reachedAt[host] = time.Now()
}

// authHostPort pulls the host and port the exchange will actually talk to.
//
// A loopback or private address is skipped: it is a test server or a local
// relay, and the WAF that blocks this deployment has nothing to do with it.
func authHostPort(raw string) (string, string, bool) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Hostname() == "" {
		return "", "", false
	}
	host := parsed.Hostname()
	if strings.EqualFold(host, "localhost") {
		return "", "", false
	}
	if ip := net.ParseIP(host); ip != nil && (ip.IsLoopback() || ip.IsPrivate()) {
		return "", "", false
	}
	port := parsed.Port()
	if port == "" {
		port = "443"
		if parsed.Scheme == "http" {
			port = "80"
		}
	}
	return host, port, true
}
