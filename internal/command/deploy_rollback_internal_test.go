package command

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"context"
	"fmt"
	"io"

	"github.com/talha7k/kampodra/internal/adapter/probe"
	"github.com/talha7k/kampodra/internal/adapter/project"
	"github.com/talha7k/kampodra/internal/adapter/transport"
)

// runnerForStamp: a Runner stub whose only answered command is the
// deployed-sha stamp read (the current-release exclusion input).
func runnerForStamp(sha string) transport.Runner {
	return stampRunner{sha: sha}
}

type stampRunner struct{ sha string }

func (s stampRunner) Run(_ context.Context, _ transport.HostSpec, cmd string) (string, error) {
	if strings.Contains(cmd, "deployed-sha") {
		return s.sha + "\n", nil
	}
	return "", fmt.Errorf("unexpected command: %s", cmd)
}

func (s stampRunner) RunWithStdin(_ context.Context, _ transport.HostSpec, _ string, _ io.Reader) (string, error) {
	return "", fmt.Errorf("unexpected stdin command")
}

func (s stampRunner) Stream(_ context.Context, _ []string) (int, error) {
	return 0, fmt.Errorf("unexpected stream")
}

// projectConfigWithStamp builds the minimal manifest the resolver reads.
func projectConfigWithStamp(stamp string) project.Config {
	pc := project.LoadDefault()
	pc.DeployedShaFile = stamp
	return pc
}

// local stand-ins: systemLookup/deadTransport live in the external
// package (command_test); the internal test defines its own.
func lookupEmpty(string) (string, bool) { return "", false }

type deadTransportLocal struct{}

func (deadTransportLocal) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("dead")
}

// F13: bare --rollback targets the last SUCCESSFUL deployment from the
// ledger (excluding what's serving) — never a failed attempt's streamed
// image, never the current release itself.
func TestResolveRollbackShaPrefersLastSuccess(t *testing.T) {
	home := t.TempDir()
	ledger := filepath.Join(home, ".kampodra", "deployments.jsonl")
	if err := os.MkdirAll(filepath.Dir(ledger), 0o755); err != nil {
		t.Fatal(err)
	}
	// oldest → newest: a success, a FAILED stream, the current success
	lines := strings.Join([]string{
		`{"ts":"t1","host":"prod","sha":"2cf0687","result":"success"}`,
		`{"ts":"t2","host":"prod","sha":"772f0ce","result":"failed"}`,
		`{"ts":"t3","host":"prod","sha":"1a1833c","result":"success"}`,
		`{"ts":"t4","host":"prod","sha":"1a1833c","result":"failed"}`,
	}, "\n") + "\n"
	if err := os.WriteFile(ledger, []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}

	stdout := &bytes.Buffer{}
	d := Deps{
		Home:   home,
		Stdout: stdout,
		Stderr: &bytes.Buffer{},
		Stdin:  strings.NewReader(""),
		Env:    lookupEmpty,
		Prober: &probe.Prober{HTTPClient: &http.Client{Transport: deadTransportLocal{}}},
		Runner: runnerForStamp("1a1833c"),
	}
	target := Target{
		HostSpec:    transport.HostSpec{Host: "root@vm"},
		ProfileName: "prod",
		Project:     projectConfigWithStamp("/etc/kampodra/deployed-sha"),
	}
	sha, source, err := resolveRollbackSha(d, nil, target, "")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if sha != "2cf0687" {
		t.Errorf("rollback target = %q, want the last success excluding current (2cf0687)", sha)
	}
	if source != "ledger" {
		t.Errorf("source = %q, want ledger", source)
	}
	if !strings.Contains(stdout.String(), "last successful deployment") {
		t.Errorf("provenance must name the ledger selection:\n%s", stdout.String())
	}
}
