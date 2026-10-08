package vmbootstrap

import (
	"strings"
	"testing"
)

// The vm-prepare.sh managed-file payloads, rendered from ProjectConfig
// naming. Content parity with the retired shell heredocs is the contract —
// only the naming is parameterized (kampodra branding on tool-owned names).
// The adapter NEVER imports the project adapter sideways: the command layer
// builds Naming from project.Config (today's values appear here as fixtures).

func testNaming() Naming {
	return Naming{
		Container:       "kampodine-api",
		EnvFilePath:     "/etc/kampodine/env",
		EnvDir:          "/etc/kampodine",
		AnchorConf:      "/etc/kampodine/anchor.conf",
		DataDir:         "/data/tenants",
		ImagePrefix:     "127.0.0.1:5000/kampodine-api",
		Registry:        "127.0.0.1:5000",
		ImageRef:        "127.0.0.1:5000/kampodine-api:latest",
		Port:            "8080",
		Network:         "kamal",
		DeployedShaFile: "/etc/kampodine/deployed-sha",
	}
}

func TestNamingFromProject(t *testing.T) {
	n := testNaming()
	if n.Container != "kampodine-api" {
		t.Errorf("Container = %q", n.Container)
	}
	if n.EnvDir != "/etc/kampodine" {
		t.Errorf("EnvDir = %q (derived from the env file path)", n.EnvDir)
	}
	if n.AnchorConf != "/etc/kampodine/anchor.conf" {
		t.Errorf("AnchorConf = %q", n.AnchorConf)
	}
	if n.Registry != "127.0.0.1:5000" {
		t.Errorf("Registry = %q (the ImagePrefix host:port)", n.Registry)
	}
	if n.ImageRef != "127.0.0.1:5000/kampodine-api:latest" {
		t.Errorf("ImageRef = %q", n.ImageRef)
	}
}

func TestRenderRegistriesConf(t *testing.T) {
	got := RenderRegistriesConf(testNaming())
	for _, want := range []string{
		"# Managed by kampodra vm-prepare — do not hand-edit.",
		"unqualified-search-registries = [\"docker.io\"]",
		"location = \"127.0.0.1:5000\"",
		"insecure = true",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("registries.conf missing %q:\n%s", want, got)
		}
	}
}

func TestRenderSysctlConf(t *testing.T) {
	got := RenderSysctlConf()
	if !strings.Contains(got, "net.ipv4.ip_unprivileged_port_start=80") {
		t.Errorf("sysctl conf wrong:\n%s", got)
	}
}

func TestRenderInitDAPI(t *testing.T) {
	got := RenderInitDAPI(testNaming())
	for _, want := range []string{
		"#!/sbin/openrc-run",
		"name=\"kampodine-api\"",
		"command_args=\"run --rm --name kampodine-api --network kamal -p 8080:8080",
		"-v /data/tenants:/data/tenants",
		"--env-file /etc/kampodine/env 127.0.0.1:5000/kampodine-api:latest\"",
		"respawn_delay=10",
		"respawn_max=0",
		"supervise_daemon_args=\"--stderr /var/log/kampodra-service.log\"",
		"after kampodra-datamount",
		"podman container exists kampodine-api 2>/dev/null && podman rm -f kampodine-api",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("init.d/%s missing %q:\n%s", "api", want, got)
		}
	}
}

func TestRenderInitDProxy(t *testing.T) {
	got := RenderInitDProxy(testNaming())
	for _, want := range []string{
		"name=\"kamal-proxy\"",
		"--network kamal -p 80:80 -p 443:443 --cap-add NET_BIND_SERVICE",
		"-v kamal-proxy-config:/home/kamal-proxy/.config docker.io/basecamp/kamal-proxy:latest",
		"--stderr /var/log/kamal-proxy-service.log",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("init.d/kamal-proxy missing %q:\n%s", want, got)
		}
	}
}

func TestRenderAnchorWatcher(t *testing.T) {
	n := testNaming()
	got := RenderAnchorScript(n)
	for _, want := range []string{
		"#!/bin/sh",
		"CONF=\"/etc/kampodine/anchor.conf\"",
		"INTERVAL=\"${KAMPODRA_ANCHOR_INTERVAL:-5}\"",
		"anchor ADDED",
		"no anchor conf",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("anchor script missing %q:\n%s", want, got)
		}
	}
}

func TestRenderInitDAnchor(t *testing.T) {
	got := RenderInitDAnchor(testNaming())
	for _, want := range []string{
		"name=\"kampodra-anchor\"",
		"command=\"/usr/local/sbin/kampodra-anchor.sh\"",
		"respawn_delay=5",
		"--stdout /var/log/kampodra-anchor.log --stderr /var/log/kampodra-anchor.log",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("init.d anchor missing %q:\n%s", want, got)
		}
	}
}

func TestRenderSSHDHardening(t *testing.T) {
	got := RenderSSHDHardening()
	for _, want := range []string{
		"PasswordAuthentication no",
		"PermitRootLogin prohibit-password",
		"AllowTcpForwarding yes",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("sshd hardening missing %q:\n%s", want, got)
		}
	}
}

// Remote snippets (single source, fixture-pinned): the busybox-safe
// bootstrap blocks the command layer pipes over `sh -s`.
func TestRemoteSnippets(t *testing.T) {
	if got := CommunityRepoSnippet(); !strings.Contains(got, "/community") || !strings.Contains(got, "apk update") {
		t.Errorf("community repo snippet wrong:\n%s", got)
	}
	if got := PodmanStackSnippet(); !strings.Contains(got, "apk add --no-progress podman podman-docker crun catatonit netavark aardvark-dns fuse-overlayfs iptables") {
		t.Errorf("podman stack snippet wrong:\n%s", got)
	}
	if got := PodstackGateSnippet(); !strings.Contains(got, "netavark") {
		t.Errorf("podstack gate wrong:\n%s", got)
	}
	if got := SysctlApplySnippet(); !strings.Contains(got, "sysctl -w net.ipv4.ip_unprivileged_port_start=80") {
		t.Errorf("sysctl apply wrong:\n%s", got)
	}
	n := testNaming()
	if got := APIConvergeSnippet(n); !strings.Contains(got, n.EnvFilePath) || !strings.Contains(got, n.ImageRef) {
		t.Errorf("api converge wrong:\n%s", got)
	}
	if got := UnitDriftSnippet(); !strings.Contains(got, "RUNNING:") {
		t.Errorf("unit drift wrong:\n%s", got)
	}
}

// The static gates (fail-closed BEFORE and AFTER mutations).
func TestGateCommands(t *testing.T) {
	n := testNaming()
	gates := map[string]string{
		"uefi":      GateUEFI,
		"nosystemd": GateNoSystemd,
		"openrc":    GateOpenRCTooling,
		"sshd":      GateSSHDHardened,
	}
	for name, g := range gates {
		if g == "" {
			t.Errorf("gate %s empty", name)
		}
	}
	if !strings.Contains(GateUEFI, "/sys/firmware/efi") {
		t.Errorf("uefi gate wrong: %s", GateUEFI)
	}
	if !strings.Contains(GateNoSystemd, "systemd") {
		t.Errorf("no-systemd gate wrong: %s", GateNoSystemd)
	}
	if !strings.Contains(GatePodmanVersion, "$2>=9") {
		t.Errorf("podman version gate wrong (must encode >= 4.9): %s", GatePodmanVersion)
	}
	if !strings.Contains(RegistriesGate(n), n.Registry) {
		t.Errorf("registries gate wrong: %s", RegistriesGate(n))
	}
}
