// The MariaDB leg of the db surface: a server engine, not a file — the
// flow composes mariadb-dump/mariadb ON the VM (or FROM the app VM against
// a db host), with credentials read from the VM's env file at run time.
// kampodra never stores DB credentials itself: they ride the project's env
// file (path injected by the caller) via `kampodra env set/push`, are
// copied into a 0600 defaults file on the VM, and never touch argv
// (nothing leaks into `ps`).

package dbq

import (
	crand "crypto/rand"
	"encoding/hex"
	"fmt"
)

// Unit-env contract keys (the VM's env file — the project adapter's
// EnvFile, path injected — carries them via `kampodra env set`/`env
// push`: the same surface the app itself uses).
const (
	EngineEnvKey        = "KAMPODRA_DB_ENGINE"   // "mariadb" selects this leg
	MariaNameEnvKey     = "KAMPODRA_DB_NAME"     // the schema to dump/query
	MariaHostEnvKey     = "KAMPODRA_DB_HOST"     // empty = local socket (same-VM)
	MariaPortEnvKey     = "KAMPODRA_DB_PORT"     // empty = client default
	MariaUserEnvKey     = "KAMPODRA_DB_USER"     // empty = ssh user (unix_socket auth)
	MariaPasswordEnvKey = "KAMPODRA_DB_PASSWORD" // empty = no password line
)

// MariaProbeCommand answers yes/no in ONE round-trip: is the MariaDB
// client stack present? (mariadb-dump AND the mariadb client — the dump
// alone can't run queries.)
const MariaProbeCommand = `if command -v mariadb-dump >/dev/null 2>&1 && command -v mariadb >/dev/null 2>&1; then echo yes; else echo no; fi`

// EngineMagicProbeCommand classifies a FILE argument by its magic: the
// sqlite leg's signature is the 16-byte header. Used by auto engine
// resolution when the unit env declares nothing.
func EngineMagicProbeCommand(dbPath string) string {
	return "head -c 15 " + dbPath + " 2>/dev/null | grep -q 'SQLite format 3' && echo sqlite || echo other"
}

// EngineUnitEnvProbeCommand echoes the engine the unit env declares
// (mariadb | libsql | empty — the operator's explicit choice rides
// KAMPODRA_DB_ENGINE). The env file's path is the project adapter's
// (profile-configurable) — injected, never hardcoded here.
func EngineUnitEnvProbeCommand(envFile string) string {
	return "grep -E '^" + EngineEnvKey + "=(mariadb|libsql|postgres)' " + envFile + " 2>/dev/null | head -1 | cut -d= -f2-"
}

// MariaDefaultsPath is the on-VM defaults file holding the client creds —
// 0600, scratch-named, cleaned up by the flow that created it.
func MariaDefaultsPath() string {
	return "/tmp/kampodra-maria-defaults"
}

// MariaDefaultsFromUnitEnvCommand builds the client defaults file FROM the
// unit env in ONE compound sh run: grep (never source — the unit env is
// env-file semantics, values are raw after the first '='), percent-escape
// for the option-file format, umask 077 + chmod 600. The password goes
// env-file → defaults-file on the VM; it never transits argv or stdout.
func MariaDefaultsFromUnitEnvCommand(envFile, defaults string) string {
	return `get() { grep -E "^$1=" ` + envFile + ` 2>/dev/null | head -1 | cut -d= -f2-; }; ` +
		`esc() { sed 's/%/%%/g'; }; ` +
		`umask 077; { ` +
		`printf '[client]\n'; ` +
		`v=$(get ` + MariaHostEnvKey + `); [ -n "$v" ] && printf 'host=%s\n' "$v"; ` +
		`v=$(get ` + MariaPortEnvKey + `); [ -n "$v" ] && printf 'port=%s\n' "$v"; ` +
		`v=$(get ` + MariaUserEnvKey + `); [ -n "$v" ] && printf 'user=%s\n' "$v"; ` +
		`v=$(get ` + MariaPasswordEnvKey + ` | esc); [ -n "$v" ] && printf 'password=%s\n' "$v"; ` +
		`} > ` + defaults + ` && chmod 600 ` + defaults
}

// MariaDefaultsCleanupCommand removes the defaults file (rides
// context.Background() — must survive ctrl-C, never masks the verdict).
func MariaDefaultsCleanupCommand(defaults string) string {
	return "rm -f " + defaults
}

// MariaScratchToken is the maria legs' shared crypto/rand suffix.
func MariaScratchToken() string {
	var b [4]byte
	if _, err := crand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("crypto/rand: %v", err))
	}
	return hex.EncodeToString(b[:])
}

// MariaScratchPath mints the dump's scratch file — same crypto/rand
// discipline as the sqlite snapshot scratch, .sql-suffixed.
func MariaScratchPath() string {
	return "/tmp/kampodra-dump-" + MariaScratchToken() + ".sql"
}

// ValidateMariaDbName gates the schema NAME before it rides a remote
// command: a single shell-safe token (no whitespace, no metacharacters —
// the name is composed into `mariadb-dump NAME > scratch`).
func ValidateMariaDbName(name string) error {
	if name == "" {
		return fmt.Errorf("mariadb leg: a database name is required (--db, or %s in the unit env)", MariaNameEnvKey)
	}
	if len(name) > 64 {
		return fmt.Errorf("mariadb leg: database name too long (%d chars, max 64)", len(name))
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '_', r == '-', r == '$':
		default:
			return fmt.Errorf("mariadb leg: database name must be a bare identifier (got %.20q — whitespace and shell metacharacters are rejected)", name)
		}
	}
	return nil
}

// MariaDumpCommand streams a consistent logical dump to the scratch file:
// --single-transaction gives InnoDB a point-in-time without locking,
// --quick streams row-by-row (no VM-side memory blowup), routines and
// triggers ride along. stderr passes through to the operator.
func MariaDumpCommand(defaults, dbName, scratch string) string {
	return "mariadb-dump --defaults-extra-file=" + defaults +
		" --single-transaction --quick --routines --triggers" +
		" " + dbName + " > " + scratch
}

// MariaVerifyDumpCommand checks the dump's tail — a complete mariadb-dump
// always ends with a "Dump completed" stamp; its absence means the dump
// died mid-stream (disk, conn) and the copy must NOT be trusted.
func MariaVerifyDumpCommand(scratch string) string {
	return "tail -c 200 " + scratch + " | grep -q 'Dump completed'"
}

// MariaQueryCommand runs the statement arriving on stdin — batch mode
// (tab-separated, header row), SQL never rides argv.
func MariaQueryCommand(defaults, dbName string) string {
	return "mariadb --defaults-extra-file=" + defaults + " --batch --binary-as-hex " + dbName
}

// MariaDbNameFromUnitEnvCommand prints the declared schema (the query/
// snapshot flows need a NAME, not a path — resolved once, named here).
func MariaDbNameFromUnitEnvCommand(envFile string) string {
	return "grep -E '^" + MariaNameEnvKey + "=' " + envFile + " 2>/dev/null | head -1 | cut -d= -f2-"
}

// MariaGrantROSQL returns the idempotent read-only role SQL for
// `db grant-ro` (MariaDB dialect, unix_socket root over ssh): create-or-
// rotate the role, then SELECT on the schema. The schema is backtick-
// quoted; the password rides stdin via the caller (never argv).
func MariaGrantROSQL(role, password, dbName string) string {
	return "CREATE USER IF NOT EXISTS " + jsonString(role) + "@'localhost' IDENTIFIED BY " + jsonString(password) + ";" +
		" ALTER USER " + jsonString(role) + "@'localhost' IDENTIFIED BY " + jsonString(password) + ";" +
		" GRANT SELECT ON `" + dbName + "`.* TO " + jsonString(role) + "@'localhost';"
}

// MariaEnvAppendCommand appends the role's env keys to the VM's env file
// idempotently (existing KAMPODRA_DB_USER/PASSWORD lines are removed
// first) — the VALUES ride stdin (never argv): two lines, USER then
// PASSWORD.
func MariaEnvAppendCommand(envFile string) string {
	return `umask 077; sed -i '/^` + MariaUserEnvKey + `=/d;/^` + MariaPasswordEnvKey + `=/d' ` + envFile + ` && printf '%s' "$(cat)" >> ` + envFile
}

// MariaInstallHint names the client packages for probe-fail errors.
const MariaInstallHint = "apk add mariadb-client / apt-get install mariadb-client"

// --- the podman container path -------------------------------------------
// MariaDB deployed as a podman container (the estate's shape): the server
// tools live IN the image — mariadb/mariadb-dump/mariabackup run via
// `podman exec`, in-container root over the socket (ssh root = DBA, the
// same trust model). The host-client path stays as fallback.

// MariaContainerEnvKey names the container explicitly in the env file
// (beats detection); empty = auto-detect the first running container
// whose name or image matches maria|mysql.
const MariaContainerEnvKey = "KAMPODRA_DB_CONTAINER"

// The mode env key: ssh (host client) | pod (podman exec) — an explicit
// choice beats auto-detection (which prefers pod: a running container is
// self-evident, while a host CLIENT can exist without a host SERVER).
const MariaModeEnvKey = "KAMPODRA_DB_MODE"

// MariaModeProbeCommand answers THREE ways in one round-trip: "container
// <name>" (a running maria/mysql container — explicit env key first,
// else detected; auto's PREFERRED answer), "host" (host client stack),
// or "no". The caller's explicit mode decides before this runs.
func MariaModeProbeCommand(envFile string) string {
	return `get() { grep -E "^$1=" ` + envFile + ` 2>/dev/null | head -1 | cut -d= -f2-; }; ` +
		`if command -v podman >/dev/null 2>&1; then ` +
		`c=$(get ` + MariaContainerEnvKey + `); ` +
		`if [ -z "$c" ]; then c=$(podman ps --format '{{.Names}} {{.Image}}' 2>/dev/null | awk 'tolower($0) ~ /maria|mysql/ {print $1; exit}'); fi; ` +
		`if [ -n "$c" ]; then echo "container $c"; exit 0; fi; ` +
		`fi; ` +
		`if command -v mariadb-dump >/dev/null 2>&1 && command -v mariadb >/dev/null 2>&1; then echo host; exit 0; fi; ` +
		`echo no`
}

// MariaContainerQueryCommand: the statement rides stdin into the
// container's mariadb client (in-container root — the gate is the only
// limiter; the ro-role concept belongs to the host-credential path).

// MariaContainerGrantROSQL runs the role SQL inside the container (root
// socket) — same SQL as the host path.
// MariaContainerBackupDir mints the physical backup's scratch directory
// (the crypto/rand discipline; mariabackup requires a fresh empty dir).
func MariaContainerBackupDir() string {
	return "/tmp/kampodra-mariabackup-" + MariaScratchToken()
}

// The physical flow, one step per command: backup (in-container root,
// socket auth) → prepare (crash-apply, makes it restorable) → tar (host
// side — the dir is on the VM fs) → verify (the archive must contain
// mariabackup's control files; a torn tar fails the listing) → fetch
// (the shared cat) → rm -rf both.
// MariaContainerQueryCommand: the statement rides stdin into the
// container's client, authenticating against the staged defaults file
// (the official image is password-auth; unix_socket is the
// Debian-package shape — found by the real-engine smoke).
func MariaContainerQueryCommand(container string) string {
	return "podman exec -i " + container + " mariadb --defaults-extra-file=" + MariaContainerCredsPath + " --batch --binary-as-hex"
}

func MariaContainerBackupCommand(container, dir string) string {
	return "podman exec " + container + " mariabackup --defaults-extra-file=" + MariaContainerCredsPath + " --backup --target-dir=" + dir
}

func MariaContainerPrepareCommand(container, dir string) string {
	return "podman exec " + container + " mariabackup --defaults-extra-file=" + MariaContainerCredsPath + " --prepare --target-dir=" + dir
}

func MariaContainerTarCommand(dir, tar string) string {
	return "tar -C " + dir + " -cf " + tar + " ."
}

func MariaContainerVerifyTarCommand(tar string) string {
	return "tar -tf " + tar + " | grep -q xtrabackup_checkpoints"
}

func MariaContainerCleanupCommand(dir, tar string) string {
	return "rm -rf " + dir + " " + tar
}

// MariaContainerResolveCommand answers ONLY the container name (the
// explicit pod mode's single question; empty = none running).
func MariaContainerResolveCommand(envFile string) string {
	return `get() { grep -E "^$1=" ` + envFile + ` 2>/dev/null | head -1 | cut -d= -f2-; }; ` +
		`c=$(get ` + MariaContainerEnvKey + `); ` +
		`if [ -z "$c" ]; then c=$(podman ps --format '{{.Names}} {{.Image}}' 2>/dev/null | awk 'tolower($0) ~ /maria|mysql/ {print $1; exit}'); fi; ` +
		`echo "container $c"`
}

// MariaRootPasswordEnvKey: the container image authenticates root by
// password (MARIADB_ROOT_PASSWORD), not unix_socket — physical backups
// (mariabackup) need this privilege level; the env file carries it.
const MariaRootPasswordEnvKey = "KAMPODRA_DB_ROOT_PASSWORD"

// MariaContainerCredsPath is the defaults file INSIDE the container.
const MariaContainerCredsPath = "/tmp/kampodra-maria.cnf"

// MariaContainerCredsStageCommand builds the in-container defaults file:
// the VM's env file is parsed HOST-side and the ini is piped into
// `podman exec -i … cat >` (0600 in-container; the password transits the
// pipe, never argv). Least privilege: KAMPODRA_DB_USER/PASSWORD (the ro
// role) when set; otherwise root via KAMPODRA_DB_ROOT_PASSWORD —
// mariabackup requires that privilege level, so physical snapshots want
// the root key present.
func MariaContainerCredsStageCommand(envFile, container string, forceRoot bool) string {
	rootPref := `if [ -n "$u" ]; then printf 'user=%s\n' "$u"; [ -n "$w" ] && printf 'password=%s\n' "$w"; `
	rootSuf := `elif [ -n "$r" ]; then printf 'user=root\npassword=%s\n' "$r"; fi; `
	if forceRoot {
		// grant-ro rotates and mariabackup needs root: the ro role can't
		// do either, so the staged cnf is root-only.
		rootPref = `u=root; [ -z "$w" ] && w=$r; if [ -n "$w" ]; then printf 'user=%s\n' "$u"; printf 'password=%s\n' "$w"; `
		rootSuf = `fi; `
	}
	return `get() { grep -E "^$1=" ` + envFile + ` 2>/dev/null | head -1 | cut -d= -f2-; }; ` +
		`{ printf '[client]\n'; ` +
		`u=$(get ` + MariaUserEnvKey + `); w=$(get ` + MariaPasswordEnvKey + `); r=$(get ` + MariaRootPasswordEnvKey + `); ` +
		rootPref + rootSuf + `} | ` +
		`podman exec -i ` + container + ` sh -c 'umask 077; cat > ` + MariaContainerCredsPath + ` && chmod 600 ` + MariaContainerCredsPath + `'; ` +
		`podman exec ` + container + ` sh -c 'test -s ` + MariaContainerCredsPath + ` || test -f ` + MariaContainerCredsPath + `'`
}

// MariaContainerCredsCleanupCommand removes the staged cnf (in-container
// /tmp — the staged role/root password must not outlive the command).
func MariaContainerCredsCleanupCommand(container string) string {
	return "podman exec " + container + " rm -f " + MariaContainerCredsPath
}
