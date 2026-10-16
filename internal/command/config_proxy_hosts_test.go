package command_test

import (
	"strings"
	"testing"
)

// The flat profile's proxyHosts list (the production edge serves several
// hostnames): config init sets it comma-separated, a re-init WITHOUT the
// flag KEEPS the existing list (the hand-extended list must never be
// silently dropped by a casual re-init — that is the exact incident class
// this field exists to fix), clone carries it, show renders it.

func TestConfigInitProxyHostsFlagPersistsList(t *testing.T) {
	deps, home, stdout, stderr := setupConfig(t)
	code := runConfig(t, deps, "init",
		"--name", "prod",
		"--host", "root@203.0.113.9",
		"--ssh-key", "/tmp/k",
		"--proxy-host", "edge.example.com",
		"--proxy-hosts", "edge.example.com,www.example.com,api.example.com")
	if code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, stderr.String())
	}
	p := loadConfigFile(t, home).Profiles["prod"]
	if strings.Join(p.ProxyHosts, ",") != "edge.example.com,www.example.com,api.example.com" {
		t.Errorf("proxyHosts = %v, want the comma-separated list", p.ProxyHosts)
	}

	stdout.Reset()
	if code := runConfig(t, deps, "show"); code != 0 {
		t.Fatalf("show exit = %d, stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "proxyHosts: edge.example.com,www.example.com,api.example.com") {
		t.Errorf("show must render the list:\n%s", stdout.String())
	}
}

func TestConfigReInitWithoutProxyHostsKeepsTheList(t *testing.T) {
	deps, home, _, stderr := setupConfig(t)
	runConfig(t, deps, "init", "--name", "prod", "--host", "root@203.0.113.9",
		"--ssh-key", "/tmp/k", "--proxy-hosts", "edge.example.com,www.example.com")

	// Casual re-init (no --proxy-hosts): the list SURVIVES.
	if code := runConfig(t, deps, "init", "--name", "prod", "--host", "root@203.0.113.9",
		"--ssh-key", "/tmp/k", "--force"); code != 0 {
		t.Fatalf("re-init exit = %d, stderr: %s", code, stderr.String())
	}
	if got := loadConfigFile(t, home).Profiles["prod"].ProxyHosts; strings.Join(got, ",") != "edge.example.com,www.example.com" {
		t.Errorf("proxyHosts = %v, want the existing list KEPT by a re-init without the flag", got)
	}

	// An explicit --proxy-hosts replaces the list.
	if code := runConfig(t, deps, "init", "--name", "prod", "--host", "root@203.0.113.9",
		"--ssh-key", "/tmp/k", "--proxy-hosts", "new.example.com", "--force"); code != 0 {
		t.Fatalf("re-init exit = %d, stderr: %s", code, stderr.String())
	}
	if got := loadConfigFile(t, home).Profiles["prod"].ProxyHosts; strings.Join(got, ",") != "new.example.com" {
		t.Errorf("proxyHosts = %v, want the explicit replacement", got)
	}
}

func TestConfigCloneCarriesProxyHosts(t *testing.T) {
	deps, home, _, stderr := setupConfig(t)
	runConfig(t, deps, "init", "--name", "src", "--host", "root@203.0.113.9",
		"--ssh-key", "/tmp/k", "--proxy-hosts", "edge.example.com,www.example.com")
	if code := runConfig(t, deps, "clone", "src", "dst", "--host", "root@203.0.113.8"); code != 0 {
		t.Fatalf("clone exit = %d, stderr: %s", code, stderr.String())
	}
	got := loadConfigFile(t, home).Profiles["dst"].ProxyHosts
	if strings.Join(got, ",") != "edge.example.com,www.example.com" {
		t.Errorf("cloned proxyHosts = %v, want the src list carried over", got)
	}
}

func TestConfigInitRejectsMalformedProxyHosts(t *testing.T) {
	for _, bad := range []string{"a.example.com,,c.example.com", "a.example.com,has space"} {
		deps, _, _, stderr := setupConfig(t)
		code := runConfig(t, deps, "init", "--name", "prod", "--host", "root@203.0.113.9",
			"--ssh-key", "/tmp/k", "--proxy-hosts", bad)
		if code != 1 {
			t.Errorf("--proxy-hosts %q: exit = %d, want 1", bad, code)
		}
		if !strings.Contains(stderr.String(), "--proxy-hosts") {
			t.Errorf("--proxy-hosts %q: stderr must name the flag: %s", bad, stderr.String())
		}
		if strings.Contains(stderr.String(), bad) {
			t.Errorf("--proxy-hosts %q: stderr must not echo the rejected value: %s", bad, stderr.String())
		}
	}
}
