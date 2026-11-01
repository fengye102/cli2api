package raccoon

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/caigee-cmd/cli2api/internal/providers"
)

// stubConn is a successful dial's result; only Close is used.
type stubConn struct{}

func (stubConn) Read([]byte) (int, error)         { return 0, nil }
func (stubConn) Write(b []byte) (int, error)      { return len(b), nil }
func (stubConn) Close() error                     { return nil }
func (stubConn) LocalAddr() net.Addr              { return stubAddr{} }
func (stubConn) RemoteAddr() net.Addr             { return stubAddr{} }
func (stubConn) SetDeadline(time.Time) error      { return nil }
func (stubConn) SetReadDeadline(time.Time) error  { return nil }
func (stubConn) SetWriteDeadline(time.Time) error { return nil }

type stubAddr struct{}

func (stubAddr) Network() string { return "tcp" }
func (stubAddr) String() string  { return "stub" }

// A round with no proxy on a deployment whose address the site refuses has to
// say so before the operator authorizes anything, and it has to name the fix.
// This is the regression that made a raccoon login look like it hung after
// pasting the callback: the exchange sat on a thirty-second timeout and the
// message was "context deadline exceeded".
func TestEgressPreflightNamesTheMissingProxy(t *testing.T) {
	client, _ := loginTestClient(t, func(http.ResponseWriter, *http.Request) {})
	client.authBase = "https://xiaohuanxiong.com/api/web/auth/v1"
	client.dial = func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("i/o timeout")
	}
	_, err := client.StartLogin(context.Background(), "acc1")
	if err == nil {
		t.Fatal("a direct round with no proxy must be refused")
	}
	for _, want := range []string{"proxy_url", "xiaohuanxiong.com", "全局代理", "skip_egress_probe"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal should mention %q: %s", want, err)
		}
	}
	// The refusal must not claim the site refuses every direct connection:
	// other egresses reach it, and an operator who knows that would stop
	// trusting the check. Name the source-address block instead.
	if !strings.Contains(err.Error(), "出口 IP") {
		t.Errorf("the refusal should name the blocked egress IP as the cause: %s", err)
	}
	if strings.Contains(err.Error(), "这个站只接受代理出口") {
		t.Errorf("the refusal must not present a site policy that does not exist: %s", err)
	}
}

// TestEgressPreflightHonoursSkipEgressProbe: the pre-flight replaces a silent
// thirty-second hang with an explanation, but it is not a gate. An operator who
// has measured the route - or is waiting out a block that may lift - can wave it
// away and get the round they asked for.
func TestEgressPreflightHonoursSkipEgressProbe(t *testing.T) {
	client, _ := loginTestClient(t, func(http.ResponseWriter, *http.Request) {})
	client.authBase = "https://xiaohuanxiong.com/api/web/auth/v1"
	client.dial = func(context.Context, string, string) (net.Conn, error) {
		t.Fatal("a waived pre-flight must not dial at all")
		return nil, nil
	}
	ctx := providers.WithLoginInput(context.Background(), providers.LoginInput{SkipEgressProbe: true})
	session, err := client.StartLogin(ctx, "acc1")
	if err != nil {
		t.Fatalf("a waived pre-flight must let the round start: %v", err)
	}
	// The test client's site base is its own loopback server, so the round is
	// proven by the authorize path and a state, not by the host name.
	if !strings.Contains(session.AuthURL, "/authorize?appname=") || session.State == "" {
		t.Fatalf("the round should still hand out a real authorize URL, got %q", session.AuthURL)
	}
}

// TestEgressPreflightNamesTheKnownEgress: "configure a proxy" is advice, but an
// operator who already runs an egress on this host should not have to guess the
// address - the refusal names the one this deployment already uses.
func TestEgressPreflightNamesTheKnownEgress(t *testing.T) {
	client, store := loginTestClient(t, func(http.ResponseWriter, *http.Request) {})
	client.authBase = "https://xiaohuanxiong.com/api/web/auth/v1"
	store.secrets = map[string]string{otherEgressSecret: "socks5://ag-egress-socks:17890"}
	client.dial = func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("i/o timeout")
	}
	_, err := client.StartLogin(context.Background(), "acc1")
	if err == nil || !strings.Contains(err.Error(), "socks5://ag-egress-socks:17890") {
		t.Fatalf("the refusal should name the egress this deployment already uses, got %v", err)
	}
}

// TestEgressPreflightBlocksThePasteToo covers a round that was opened before
// the route was fixed: the paste must not spend thirty seconds finding out what
// the pre-flight already knows.
func TestEgressPreflightBlocksThePasteToo(t *testing.T) {
	client, _ := loginTestClient(t, func(http.ResponseWriter, *http.Request) {})
	client.authBase = "https://xiaohuanxiong.com/api/web/auth/v1"
	client.dial = func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("i/o timeout")
	}
	// Open the round directly so the paste is the first thing that sees the check.
	client.setLoginRound("acc1", &loginRound{state: "STATE1", createdAt: time.Now()})
	err := client.CompleteLogin(context.Background(), "acc1", "http://127.0.0.1:1234/raccoon-callback?authorization_code=C&state=STATE1")
	if err == nil || !strings.Contains(err.Error(), "proxy_url") {
		t.Fatalf("the paste should name the missing proxy, got %v", err)
	}
}

func TestEgressPreflightSkipsTheDialWhenAProxyIsConfigured(t *testing.T) {
	client, store := loginTestClient(t, func(http.ResponseWriter, *http.Request) {})
	client.authBase = "https://xiaohuanxiong.com/api/web/auth/v1"
	store.acc = map[string]string{"acc1": "socks5://ag-egress-socks:17890"}
	client.dial = func(context.Context, string, string) (net.Conn, error) {
		t.Fatal("a proxied round must not dial directly")
		return nil, nil
	}
	if _, err := client.StartLogin(context.Background(), "acc1"); err != nil {
		t.Fatalf("a proxied round must go ahead: %v", err)
	}
}

func TestEgressPreflightIgnoresALocalRelay(t *testing.T) {
	client, _ := loginTestClient(t, func(http.ResponseWriter, *http.Request) {})
	client.dial = func(context.Context, string, string) (net.Conn, error) {
		t.Fatal("a loopback auth base is a test server or a local relay, never the blocked route")
		return nil, nil
	}
	if _, err := client.StartLogin(context.Background(), "acc1"); err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
}

func TestEgressPreflightCachesSuccessAndNotFailure(t *testing.T) {
	client, _ := loginTestClient(t, func(http.ResponseWriter, *http.Request) {})
	client.authBase = "https://xiaohuanxiong.com/api/web/auth/v1"
	dials := 0
	failing := false
	client.dial = func(context.Context, string, string) (net.Conn, error) {
		dials++
		if failing {
			return nil, errors.New("i/o timeout")
		}
		return stubConn{}, nil
	}

	if _, err := client.StartLogin(context.Background(), "acc1"); err != nil {
		t.Fatalf("first round: %v", err)
	}
	if _, err := client.StartLogin(context.Background(), "acc1"); err != nil {
		t.Fatalf("second round: %v", err)
	}
	if dials != 1 {
		t.Fatalf("a known-good route should not be re-dialled every round, dials=%d", dials)
	}

	// A failure is never cached: after the route is fixed the next attempt has
	// to work without waiting for a cache to expire.
	failing = true
	client.reachMu.Lock()
	client.reachedAt = map[string]time.Time{}
	client.reachMu.Unlock()
	if _, err := client.StartLogin(context.Background(), "acc1"); err == nil {
		t.Fatal("expected the blocked route to be reported")
	}
	failing = false
	if _, err := client.StartLogin(context.Background(), "acc1"); err != nil {
		t.Fatalf("after the fix the round must go ahead: %v", err)
	}
}
