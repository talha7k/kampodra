package dbq

import (
	"strings"
	"testing"
)

// testEnvFile — the env file's path is the PROJECT's (profile-
// configurable); tests inject a literal so dbq stays magic-string-clean.
const testEnvFile = "/etc/kampodra/env"

// The MariaDB leg's contract: credentials ride unit env → 0600 defaults
// file ON the VM (never argv — `ps` must never see a password), the dump
// is single-transaction + quick, and completeness is proven by the tail
// stamp before anything is fetched.

func TestMariaDefaultsFromUnitEnvCommand(t *testing.T) {
	got := MariaDefaultsFromUnitEnvCommand(testEnvFile, MariaDefaultsPath())
	for _, want := range []string{
		"umask 077",
		"[client]",
		"get() { grep -E \"^$1=\" " + testEnvFile,
		"get " + MariaHostEnvKey,
		"get " + MariaPasswordEnvKey,
		"sed 's/%/%%/g'", // option-file percent escape on the password only
		"chmod 600 " + MariaDefaultsPath(),
	} {
		if !strings.Contains(got, want) {
			t.Errorf("defaults command missing %q:\n%s", want, got)
		}
	}
	// The host/user/password greps must NOT run through esc() — only the
	// password needs the option-file percent escape.
	if strings.Count(got, "esc)") != 1 {
		t.Errorf("esc() must apply to the password line only:\n%s", got)
	}
}

func TestMariaCommandComposition(t *testing.T) {
	defaults := MariaDefaultsPath()
	if dump := MariaDumpCommand(defaults, "esellar", "/tmp/scratch.sql"); !strings.Contains(dump, "--single-transaction") ||
		!strings.Contains(dump, "--quick") ||
		!strings.HasSuffix(dump, "esellar > /tmp/scratch.sql") {
		t.Errorf("dump command malformed:\n%s", dump)
	}
	if q := MariaQueryCommand(defaults, "esellar"); !strings.Contains(q, "--defaults-extra-file="+defaults) ||
		!strings.Contains(q, "--batch esellar") {
		t.Errorf("query command malformed:\n%s", q)
	}
	// The SQL rides stdin — the query command itself must contain no
	// statement text and no credentials by construction.
	if v := MariaVerifyDumpCommand("/tmp/scratch.sql"); !strings.Contains(v, "grep -q 'Dump completed'") {
		t.Errorf("verify command malformed:\n%s", v)
	}
}

func TestMariaProbeAndEngineProbes(t *testing.T) {
	if !strings.Contains(MariaProbeCommand, "mariadb-dump") || !strings.Contains(MariaProbeCommand, "command -v mariadb ") {
		t.Errorf("client probe must require BOTH binaries:\n%s", MariaProbeCommand)
	}
	if magic := EngineMagicProbeCommand("/data/root.db"); !strings.Contains(magic, "head -c 15 /data/root.db") ||
		!strings.Contains(magic, "SQLite format 3") {
		t.Errorf("magic probe malformed:\n%s", magic)
	}
	if env := EngineUnitEnvProbeCommand(testEnvFile); !strings.Contains(env, EngineEnvKey+"=(mariadb|libsql|postgres)") ||
		!strings.Contains(env, testEnvFile) {
		t.Errorf("unit-env engine probe malformed:\n%s", env)
	}
	if name := MariaDbNameFromUnitEnvCommand(testEnvFile); !strings.Contains(name, MariaNameEnvKey) {
		t.Errorf("db-name probe malformed:\n%s", name)
	}
}

// --- the engine seam ------------------------------------------------------

// The maria leg is fully self-describing: probe, creds stage+cleanup, its
// own validator, and a query composition that takes only the target.
func TestMariaEngineSteps(t *testing.T) {
	eng := MariaEngine(testEnvFile)
	if eng.Name != "mariadb" || eng.ProbeCommand != MariaProbeCommand {
		t.Fatalf("engine identity malformed: %+v", eng)
	}
	if eng.CredsStage == "" || !strings.Contains(eng.CredsStage, MariaDefaultsPath()) {
		t.Errorf("creds stage must compose the defaults file:\n%s", eng.CredsStage)
	}
	if eng.CredsCleanup != MariaDefaultsCleanupCommand(MariaDefaultsPath()) {
		t.Errorf("creds cleanup malformed:\n%s", eng.CredsCleanup)
	}
	if err := eng.Validate("esellar"); err != nil {
		t.Errorf("valid schema rejected: %v", err)
	}
	if err := eng.Validate("ese llar; rm -rf"); err == nil {
		t.Errorf("metacharacter schema accepted")
	}
	if q := eng.QueryCommand("esellar"); !strings.Contains(q, "--batch esellar") {
		t.Errorf("query composition malformed:\n%s", q)
	}
}

// The sqlite leg carries no creds and no probe — provisioning is db.go's
// helper flow — and its query composition closes over the helper path.
func TestSQLiteEngineSteps(t *testing.T) {
	eng := SQLiteEngine("/tmp/kampodra-sqlq-v1-arm64")
	if eng.Name != "sqlite" || eng.ProbeCommand != "" || eng.CredsStage != "" || eng.CredsCleanup != "" {
		t.Fatalf("sqlite steps must be empty where the flow provisions: %+v", eng)
	}
	if err := eng.Validate("/data/root.db"); err != nil {
		t.Errorf("valid path rejected: %v", err)
	}
	if err := eng.Validate("root.db"); err != nil {
		t.Errorf("relative paths are legitimate sqlite targets: %v", err)
	}
	if err := eng.Validate("x; rm -rf /"); err == nil {
		t.Errorf("metacharacters accepted as a sqlite path")
	}
	if q := eng.QueryCommand("/data/root.db"); !strings.Contains(q, "/tmp/kampodra-sqlq-v1-arm64 -db /data/root.db") {
		t.Errorf("query composition must use the provisioned helper:\n%s", q)
	}
}

// The grant-ro SQL + env-append composition: create-or-rotate + SELECT
// on the schema; the env keys ride stdin (never argv).
func TestMariaGrantROComposition(t *testing.T) {
	sql := MariaGrantROSQL("kampodra_ro", "pw456", "esellar")
	for _, want := range []string{
		"CREATE USER IF NOT EXISTS", "ALTER USER", "IDENTIFIED BY",
		"GRANT SELECT ON `esellar`.*",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("grant SQL missing %q:\n%s", want, sql)
		}
	}
	appendCmd := MariaEnvAppendCommand(testEnvFile)
	for _, want := range []string{
		"umask 077",
		"sed -i '/^KAMPODRA_DB_USER=/d;/^KAMPODRA_DB_PASSWORD=/d'",
		`printf '%s' "$(cat)"`,
		testEnvFile,
	} {
		if !strings.Contains(appendCmd, want) {
			t.Errorf("env append missing %q:\n%s", want, appendCmd)
		}
	}
}

// GeneratePassword: 48 hex chars, no quoting hazards.
func TestGeneratePassword(t *testing.T) {
	pw, err := GeneratePassword()
	if err != nil {
		t.Fatal(err)
	}
	if len(pw) != 48 {
		t.Errorf("password length %d, want 48", len(pw))
	}
	for _, r := range pw {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			t.Errorf("password must be lowercase hex, got %q", pw)
			break
		}
	}
}

// --- the podman container path --------------------------------------------

// The mode probe: host wins when the client stack exists; the explicit
// container env key beats detection; detection greps names+images.
func TestMariaModeProbeCommand(t *testing.T) {
	probe := MariaModeProbeCommand(testEnvFile)
	for _, want := range []string{
		`get() { grep -E "^$1=" ` + testEnvFile,
		"if command -v mariadb-dump", // host client first
		"command -v podman",
		"get " + MariaContainerEnvKey, // explicit beats detection
		"tolower($0) ~ /maria|mysql/", // then names+images
	} {
		if !strings.Contains(probe, want) {
			t.Errorf("mode probe missing %q:\n%s", want, probe)
		}
	}
}

// Container compositions: stdin clients and the physical backup flow.
func TestMariaContainerCompositions(t *testing.T) {
	if q := MariaContainerQueryCommand("maria-pod"); q != "podman exec -i maria-pod mariadb --batch" {
		t.Errorf("container query malformed:\n%s", q)
	}
	dir := MariaContainerBackupDir()
	if !strings.HasPrefix(dir, "/tmp/kampodra-mariabackup-") {
		t.Errorf("backup dir malformed: %s", dir)
	}
	if b := MariaContainerBackupCommand("maria-pod", dir); !strings.Contains(b, "podman exec maria-pod mariabackup --backup --target-dir="+dir) {
		t.Errorf("backup malformed:\n%s", b)
	}
	if p := MariaContainerPrepareCommand("maria-pod", dir); !strings.Contains(p, "mariabackup --prepare --target-dir="+dir) {
		t.Errorf("prepare malformed:\n%s", p)
	}
	tar := dir + ".tar"
	if tc := MariaContainerTarCommand(dir, tar); !strings.Contains(tc, "tar -C "+dir+" -cf "+tar) {
		t.Errorf("tar malformed:\n%s", tc)
	}
	if v := MariaContainerVerifyTarCommand(tar); !strings.Contains(v, "grep -q xtrabackup_checkpoints") {
		t.Errorf("verify must gate on the control files:\n%s", v)
	}
	if c := MariaContainerCleanupCommand(dir, tar); c != "rm -rf "+dir+" "+tar {
		t.Errorf("cleanup malformed:\n%s", c)
	}
}
