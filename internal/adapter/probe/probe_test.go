package probe

import (
	"context"
	"net/http"
	"net/http/httptest"
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
	srv, p := newTLSServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/auth/ok" {
			t.Errorf("probe path = %q, want /api/auth/ok", r.URL.Path)
		}
		w.Write([]byte(`{"ok":true,"git":"ccc3333"}`))
	})
	got, ok := p.LiveStatus(context.Background(), hostOf(t, srv))
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
	if _, ok := p.LiveStatus(context.Background(), hostOf(t, srv)); ok {
		t.Error("LiveStatus() on 502 must not be ok (curl -sf parity)")
	}
}

func TestLiveStatusFailsClosedOnUnreachable(t *testing.T) {
	p := &Prober{HTTPClient: &http.Client{}}
	if _, ok := p.LiveStatus(context.Background(), "127.0.0.1:1"); ok {
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
