package project

import (
	"strings"
	"testing"
)

// migrateMode is the migrate runner's shape switch: "script" (the default
// — pnpm exec tsx over the configured script, today's behavior
// byte-identical) or "binary" (a static Go binary executed directly with
// the migrater's -db/-dir pairing — the no-node estate's runner). The
// contract:
//
//	MigrateBinary() fails closed on an unknown value — a typo must stop
//	the migrate, never silently fall back to the pnpm script.

func TestMigrateModeDefaultIsScript(t *testing.T) {
	if got := LoadDefault().MigrateMode; got != "script" {
		t.Errorf("MigrateMode default = %q, want \"script\" (byte-identical legacy behavior)", got)
	}
	binary, err := LoadDefault().MigrateBinary()
	if err != nil || binary {
		t.Errorf("MigrateBinary() = %v, %v — the default must be the script runner", binary, err)
	}
}

func TestMigrateBinaryFailsClosedOnUnknownValue(t *testing.T) {
	cfg := LoadDefault()
	cfg.MigrateMode = "wat"
	if _, err := cfg.MigrateBinary(); err == nil {
		t.Fatal("MigrateBinary() must fail closed on an unknown migrateMode")
	} else if !strings.Contains(err.Error(), "wat") {
		t.Errorf("err = %v, want it to name the bad value", err)
	}
	// The zero-value config tolerates "" as script (Config{} is a valid
	// pre-ladder shape in tests).
	zero, err := Config{}.MigrateBinary()
	if err != nil || zero {
		t.Errorf("Config{}.MigrateBinary() = %v, %v — \"\" must read as script", zero, err)
	}
}

func TestResolveMigrateModeLadder(t *testing.T) {
	t.Run("profile block sets binary", func(t *testing.T) {
		cfg := Resolve(nil, []byte(`{"migrateMode": "binary"}`), lookupOf(nil))
		binary, err := cfg.MigrateBinary()
		if err != nil || !binary {
			t.Errorf("MigrateBinary() = %v, %v — the profile block must select binary", binary, err)
		}
	})

	t.Run("env overrides the profile block", func(t *testing.T) {
		cfg := Resolve(nil, []byte(`{"migrateMode": "binary"}`), lookupOf(map[string]string{
			"KAMPODRA_MIGRATE_MODE": "script",
		}))
		binary, err := cfg.MigrateBinary()
		if err != nil || binary {
			t.Errorf("MigrateBinary() = %v, %v — env must beat the profile block", binary, err)
		}
	})

	t.Run("env sets binary without a block", func(t *testing.T) {
		cfg := Resolve(nil, nil, lookupOf(map[string]string{"KAMPODRA_MIGRATE_MODE": "binary"}))
		if binary, _ := cfg.MigrateBinary(); !binary {
			t.Error("MigrateBinary() = false — KAMPODRA_MIGRATE_MODE must resolve")
		}
	})
}

// migrateDir is the repo-relative migrations root the binary runner feeds
// -dir with (holding the migrater's root/ + tenant/ pairing). Script mode
// never reads it; the default stays empty.
func TestResolveMigrateDirLadder(t *testing.T) {
	if got := LoadDefault().MigrateDir; got != "" {
		t.Errorf("MigrateDir default = %q, want empty (script mode never reads it)", got)
	}
	cfg := Resolve(nil, []byte(`{"migrateDir": "apps/api/drizzle"}`), lookupOf(nil))
	if cfg.MigrateDir != "apps/api/drizzle" {
		t.Errorf("MigrateDir = %q, want the profile block value", cfg.MigrateDir)
	}
	cfg = Resolve(nil, []byte(`{"migrateDir": "apps/api/drizzle"}`), lookupOf(map[string]string{
		"KAMPODRA_MIGRATE_DIR": "ops/migrations",
	}))
	if cfg.MigrateDir != "ops/migrations" {
		t.Errorf("MigrateDir = %q, want the env value (env beats the profile block)", cfg.MigrateDir)
	}
}

func TestMigrateModeAndDirFieldViewRendering(t *testing.T) {
	// config print renders both rows with provenance — the catalog must
	// never silently drop a field when Config grows.
	cfg, traces := ResolveTraced(nil, lookupOf(map[string]string{
		"KAMPODRA_MIGRATE_MODE": "binary",
		"KAMPODRA_MIGRATE_DIR":  "apps/api/drizzle",
	}), nil, nil)
	sawMode, sawDir := false, false
	for _, v := range FieldViews(cfg, traces) {
		switch v.Key {
		case "migrateMode":
			sawMode = true
			if v.Value != "binary" {
				t.Errorf("migrateMode row = %q, want binary", v.Value)
			}
			if v.Trace.Source != SourceEnv || v.Trace.Origin != "KAMPODRA_MIGRATE_MODE" {
				t.Errorf("migrateMode trace = %+v, want env origin KAMPODRA_MIGRATE_MODE", v.Trace)
			}
		case "migrateDir":
			sawDir = true
			if v.Value != "apps/api/drizzle" {
				t.Errorf("migrateDir row = %q, want the env value", v.Value)
			}
		}
	}
	if !sawMode || !sawDir {
		t.Fatalf("FieldViews is missing rows (mode=%v dir=%v) — config print would drop them", sawMode, sawDir)
	}
}
