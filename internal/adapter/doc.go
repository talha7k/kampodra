// Package adapter hosts kampodra's outbound ports and their adapters.
//
// Strict layering (enforced by internal/arch/arch_test.go):
//
//	cmd/kampodra        thin entrypoint; imports internal/command only
//	internal/command     cobra command tree; may import internal/adapter/*
//	internal/adapter/*   transport, runtime, init, probe, osfacts, cloud,
//	                     state; may import stdlib + third-party libs ONLY —
//	                     never internal/command, never each other sideways
//
// Each subpackage owns one boundary of the outside world:
//
//	transport — ssh (os/exec of the ssh binary; same host/key resolution
//	            ladder as the shell version: --ssh-key > KAMPODRA_SSH_KEY >
//	            agent/ssh-config; --host > KAMPODRA_HOST > ESPELLAR_HOST
//	            legacy)
//	runtime   — podman containers/images on the VM
//	init      — service supervision (OpenRC detection, service states)
//	probe     — live edge health (kamal-proxy the app health endpoint, build-id.txt)
//	osfacts   — remote OS facts readable over ssh (df -P disk usage, …)
//	cloud     — OCI CLI surface (bluegreen, dns, image-import)
//	state     — local state (the append-only deployments ledger)
package adapter
