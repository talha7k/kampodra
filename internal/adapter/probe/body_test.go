package probe

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// BodyServesSha is the deploy health gate's served-sha verdict (the shell's
// `grep -qE "\"(git|build)\":\"$VER"` — the grep runs CLIENT-side on the
// fetched body; the pattern has NO closing quote, so it is a prefix match
// on the sha stamp).

func TestBodyServesShaMatchesGitStamp(t *testing.T) {
	body := `{"ok":true,"git":"abc1234","build":"abc1234"}`
	if !BodyServesSha(body, "abc1234") {
		t.Error("BodyServesSha() = false for a git-stamped body, want true")
	}
}

func TestBodyServesShaMatchesBuildStamp(t *testing.T) {
	body := `{"ok":true,"build":"abc1234"}`
	if !BodyServesSha(body, "abc1234") {
		t.Error("BodyServesSha() = false for a build-stamped body, want true")
	}
}

func TestBodyServesShaIsPrefixMatch(t *testing.T) {
	// The shell grep had no closing quote: a body serving the FULL sha
	// satisfies a sha-fragment gate (7-char tag vs 40-char stamp).
	if !BodyServesSha(`{"git":"abc1234deadbeef000000000000000000000000"}`, "abc1234") {
		t.Error("BodyServesSha() must prefix-match the fragment against the full sha")
	}
}

func TestBodyServesShaRejectsWrongSha(t *testing.T) {
	if BodyServesSha(`{"ok":true,"git":"bbb2222"}`, "abc1234") {
		t.Error("BodyServesSha() = true for a different served sha — the gate would pass a stale deploy")
	}
}

func TestBodyServesShaRejectsNonStampOccurrences(t *testing.T) {
	// The sha must sit in the git/build stamp position, not just anywhere.
	if BodyServesSha(`{"note":"watch out for abc1234 in prose"}`, "abc1234") {
		t.Error("BodyServesSha() matched a non-stamp occurrence")
	}
	if BodyServesSha("", "abc1234") {
		t.Error("BodyServesSha() matched an empty body")
	}
}

// Up probes the kamal-proxy /up convention (the public smoke's second leg).

func TestUpOK(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/up" {
			t.Errorf("Up path = %q, want /up", r.URL.Path)
		}
		w.Write([]byte("ok\n"))
	}))
	defer srv.Close()
	p := &Prober{HTTPClient: srv.Client()}
	if !p.Up(context.Background(), hostOf(t, srv)) {
		t.Error("Up() = false on a 200 /up, want true")
	}
}

func TestUpFailsOnHTTPError(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	p := &Prober{HTTPClient: srv.Client()}
	if p.Up(context.Background(), hostOf(t, srv)) {
		t.Error("Up() = true on a 503, want false (curl -fsS parity)")
	}
}

func TestUpFailsClosedOnUnreachable(t *testing.T) {
	p := &Prober{HTTPClient: &http.Client{}}
	if p.Up(context.Background(), "127.0.0.1:1") {
		t.Error("Up() on a closed port must not be ok")
	}
}
