// Package env is the Go port of `kampodine env` (remote app env file
// /etc/kampodine/env, 0600: list | push | pull | fingerprint — fingerprints
// only, values NEVER printed). NOT_YET_PORTED: scripts/env.sh (kampodine
// HEAD) is the behavioral spec; the command-parity guard tracks porting via
// internal/parity/baseline.json.
package env
