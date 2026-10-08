// Package probe probes the public edge through kamal-proxy: the app health
// endpoint (/api/auth/ok) and the served build id (/build-id.txt), with the
// shell version's 8s timeout and tolerant degradation (curl -sf -m 8
// semantics, ported to net/http — no curl subprocess).
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
	healthPath = "/api/auth/ok"
	buildPath  = "/build-id.txt"
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

// LiveStatus GETs https://<host>/api/auth/ok and returns the body when the
// endpoint answers 2xx (curl -sf parity: any HTTP error or transport
// failure reads as unreachable).
func (p *Prober) LiveStatus(ctx context.Context, host string) (string, bool) {
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
