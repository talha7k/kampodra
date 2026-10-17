package command

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/talha7k/kampodra/internal/adapter/project"
	"github.com/talha7k/kampodra/internal/adapter/transport"
	"github.com/talha7k/kampodra/internal/adapter/vmbootstrap"
)

// The readiness loops must GIVE UP (fail closed) after the configured
// attempts — with a shrunken policy so no test ever waits real seconds.

type failingRunner struct{ calls int }

func (f *failingRunner) Run(context.Context, transport.HostSpec, string) (string, error) {
	f.calls++
	return "", errors.New("unreachable")
}
func (f *failingRunner) RunWithStdin(context.Context, transport.HostSpec, string, io.Reader) (string, error) {
	return "", errors.New("unreachable")
}
func (f *failingRunner) Stream(context.Context, []string) (int, error) {
	return 1, errors.New("unreachable")
}

func TestWaitForSSHGivesUpAfterAttempts(t *testing.T) {
	old := vmPrepareSSHWait
	vmPrepareSSHWait = waitPolicy{attempts: 3, interval: time.Millisecond}
	defer func() { vmPrepareSSHWait = old }()

	runner := &failingRunner{}
	err := waitForSSH(context.Background(), runner, transport.HostSpec{Host: "root@h"}, vmPrepareSSHWait)
	if err == nil || !strings.Contains(err.Error(), "no ssh after") {
		t.Fatalf("waitForSSH err = %v, want the no-ssh-after failure", err)
	}
	if runner.calls != 3 {
		t.Errorf("attempts = %d, want 3", runner.calls)
	}
}

func TestWaitUntilStopsOnSuccess(t *testing.T) {
	old := vmPrepareProxyWait
	vmPrepareProxyWait = waitPolicy{attempts: 5, interval: time.Millisecond}
	defer func() { vmPrepareProxyWait = old }()

	runner := &flakyRunner{failFirst: 2}
	if err := waitUntil(context.Background(), runner, transport.HostSpec{Host: "root@h"},
		vmPrepareProxyWait, "probe", "never"); err != nil {
		t.Fatalf("waitUntil err = %v", err)
	}
	if runner.calls != 3 {
		t.Errorf("calls = %d, want 3 (two failures then success)", runner.calls)
	}
}

type flakyRunner struct {
	failFirst int
	calls     int
}

func (f *flakyRunner) Run(context.Context, transport.HostSpec, string) (string, error) {
	f.calls++
	if f.calls <= f.failFirst {
		return "", errors.New("not yet")
	}
	return "ok", nil
}
func (f *flakyRunner) RunWithStdin(context.Context, transport.HostSpec, string, io.Reader) (string, error) {
	return "", nil
}
func (f *flakyRunner) Stream(context.Context, []string) (int, error) { return 0, nil }

// namingFromProject renders the unit's bind mounts: the pinned block's
// mount + exec override, plus EVERY static block's mount + env — a static
// SPA dist shares the unit with the exec-kind backend.
func TestNamingFromProjectStaticBlock(t *testing.T) {
	apiGo := project.ManifestBinary{Kind: project.BinaryKindGo, Dir: "/data/app", Entry: "esellar-api-go"}
	web := project.ManifestBinary{Kind: project.BinaryKindStatic, Dir: "/data/web", Env: "WEB_DIST_DIR"}

	n := namingFromProject(project.Config{}, &apiGo, []project.ManifestBinary{web}, nil)

	if n.BinaryMount != "-v /data/app:/data/app" || n.BinaryEntrypointFlag != "--entrypoint /data/app/esellar-api-go" {
		t.Errorf("the pinned exec-kind block must keep its mount+override: %+v", n)
	}
	if len(n.StaticMounts) != 1 || n.StaticMounts[0] != "-v /data/web:/data/web" {
		t.Errorf("StaticMounts = %v, want the web mount", n.StaticMounts)
	}
	if len(n.StaticEnvs) != 1 || n.StaticEnvs[0] != "-e WEB_DIST_DIR=/data/web/current" {
		t.Errorf("StaticEnvs = %v, want the env passthrough", n.StaticEnvs)
	}
}

// When the PINNED block is itself static, its mount renders once (via
// BinaryMount) and the static sweep must not duplicate it — but the env
// still renders exactly once.
func TestNamingFromProjectPinnedStaticNoDoubleMount(t *testing.T) {
	web := project.ManifestBinary{Kind: project.BinaryKindStatic, Dir: "/data/web", Env: "WEB_DIST_DIR"}

	n := namingFromProject(project.Config{}, &web, []project.ManifestBinary{web}, nil)

	if n.BinaryMount != "-v /data/web:/data/web" {
		t.Errorf("BinaryMount = %q", n.BinaryMount)
	}
	if n.BinaryEntrypointFlag != "" || n.BinaryCmdOverride != "" {
		t.Errorf("a static block must render no exec override: %+v", n)
	}
	if len(n.StaticMounts) != 0 {
		t.Errorf("StaticMounts = %v — the pinned mount must not duplicate", n.StaticMounts)
	}
	if len(n.StaticEnvs) != 1 || n.StaticEnvs[0] != "-e WEB_DIST_DIR=/data/web/current" {
		t.Errorf("StaticEnvs = %v, want exactly the one env", n.StaticEnvs)
	}
}

// serveWeb=false: the estate serves NO web SPA (web on Vercel, the VM
// strictly api) — every static-kind block drops out of the unit render:
// no -v dist mount, no WEB_DIST_DIR -e, no releases layout. The pinned
// exec-kind backend keeps its mount + override.
func TestNamingFromProjectServeWebFalseDropsStatics(t *testing.T) {
	apiGo := project.ManifestBinary{Kind: project.BinaryKindGo, Dir: "/data/app", Entry: "esellar-api-go"}
	web := project.ManifestBinary{Kind: project.BinaryKindStatic, Dir: "/data/web", Env: "WEB_DIST_DIR"}
	pj := project.Config{ServeWeb: boolPtr(false)}

	n := namingFromProject(pj, &apiGo, []project.ManifestBinary{web}, nil)

	if n.BinaryMount != "-v /data/app:/data/app" || n.BinaryEntrypointFlag != "--entrypoint /data/app/esellar-api-go" {
		t.Errorf("the pinned exec-kind block must survive serveWeb=false: %+v", n)
	}
	if len(n.StaticMounts) != 0 {
		t.Errorf("StaticMounts = %v — the web mount must drop (serveWeb=false)", n.StaticMounts)
	}
	if len(n.StaticEnvs) != 0 {
		t.Errorf("StaticEnvs = %v — WEB_DIST_DIR must drop (serveWeb=false)", n.StaticEnvs)
	}
	if n.StaticReleases {
		t.Error("StaticReleases = true — the releases layout must drop (serveWeb=false)")
	}
}

// serveWeb=false also drops a PINNED static block (pinning the SPA as the
// unit's binary contradicts a no-web estate) — the unit serves only the
// image/binary entry then.
func TestNamingFromProjectServeWebFalseDropsPinnedStatic(t *testing.T) {
	web := project.ManifestBinary{Kind: project.BinaryKindStatic, Dir: "/data/web", Env: "WEB_DIST_DIR"}

	n := namingFromProject(project.Config{ServeWeb: boolPtr(false)}, &web, []project.ManifestBinary{web}, nil)

	if n.BinaryMount != "" || n.BinaryDir != "" || n.StaticReleases {
		t.Errorf("a pinned static block must fully drop (serveWeb=false): %+v", n)
	}
	if len(n.StaticEnvs) != 0 || len(n.StaticMounts) != 0 {
		t.Errorf("statics must not survive (serveWeb=false): mounts=%v envs=%v", n.StaticMounts, n.StaticEnvs)
	}
}

// The render shapes, end-to-end: the SAME naming pipeline feeding the real
// unit renderers. serveWeb=true (the default) renders the web statics;
// serveWeb=false renders a unit with NO /data/web trace at all.
func TestUnitRenderServeWebShapes(t *testing.T) {
	apiGo := project.ManifestBinary{Kind: project.BinaryKindGo, Dir: "/data/app", Entry: "esellar-api-go"}
	web := project.ManifestBinary{Kind: project.BinaryKindStatic, Dir: "/data/web", Env: "WEB_DIST_DIR"}

	webOn := vmbootstrap.RenderInitDAPI(namingFromProject(project.Config{}, &apiGo, []project.ManifestBinary{web}, nil))
	for _, want := range []string{"-v /data/web:/data/web", "-e WEB_DIST_DIR=/data/web/current"} {
		if !strings.Contains(webOn, want) {
			t.Errorf("the default (serveWeb=true) unit must render %q:\n%s", want, webOn)
		}
	}

	webOff := vmbootstrap.RenderInitDAPI(namingFromProject(project.Config{ServeWeb: boolPtr(false)}, &apiGo, []project.ManifestBinary{web}, nil))
	for _, banned := range []string{"/data/web", "WEB_DIST_DIR"} {
		if strings.Contains(webOff, banned) {
			t.Errorf("the serveWeb=false unit must carry no %q trace:\n%s", banned, webOff)
		}
	}
	if !strings.Contains(webOff, "-v /data/app:/data/app") {
		t.Errorf("the serveWeb=false unit must keep the backend mount:\n%s", webOff)
	}
}

func boolPtr(b bool) *bool { return &b }
