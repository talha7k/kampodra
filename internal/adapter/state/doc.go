// Package state owns local (deploy-machine) state: the append-only
// deployment ledger ~/.kampodine/deployments.jsonl — the deployment HISTORY.
// Lines are {ts, host, sha, tag, result, duration_ms, subject}; shas/tags/
// hosts/subjects only, NEVER secrets.
package state
