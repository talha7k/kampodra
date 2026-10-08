// Package probe probes the public edge through kamal-proxy: the app health
// endpoint (project config HealthPath) and the served build id
// (/build-id.txt — the kamal-proxy convention), with the shell version's
// 8s timeout and tolerant degradation (curl -sf -m 8 semantics, ported to
// net/http — no curl subprocess). The health path is project config passed
// in by the command layer — never a constant here.
package probe

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	buildPath = "/build-id.txt"
	upPath    = "/up"
	// timeout is curl -m 8 parity.
	timeout = 8 * time.Second
)

// Prober probes the edge. HTTPClient is injectable for tests; nil uses a
// default client with the shell's 8s timeout.
type Prober struct {
	HTTPClient *http.Client
}

func (p *Prober) client() *http.Client {
	if p.HTTPClient != nil {
		return p.HTTPClient
	}
	p.HTTPClient = &http.Client{Timeout: timeout}
	return p.HTTPClient
}

// LiveStatus GETs https://<host><healthPath> and returns the body when the
// endpoint answers 2xx (curl -sf parity: any HTTP error or transport
// failure reads as unreachable).
func (p *Prober) LiveStatus(ctx context.Context, host, healthPath string) (string, bool) {
	body, err := p.fetch(ctx, "https://"+host+healthPath)
	if err != nil {
		return "", false
	}
	return body, true
}

// BuildID GETs https://<host>/build-id.txt; ok=false lets the caller print
// the tolerant "unreachable" (shell parity).
func (p *Prober) BuildID(ctx context.Context, host string) (string, bool) {
	body, err := p.fetch(ctx, "https://"+host+buildPath)
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(body), true
}

// Up probes the kamal-proxy /up convention (the public smoke's second leg,
// the shell's `curl -fsS https://<host>/up`): ok on a 2xx answer.
func (p *Prober) Up(ctx context.Context, host string) bool {
	_, err := p.fetch(ctx, "https://"+host+upPath)
	return err == nil
}

// BodyServesSha is the deploy health gate's served-sha verdict — the shell's
// client-side `grep -qE "\"(git|build)\":\"$VER"` on the fetched body. The
// grep pattern has NO closing quote after the sha, so this is a PREFIX
// match: a body serving the full 40-char stamp satisfies a 7-char tag gate.
// The sha must sit in the git/build stamp position — prose occurrences
// never count.
func BodyServesSha(body, sha string) bool {
	if sha == "" {
		return false
	}
	return strings.Contains(body, `"git":"`+sha) ||
		strings.Contains(body, `"build":"`+sha)
}

func (p *Prober) fetch(ctx context.Context, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := p.client().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", fmt.Errorf("%s: HTTP %d", url, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	return string(body), nil
}
