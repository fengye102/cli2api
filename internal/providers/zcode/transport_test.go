package zcode

import (
	"crypto/tls"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// seenRequest is one arrival as the TLS server observed it.
type seenRequest struct {
	proto string
	alpn  string
}

// recordServer starts a TLS test server and records how each request arrived.
// h2 makes the server speak HTTP/2 (httptest then offers h2 in ALPN by itself);
// nextProtos pins ALPN explicitly for the cases that need a http/1.1 offer.
func recordServer(t *testing.T, h2 bool, nextProtos []string) (*httptest.Server, func() []seenRequest) {
	t.Helper()
	var (
		mu   sync.Mutex
		seen []seenRequest
	)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entry := seenRequest{proto: r.Proto}
		if r.TLS != nil {
			entry.alpn = r.TLS.NegotiatedProtocol
		}
		mu.Lock()
		seen = append(seen, entry)
		mu.Unlock()
		_, _ = w.Write([]byte("ok"))
	})
	server := httptest.NewUnstartedServer(handler)
	server.EnableHTTP2 = h2
	if nextProtos != nil {
		server.TLS = &tls.Config{NextProtos: nextProtos}
	}
	server.StartTLS()
	t.Cleanup(server.Close)
	return server, func() []seenRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]seenRequest(nil), seen...)
	}
}

func callOnce(t *testing.T, transport *http.Transport, url string) {
	t.Helper()
	client := &http.Client{Transport: transport}
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatalf("read body: %v", err)
	}
}

// insecure accepts the test server's self-signed cert; it says nothing about the
// transport's protocol choice.
func insecure() *tls.Config { return &tls.Config{InsecureSkipVerify: true} }

// TestUpstreamTransportRefusesHTTP2 pins the reason for the transport: the stock
// Go transport takes the h2 a server offers (as zcode.z.ai does), the ZCode one
// stays on HTTP/1.1 like every real ZCode client.
func TestUpstreamTransportRefusesHTTP2(t *testing.T) {
	server, seen := recordServer(t, true, nil)

	control := http.DefaultTransport.(*http.Transport).Clone()
	control.TLSClientConfig = insecure()
	callOnce(t, control, server.URL)

	callOnce(t, http1Only(&http.Transport{TLSClientConfig: insecure()}), server.URL)

	got := seen()
	if len(got) != 2 {
		t.Fatalf("want 2 requests, saw %+v", got)
	}
	if got[0].proto != "HTTP/2.0" {
		t.Fatalf("control arrived as %q, so this server does not offer h2 and the test proves nothing", got[0].proto)
	}
	if got[1].proto != "HTTP/1.1" {
		t.Fatalf("zcode transport arrived as %q, want HTTP/1.1", got[1].proto)
	}
}

// TestUpstreamTransportOffersHTTP11ALPN checks the client still sends an ALPN
// offer naming http/1.1 — a real ZCode client (Node/Bun fetch) does, and the TLS
// hello is what the firewall fingerprints, not merely the absence of h2.
func TestUpstreamTransportOffersHTTP11ALPN(t *testing.T) {
	server, seen := recordServer(t, false, []string{"http/1.1"})
	callOnce(t, http1Only(&http.Transport{TLSClientConfig: insecure()}), server.URL)

	got := seen()
	if len(got) != 1 {
		t.Fatalf("want 1 request, saw %+v", got)
	}
	if got[0].proto != "HTTP/1.1" || got[0].alpn != "http/1.1" {
		t.Fatalf("arrived as %+v, want HTTP/1.1 with ALPN http/1.1", got[0])
	}
}

// TestNewClientIsHTTP11Only guards the wiring: the shared client every ZCode call
// goes through (chat, claim, probe, captcha) must be the h1 transport.
func TestNewClientIsHTTP11Only(t *testing.T) {
	client := NewClient(nil)
	transport, ok := client.http.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport is %T, want *http.Transport", client.http.Transport)
	}
	if transport.ForceAttemptHTTP2 {
		t.Fatal("ForceAttemptHTTP2 must be off")
	}
	if transport.TLSClientConfig == nil {
		t.Fatal("TLSClientConfig must carry the ALPN pin")
	}
	if got := transport.TLSClientConfig.NextProtos; len(got) != 1 || got[0] != "http/1.1" {
		t.Fatalf("NextProtos = %v, want [http/1.1]", got)
	}
	if transport.TLSNextProto == nil {
		t.Fatal("TLSNextProto must be non-nil so net/http cannot re-enable h2")
	}
	if client.captcha == nil {
		t.Fatal("captcha pool must share the client")
	}
}
