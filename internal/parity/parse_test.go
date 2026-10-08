package parity

import (
	"reflect"
	"testing"
)

// The parser derives the command surface from the shell repo's COMMITTED
// files only (cli.js dispatch + scripts' case statements + header usage
// lines). Tests use faithful miniature fixtures of those file shapes.

func TestParseCommandsJS(t *testing.T) {
	tests := []struct {
		name    string
		src     string
		want    []CommandRef
		wantErr bool
	}{
		{
			name: "committed cli.js dispatch shape (v0.5 HEAD)",
			src: `#!/usr/bin/env node
const commands = {
  deploy: "deploy.sh",
  status: "status.sh",
  bluegreen: "bluegreen.sh",
  "vm-prepare": "vm-prepare.sh",
  "image-import": "image-import.sh",
  migrate: "migrate.sh",
  env: "env.sh",
  dns: "dns.sh",
};
`,
			want: []CommandRef{
				{Name: "deploy", Script: "deploy.sh"},
				{Name: "status", Script: "status.sh"},
				{Name: "bluegreen", Script: "bluegreen.sh"},
				{Name: "vm-prepare", Script: "vm-prepare.sh"},
				{Name: "image-import", Script: "image-import.sh"},
				{Name: "migrate", Script: "migrate.sh"},
				{Name: "env", Script: "env.sh"},
				{Name: "dns", Script: "dns.sh"},
			},
		},
		{
			name:    "no commands block fails loudly",
			src:     "const x = 1;\n",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseCommandsJS(tt.src)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseCommandsJS() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ParseCommandsJS() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestParseScript(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want ScriptSurface
	}{
		{
			name: "status.sh shape: flat flags, no subcommands, no args",
			src: `#!/usr/bin/env bash
#   kampodine status [--host root@<ip>] [--profile <name>] [--ssh-key <path>] [--verbose]
while [[ $# -gt 0 ]]; do
  case "$1" in
    --host) HOST_FLAG="$2"; KAMPODINE_HOST="$2"; shift 2 ;;
    --ssh-key) KEY_FLAG="$2"; SSH_KEY="$2"; shift 2 ;;
    --profile) PROFILE_FLAG="$2"; shift 2 ;;
    --verbose) VERBOSE=1; shift ;;
    -h|--help) usage ;;
    *) die "unknown argument: $1 (--help)" ;;
  esac
done
`,
			want: ScriptSurface{
				Flags: []string{"--host", "--profile", "--ssh-key", "--verbose"},
			},
		},
		{
			name: "multi-alternative dispatch collects subcommands (deploy-lifecycle shape)",
			src: `SUB="${1:-}"
case "$SUB" in
  list|prune|logs|restart|shell) shift ;;
  -h|--help|help) usage 0 ;;
  "") usage 2 ;;
  *)
    printf 'unknown\n' >&2
    usage 2
    ;;
esac
while [[ $# -gt 0 ]]; do
  case "$1" in
    --host) KAMPODINE_HOST="$2"; shift 2 ;;
    --keep) KEEP="$2"; shift 2 ;;
    --dry-run) DRY_RUN=1; shift ;;
    *) die "unknown argument: $1 (--help)" ;;
  esac
done
`,
			want: ScriptSurface{
				Flags:       []string{"--dry-run", "--host", "--keep"},
				Subcommands: []string{"list", "logs", "prune", "restart", "shell"},
			},
		},
		{
			name: "single-label dispatch collects subcommands (bluegreen shape)",
			src: `[[ -n "${OCI_COMPARTMENT:-}" ]] || die "set OCI_COMPARTMENT"
case "$cmd" in
  status)
    say "pair view"
    ;;
  init)
    say "init"
    ;;
  provision)
    say "provision"
    ;;
  flip)
    while [[ $# -gt 0 ]]; do
      case "$1" in
        --to) to="$2"; shift 2 ;;
        --force) force=1; shift ;;
        *) die "unknown flip flag: $1" ;;
      esac
    done
    ;;
  rollback)
    say "rollback"
    ;;
  *)
    exit 2
    ;;
esac
`,
			want: ScriptSurface{
				Flags:       []string{"--force", "--to"},
				Subcommands: []string{"flip", "init", "provision", "rollback", "status"},
			},
		},
		{
			name: "header usage lines yield positional args (provision <color>, shell exec --)",
			src: `#   kampodine bluegreen status                  # pair view
#   kampodine bluegreen provision <color>       # launch the second instance
#   kampodine bluegreen flip --to <color>       # health-gated flip
case "$cmd" in
  status) ;;
  init) ;;
  provision) ;;
  flip) ;;
  rollback) ;;
esac
`,
			want: ScriptSurface{
				Subcommands: []string{"flip", "init", "provision", "rollback", "status"},
				Args:        map[string][]string{"provision": {"<color>"}},
			},
		},
		{
			name: "flag values and flag-bracket groups never become positional args",
			src: `#   kampodine deploy --version <sha7>     # stream an existing local build
#   kampodine deploy --rollback [<sha7>]  # default: previous
#   kampodine deploy --host root@<ip> ... # target VM override
#   kampodine deploy --require-disk <pct> # fail closed
while [[ $# -gt 0 ]]; do
  case "$1" in
    --host) KAMPODINE_HOST="$2"; shift 2 ;;
    --version) VERSION="$2"; MODE="version"; shift 2 ;;
    --rollback) MODE="rollback"; shift ;;
    --require-disk) REQUIRE_DISK="$2"; shift 2 ;;
    *) die "unknown argument" ;;
  esac
done
`,
			want: ScriptSurface{
				Flags: []string{"--host", "--require-disk", "--rollback", "--version"},
			},
		},
		{
			name: "shell exec -- <cmd> is a positional arg of the shell subcommand",
			src: `#   kampodine deploy shell [--host root@<ip>] [exec -- <cmd>...]
case "$SUB" in
  list|prune|logs|restart|shell) shift ;;
esac
`,
			want: ScriptSurface{
				Subcommands: []string{"list", "logs", "prune", "restart", "shell"},
				Args:        map[string][]string{"shell": {"<cmd>..."}},
			},
		},
		{
			name: "delegation to a sibling script is recorded",
			src: `case "${1:-}" in
  list|prune|logs|restart|shell)
    exec bash "$HERE/deploy-lifecycle.sh" "$@"
    ;;
esac
`,
			want: ScriptSurface{
				Subcommands: []string{"list", "logs", "prune", "restart", "shell"},
				Delegates:   []string{"deploy-lifecycle.sh"},
			},
		},
		{
			name: "fixed-arg exec is a tail exec, not a surface delegate (status -> bluegreen status shape)",
			src: `if [[ -n "$PROFILE_FLAG" ]]; then
  exec bash "$HERE/bluegreen.sh" status --profile "$PROFILE_FLAG"
else
  exec bash "$HERE/bluegreen.sh" status
fi
`,
			want: ScriptSurface{
				TailExecs: []string{"bluegreen.sh"},
			},
		},
		{
			name: "repeated execs dedupe",
			src: `exec bash "$HERE/bluegreen.sh" status
exec bash "$HERE/bluegreen.sh" status --profile p
`,
			want: ScriptSurface{
				TailExecs: []string{"bluegreen.sh"},
			},
		},
		{
			name: "case on a non-dispatch variable is ignored (dns RTYPE validation shape)",
			src: `while [[ $# -gt 0 ]]; do
  case "$1" in
    --type) RTYPE="$2"; shift 2 ;;
    --zone) ZONE_ARG="$2"; shift 2 ;;
    *) die "unknown argument" ;;
  esac
done
validate_type() {
  case "$RTYPE" in
    A|AAAA|CNAME) return 0 ;;
    "") die "--type is required" ;;
    *) die "bad type" ;;
  esac
}
`,
			want: ScriptSurface{
				Flags: []string{"--type", "--zone"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseScript("fixture.sh", tt.src)
			if err != nil {
				t.Fatalf("ParseScript() error = %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ParseScript() =\n got: %#v\nwant: %#v", got, tt.want)
			}
		})
	}
}
