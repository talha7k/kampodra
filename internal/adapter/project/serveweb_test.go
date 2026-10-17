package project

import (
	"strconv"
	"testing"
)

// serveWeb is the project's web-serving posture: TRUE by default (every
// pre-existing project renders byte-identically — the SPA dist statics
// ride the unit as always), and the no-web estate (web on Vercel, the VM
// strictly api) sets FALSE so the unit render drops every static-kind
// block: the -v dist mount, the WEB_DIST_DIR -e, the releases layout.

func TestServeWebDefaultIsTrue(t *testing.T) {
	if !LoadDefault().ServeWebEnabled() {
		t.Error("ServeWebEnabled() = false on defaults — the default must stay true (backward compat)")
	}
	if !(Config{}).ServeWebEnabled() {
		t.Error("ServeWebEnabled() = false on the zero value — nil must read as true")
	}
}

func TestServeWebExplicitFalse(t *testing.T) {
	f := false
	cfg := Config{ServeWeb: &f}
	if cfg.ServeWebEnabled() {
		t.Error("ServeWebEnabled() = true — the explicit false must win")
	}
	tr := true
	cfg.ServeWeb = &tr
	if !cfg.ServeWebEnabled() {
		t.Error("ServeWebEnabled() = false — the explicit true must win")
	}
}

func TestResolveServeWebLadder(t *testing.T) {
	t.Run("profile block sets false", func(t *testing.T) {
		cfg := Resolve(nil, []byte(`{"serveWeb": false}`), lookupOf(nil))
		if cfg.ServeWebEnabled() {
			t.Error("ServeWebEnabled() = true — the profile block's false must win")
		}
	})

	t.Run("env overrides the profile block", func(t *testing.T) {
		cfg := Resolve(nil, []byte(`{"serveWeb": false}`), lookupOf(map[string]string{
			"KAMPODRA_SERVE_WEB": "true",
		}))
		if !cfg.ServeWebEnabled() {
			t.Error("ServeWebEnabled() = false — env must beat the profile block")
		}
	})

	t.Run("env sets false without a block", func(t *testing.T) {
		cfg := Resolve(nil, nil, lookupOf(map[string]string{"KAMPODRA_SERVE_WEB": "false"}))
		if cfg.ServeWebEnabled() {
			t.Error("ServeWebEnabled() = true — KAMPODRA_SERVE_WEB=false must resolve")
		}
	})

	t.Run("garbage env value fails open to the default (true)", func(t *testing.T) {
		cfg := Resolve(nil, nil, lookupOf(map[string]string{"KAMPODRA_SERVE_WEB": "occasionally"}))
		if !cfg.ServeWebEnabled() {
			t.Error("ServeWebEnabled() = false — an unparseable value must leave the default standing")
		}
	})
}

func TestServeWebFieldViewRendering(t *testing.T) {
	// config print renders the row as true/false (never an empty cell —
	// the default row must render "true" with the default provenance).
	cfg, traces := ResolveTraced(nil, lookupOf(map[string]string{
		"KAMPODRA_SERVE_WEB": "false",
	}), nil, nil)
	saw := false
	for _, v := range FieldViews(cfg, traces) {
		if v.Key != "serveWeb" {
			continue
		}
		saw = true
		if v.Value != strconv.FormatBool(false) {
			t.Errorf("serveWeb row = %q, want \"false\"", v.Value)
		}
		if v.Trace.Source != SourceEnv || v.Trace.Origin != "KAMPODRA_SERVE_WEB" {
			t.Errorf("serveWeb trace = %+v, want env origin KAMPODRA_SERVE_WEB", v.Trace)
		}
	}
	if !saw {
		t.Fatal("FieldViews is missing the serveWeb row — config print would drop it")
	}

	defCfg, defTraces := ResolveTraced(nil, nil, nil, nil)
	for _, v := range FieldViews(defCfg, defTraces) {
		if v.Key == "serveWeb" {
			if v.Value != "true" {
				t.Errorf("default serveWeb row = %q, want \"true\"", v.Value)
			}
			if v.Trace.Source != SourceDefault {
				t.Errorf("default serveWeb source = %q, want default", v.Trace.Source)
			}
		}
	}
}
