// Package probe probes the public edge through kamal-proxy: the app health
// endpoint (/api/auth/ok) and the served build id (/build-id.txt), with the
// shell version's 8s timeout and tolerant degradation.
package probe
