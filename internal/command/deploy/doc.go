// Package deploy is the Go port of `kampodine deploy` (registry-free
// podman save | ssh podman load, sha-tagged images, health-gated kamal-proxy
// edge) plus the lifecycle subcommands list/prune/logs/restart/shell.
// NOT_YET_PORTED: scripts/deploy.sh + scripts/deploy-lifecycle.sh (kampodine
// HEAD) are the behavioral spec; the command-parity guard tracks porting via
// internal/parity/baseline.json.
package deploy
