package probe

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// newTLSServer gives a Prober pointed at an httptest TLS server, so no test
// touches the real network (the shell's curl calls, ported to net/http).
func newTLSServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *Prober) {
	t.Helper()
	srv := httptest.NewTLSServer(handler)
	t.Cleanup(srv.Close)
	p := &Prober{HTTPClient: srv.Client()}
	return srv, p
}

func hostOf(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	// Strip the scheme: the Prober takes a bare host (it owns the https).
	u := srv.URL
	if i := index(u, "://"); i >= 0 {
		return u[i+3:]
	}
	return u
}

func index(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestLiveStatusOK(t *testing.T) {
	// The health path is project config handed in by the caller — the
	// prober must request exactly the path it was given.
	srv, p := newTLSServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			t.Errorf("probe path = %q, want /healthz (the configured HealthPath)", r.URL.Path)
		}
		w.Write([]byte(`{"ok":true,"git":"ccc3333"}`))
	})
	got, ok := p.LiveStatus(context.Background(), hostOf(t, srv), "/healthz")
	if !ok {
		t.Fatal("LiveStatus() not ok")
	}
	if got != `{"ok":true,"git":"ccc3333"}` {
		t.Errorf("LiveStatus() body = %q", got)
	}
}

func TestLiveStatusFailsOnHTTPError(t *testing.T) {
	srv, p := newTLSServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	})
	if _, ok := p.LiveStatus(context.Background(), hostOf(t, srv), "/healthz"); ok {
		t.Error("LiveStatus() on 502 must not be ok (curl -sf parity)")
	}
}

func TestLiveStatusFailsClosedOnUnreachable(t *testing.T) {
	p := &Prober{HTTPClient: &http.Client{}}
	if _, ok := p.LiveStatus(context.Background(), "127.0.0.1:1", "/healthz"); ok {
		t.Error("LiveStatus() on a closed port must not be ok")
	}
}

func TestBuildID(t *testing.T) {
	srv, p := newTLSServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/build-id.txt" {
			t.Errorf("probe path = %q, want /build-id.txt", r.URL.Path)
		}
		w.Write([]byte("ccc3333\n"))
	})
	got, ok := p.BuildID(context.Background(), hostOf(t, srv))
	if !ok {
		t.Fatal("BuildID() not ok")
	}
	if got != "ccc3333" {
		t.Errorf("BuildID() = %q, want ccc3333 (whitespace trimmed)", got)
	}
}

func TestBuildIDToleratesFailure(t *testing.T) {
	p := &Prober{HTTPClient: &http.Client{}}
	if _, ok := p.BuildID(context.Background(), "127.0.0.1:1"); ok {
		t.Error("BuildID() on a closed port must not be ok — the caller prints 'unreachable'")
	}
}

func TestProberDefaultsToCurlTimeout(t *testing.T) {
	p := &Prober{HTTPClient: nil} // default client
	if got := p.client().Timeout.String(); got != "8s" {
		t.Errorf("default probe timeout = %s, want 8s (curl -m 8 parity)", got)
	}
}

func TestResolveTransportVerifiesSNIThroughIP(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"git":"abc1234"}`)
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	host, port, _ := net.SplitHostPort(u.Host)

	// The test cert is valid for example.com: dial the server's 127.0.0.1
	// address with SNI example.com => handshake must succeed.
	roots := x509.NewCertPool()
	roots.AddCert(srv.Certificate())
	tr := resolveTransport(host, "example.com", roots)
	if tr == nil {
		t.Fatal("nil transport")
	}
	// Re-point the port: the test server is not on 443.
	tr.DialTLSContext = func(ctx context.Context, _ string, _ string) (net.Conn, error) {
		raw, err := (&net.Dialer{Timeout: timeout}).DialContext(ctx, "tcp", net.JoinHostPort(host, port))
		if err != nil {
			return nil, err
		}
		conn := tls.Client(raw, &tls.Config{ServerName: "example.com", RootCAs: roots})
		if err := conn.HandshakeContext(ctx); err != nil {
			raw.Close()
			return nil, err
		}
		return conn, nil
	}
	client := &http.Client{Transport: tr, Timeout: timeout}
	resp, err := client.Get("https://example.com/up")
	if err != nil {
		t.Fatalf("SNI dial through the server IP failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	// Wrong SNI must fail the handshake: the cert is not valid for it.
	tr2 := resolveTransport(host, "not-example.invalid", roots)
	tr2.DialTLSContext = func(ctx context.Context, _ string, _ string) (net.Conn, error) {
		raw, err := (&net.Dialer{Timeout: timeout}).DialContext(ctx, "tcp", net.JoinHostPort(host, port))
		if err != nil {
			return nil, err
		}
		conn := tls.Client(raw, &tls.Config{ServerName: "not-example.invalid", RootCAs: roots})
		if err := conn.HandshakeContext(ctx); err != nil {
			raw.Close()
			return nil, err
		}
		return conn, nil
	}
	if _, err := (&http.Client{Transport: tr2, Timeout: timeout}).Get("https://not-example.invalid/up"); err == nil {
		t.Error("wrong SNI must fail certificate validation")
	}
}
