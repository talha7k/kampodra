// Command kampodine is the registry-free deploy CLI (podman save | ssh
// podman load, sha-tagged images, health-gated kamal-proxy edge) — the Go
// port of the npm `kampodine` shell CLI. Thin entrypoint: everything lives
// in internal/command and internal/adapter.
package main

import (
	"os"

	"github.com/talha7k/kampodine-go/internal/command"
)

// version is stamped at release time via -ldflags "-X main.version=…"; the
// npm package.json version is pinned to the same value.
var version = "0.7.0-alpha.1"

func main() {
	os.Exit(command.Execute(version, command.Deps{}, os.Args[1:]))
}
