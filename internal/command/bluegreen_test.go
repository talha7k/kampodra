package command_test

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/talha7k/kampodra/internal/adapter/probe"
	"github.com/talha7k/kampodra/internal/command"
)

// The bluegreen suite's shared harness: PATH-shimmed oci + ssh, fixture-fed
// OCI JSON, and the status probe round-tripper. Per-area tests:
// bluegreen_status_test.go, bluegreen_provision_test.go,
// bluegreen_flip_test.go, bluegreen_guest_test.go.

type bgHarness struct {
	deps   command.Deps
	stdout *bytes.Buffer
	stderr *bytes.Buffer
	ociLog string
	sshLog string
}

func (h *bgHarness) run(t *testing.T, args ...string) int {
	t.Helper()
	return command.Execute("test", h.deps, append([]string{"bluegreen"}, args...))
}

func (h *bgHarness) ociCalls(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(h.ociLog)
	if err != nil {
		return nil
	}
	var out []string
	for _, l := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

func (h *bgHarness) sshCalls(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(h.sshLog)
	if err != nil {
		return nil
	}
	var out []string
	for _, l := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

// bgRoundTripper answers flip HTTPS verifies: 200 + the served body.
type bgRoundTripper struct {
	body string
}

func (b bgRoundTripper) RoundTrip(_ *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: 200,
		Body:       io.NopCloser(strings.NewReader(b.body)),
		Header:     make(http.Header),
	}, nil
}

func greenRow() string {
	return `{"data":[{"id":"ocid1.instance.green1","availability-domain":"AD-1","time-created":"2026-10-01T00:00:00Z"}]}`
}

func blueRow() string {
	return `{"data":[{"id":"ocid1.instance.blue1","availability-domain":"AD-1","time-created":"2026-09-01T00:00:00Z"}]}`
}

func setupBG(t *testing.T, ociFixtures map[string]string, sshEnv map[string]string) *bgHarness {
	t.Helper()
	home := t.TempDir()
	stub := t.TempDir()
	ociLog := filepath.Join(stub, "oci-calls.log")
	sshLog := filepath.Join(stub, "ssh-calls.log")

	def := func(key, val string) {
		if _, ok := ociFixtures[key]; !ok {
			ociFixtures[key] = val
		}
	}
	def("compartments", `{"data":[{"id":"ocid1.compartment.test","name":"test-compartment"}]}`)
	def("reserved", `ocid1.publicip.r1 203.0.113.50 -`)
	def("instances-blue", `{"data":[]}`)
	def("instances-green", `{"data":[]}`)
	if _, ok := ociFixtures["instances-green-later"]; !ok {
		ociFixtures["instances-green-later"] = ociFixtures["instances-green"]
	}
	def("instance", `{"data":{"lifecycle-state":"RUNNING","image-id":"ocid1.image.platform","availability-domain":"AD-1"}}`)
	def("vnic", `{"data":[{"vnic-id":"ocid1.vnic.v1","subnet-id":"ocid1.subnet.s1"}]}`)
	def("vnicGet", `{"data":{"public-ip":"198.51.100.7"}}`)
	def("privateIPs", `{"data":[{"id":"ocid1.privateip.primary","is-primary":true},{"id":"ocid1.privateip.anchor","is-primary":false}]}`)
	def("privateGet", `{"data":{"ip-address":"10.0.0.7"}}`)
	def("subnet", `{"data":{"cidr-block":"10.0.0.0/24"}}`)
	def("images", `{"data":[]}`)
	def("imageGet", `{"data":{"launch-options":{"firmware":"UEFI_64"}}}`)

	files := map[string]string{}
	for k, v := range ociFixtures {
		p := filepath.Join(stub, k+".json")
		os.WriteFile(p, []byte(v), 0o600)
		files[k] = p
	}

	ociShim := "#!/bin/bash\n" +
		"printf '%s\\n' \"$*\" >> '" + ociLog + "'\n" +
		"case \"$1 $2\" in\n" +
		"  \"iam compartment\") cat '" + files["compartments"] + "'; exit 0 ;;\n" +
		"  \"network public-ip\") if [[ \"$3\" == list ]]; then cat '" + files["reserved"] + "'; elif [[ \"$3\" == create ]]; then printf '{\"data\":{\"ip-address\":\"203.0.113.50\"}}'; fi; exit 0 ;;\n" +
		"  \"compute instance\") if [[ \"$3\" == list ]]; then " +
		"if [[ \"$*\" == *\"app-blue\"* ]]; then cat '" + files["instances-blue"] + "'; " +
		"elif grep -q \"app-green\" '" + ociLog + "' 2>/dev/null && [ \"$(grep -c \"app-green\" '" + ociLog + "')\" -gt 1 ]; then cat '" + files["instances-green-later"] + "'; " +
		"else cat '" + files["instances-green"] + "'; fi; exit 0; " +
		"elif [[ \"$3\" == launch ]]; then printf 'ocid1.instance.new-1'; else cat '" + files["instance"] + "'; fi; exit 0 ;;\n" +
		"  \"compute vnic-attachment\") cat '" + files["vnic"] + "'; exit 0 ;;\n" +
		"  \"compute image\") if [[ \"$3\" == list ]]; then cat '" + files["images"] + "'; elif [[ \"$3\" == import ]]; then printf '{\"data\":{\"id\":\"ocid1.image.imp\"}}'; else cat '" + files["imageGet"] + "'; fi; exit 0 ;;\n" +
		"  \"network vnic\") cat '" + files["vnicGet"] + "'; exit 0 ;;\n" +
		"  \"network private-ip\") if [[ \"$3\" == list ]]; then cat '" + files["privateIPs"] + "'; elif [[ \"$3\" == create ]]; then printf '{\"data\":{\"id\":\"ocid1.privateip.new\"}}'; else cat '" + files["privateGet"] + "'; fi; exit 0 ;;\n" +
		"  \"network subnet\") cat '" + files["subnet"] + "'; exit 0 ;;\n" +
		"  \"os ns\") printf '{\"data\":\"test-ns\"}'; exit 0 ;;\n" +
		"  \"os object\") exit 0 ;;\n" +
		"esac\n" +
		"exit 0\n"
	os.WriteFile(filepath.Join(stub, "oci"), []byte(ociShim), 0o755)

	envVal := func(k, def string) string {
		if v, ok := sshEnv[k]; ok {
			return v
		}
		return def
	}
	sshShim := "#!/bin/bash\n" +
		"printf '%s\\n' \"$*\" >> '" + sshLog + "'\n" +
		"remote=\"${*: -1}\"\n" +
		"case \"$remote\" in\n" +
		"  *\"cat /etc/alpine-release\"*) printf '" + envVal("ALPINE", "3.22.6") + "'; exit 0 ;;\n" +
		"  *\"cat /etc/os-release\"*) printf 'NAME=\"Ubuntu\"\\nID=ubuntu\\nVERSION_ID=\"24.04\"\\n'; exit 0 ;;\n" +
		"  *\"cat /etc/kampodra/deployed-sha\"*) printf '" + envVal("SHA", "abc1234") + "'; exit 0 ;;\n" +
		"  *\"grep -h\"*) printf '" + envVal("CONFADDR", "") + "'; exit 0 ;;\n" +
		"  *\"rc-service app status\"*) exit " + envVal("HEALTHY", "0") + " ;;\n" +
		"  *wget*) exit " + envVal("HEALTHY", "0") + " ;;\n" +
		"  *\"ANCHOR_CONF_WRITTEN\"*|*\"anchor.conf\"*printf*) printf 'ANCHOR_CONF_WRITTEN'; exit 0 ;;\n" +
		"  *\"ip -4 addr show | grep\"*) exit " + envVal("ADDR_READY", "0") + " ;;\n" +
		"  *\"kamal-proxy deploy\"*) exit " + envVal("ACME", "0") + " ;;\n" +
		"  *\"lsblk\"*) printf 'sda'; exit 0 ;;\n" +
		"  *\"findmnt\"*) printf '/dev/sda1'; exit 0 ;;\n" +
		"  *\"gunzip\"*|*\"reboot\"*|*\"ip addr del\"*|*\"rm -f\"*) exit 0 ;;\n" +
		"  *\"true\"*) exit 0 ;;\n" +
		"esac\n" +
		"exit 0\n"
	os.WriteFile(filepath.Join(stub, "ssh"), []byte(sshShim), 0o755)

	t.Setenv("PATH", stub+":/usr/bin:/bin")
	t.Setenv("OCI_COMPARTMENT", "test-compartment")
	for _, k := range []string{"KAMPODRA_PROFILE", "OCI_PROFILE"} {
		t.Setenv(k, "")
	}

	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	return &bgHarness{
		deps: command.Deps{
			Home:   home,
			Env:    systemLookup,
			Stdout: stdout,
			Stderr: stderr,
			Stdin:  strings.NewReader(""),
			Prober: &probe.Prober{HTTPClient: &http.Client{Transport: bgRoundTripper{body: `{"git":"abc1234"}`}}},
		},
		stdout: stdout, stderr: stderr, ociLog: ociLog, sshLog: sshLog,
	}
}
