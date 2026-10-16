package project

import (
	"strings"
	"testing"
)

// proxyHosts is the optional LIST form of proxyHost: production
// kamal-proxy edges serve several hostnames (the esellar VM hand-extended
// sslip.io + 3 real domains via ssh because the CLI had no surface — a
// scalar re-point would silently DROP the rest). The contract:
//
// EffectiveProxyHosts = ProxyHosts wholesale, else EXACTLY [ProxyHost].
// A scalar-only project resolves byte-identically to before.

func TestEffectiveProxyHostsFallback(t *testing.T) {
	scalarOnly := LoadDefault()
	got := scalarOnly.EffectiveProxyHosts()
	if len(got) != 1 || got[0] != scalarOnly.ProxyHost {
		t.Errorf("EffectiveProxyHosts() = %v, want exactly [%q]", got, scalarOnly.ProxyHost)
	}

	listed := LoadDefault()
	listed.ProxyHosts = []string{"edge.example.com", "www.example.com", "api.example.com"}
	got = listed.EffectiveProxyHosts()
	if strings.Join(got, ",") != "edge.example.com,www.example.com,api.example.com" {
		t.Errorf("EffectiveProxyHosts() = %v, want the configured list WHOLESALE", got)
	}
}

func TestResolveProxyHostsLadder(t *testing.T) {
	t.Run("env comma-separated list resolves; the scalar stays untouched", func(t *testing.T) {
		cfg := Resolve(nil, nil, lookupOf(map[string]string{
			"KAMPODRA_PROXY_HOSTS": "edge.example.com,www.example.com",
		}))
		if strings.Join(cfg.EffectiveProxyHosts(), ",") != "edge.example.com,www.example.com" {
			t.Errorf("EffectiveProxyHosts() = %v, want the env list", cfg.EffectiveProxyHosts())
		}
		if cfg.ProxyHost != LoadDefault().ProxyHost {
			t.Errorf("ProxyHost = %q, want the untouched default (the list is not the scalar)", cfg.ProxyHost)
		}
	})

	t.Run("the parser splits on commas AND whitespace", func(t *testing.T) {
		// resolver.set hands apply() a SPACE-joined list for list-typed
		// layers, and operators type comma-separated values — both must
		// parse identically.
		cfg := Resolve(nil, nil, lookupOf(map[string]string{
			"KAMPODRA_PROXY_HOSTS": "edge.example.com, www.example.com api.example.com",
		}))
		if strings.Join(cfg.ProxyHosts, ",") != "edge.example.com,www.example.com,api.example.com" {
			t.Errorf("ProxyHosts = %v, want three hosts", cfg.ProxyHosts)
		}
	})

	t.Run("profile block list loads and env beats it", func(t *testing.T) {
		cfg := Resolve(nil, []byte(`{"proxyHosts": ["pf.example.com"]}`), lookupOf(nil))
		if strings.Join(cfg.ProxyHosts, ",") != "pf.example.com" {
			t.Errorf("ProxyHosts = %v, want the profile block list", cfg.ProxyHosts)
		}
		cfg = Resolve(nil, []byte(`{"proxyHosts": ["pf.example.com"]}`), lookupOf(map[string]string{
			"KAMPODRA_PROXY_HOSTS": "env.example.com",
		}))
		if strings.Join(cfg.ProxyHosts, ",") != "env.example.com" {
			t.Errorf("ProxyHosts = %v, want the env value (env beats the profile block)", cfg.ProxyHosts)
		}
	})
}

func TestProxyHostsFieldViewRendering(t *testing.T) {
	// config print renders the row comma-joined (the kamal-proxy --host
	// wire format) with per-layer provenance, and the default row renders
	// the effective scalar — never an empty cell.
	cfg, traces := ResolveTraced(nil, lookupOf(map[string]string{
		"KAMPODRA_PROXY_HOSTS": "edge.example.com,www.example.com",
	}), nil, nil)
	var sawList bool
	for _, v := range FieldViews(cfg, traces) {
		if v.Key != "proxyHosts" {
			continue
		}
		sawList = true
		if v.Value != "edge.example.com,www.example.com" {
			t.Errorf("proxyHosts row = %q, want the comma-joined list", v.Value)
		}
		if v.Trace.Source != SourceEnv || v.Trace.Origin != "KAMPODRA_PROXY_HOSTS" {
			t.Errorf("proxyHosts trace = %+v, want env origin KAMPODRA_PROXY_HOSTS", v.Trace)
		}
	}
	if !sawList {
		t.Fatal("FieldViews is missing the proxyHosts row — config print would drop it")
	}

	defCfg, defTraces := ResolveTraced(nil, nil, nil, nil)
	for _, v := range FieldViews(defCfg, defTraces) {
		if v.Key == "proxyHosts" {
			if v.Value != LoadDefault().ProxyHost {
				t.Errorf("default proxyHosts row = %q, want the effective scalar %q", v.Value, LoadDefault().ProxyHost)
			}
			if v.Trace.Source != SourceDefault {
				t.Errorf("default proxyHosts source = %q, want default", v.Trace.Source)
			}
		}
	}
}
