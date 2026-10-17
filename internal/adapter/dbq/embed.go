package dbq

import (
	_ "embed"
)

// The sqlq helper's standalone build unit travels INSIDE the kampodra
// binary: the source (which is also the live package ./sqlq in this
// module — one source of truth, tested by `go test ./internal/...`) plus
// the module manifests the on-demand build needs. Never a committed
// binary blob — the helper compiles from these bytes at first use.

//go:embed sqlq/main.go
var SQLQSource string

// gateSource is the stdlib-only gate, embedded VERBATIM into the
// materialized helper module — the same bytes both sides enforce.
//
//go:embed gate.go
var gateSource []byte

// The manifests live as .tmpl because a directory holding a real go.mod
// is a separate module to the go tool — go:embed refuses files across
// that boundary. MaterializeHelperModule writes them under their real
// names into the build dir.

//go:embed helpermod/go.mod.tmpl
var helperGoMod []byte

//go:embed helpermod/go.sum.tmpl
var helperGoSum []byte
