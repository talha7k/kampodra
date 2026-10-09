package command

import (
	"encoding/json"

	"github.com/talha7k/kampodra/internal/adapter/project"
	"github.com/talha7k/kampodra/internal/adapter/state"
	"github.com/talha7k/kampodra/internal/adapter/transport"
)

// Target is a fully resolved run target: where to ssh, what the public
// edge hostname is, the profile that contributed values, and the resolved
// project shape (project.Config — naming never referenced directly).
type Target struct {
	HostSpec    transport.HostSpec
	ProxyHost   string
	ProfileName string
	ProfileInit string // cached init verdict from the profile (probe skip)
	Project     project.Config
	Cloud       json.RawMessage // raw profile "cloud" block (provider-CLI auth overrides)
}

// ResolveTarget is the pure profile_resolve port shared by every host-aware
// command:
//
//	profile selection: --profile > KAMPODRA_PROFILE > config defaultProfile
//	host:  --host flag > profile host > KAMPODRA_HOST
//	key:   --ssh-key flag > profile sshKey > KAMPODRA_SSH_KEY
//	proxy: profile project.proxyHost > KAMPODRA_PROXY_HOST > project default
//	       (the profile's legacy flat proxyHost field still wins when set)
//	project: flags > KAMPODRA_* env > profile "project" block >
//	         kampodra.json manifest (nil = none) > project defaults
//
// An explicit per-invocation flag always wins; session env beats the
// persisted layers. An unknown profile name fails closed naming the known
// set.
func ResolveTarget(cfg *state.Config, flagHost, flagKey, flagProfile string, manifest *project.Manifest, lookup func(string) (string, bool)) (Target, error) {
	name, err := state.SelectProfileName(cfg, flagProfile, lookup)
	if err != nil {
		return Target{}, err
	}
	var profile state.Profile
	if name != "" {
		profile, err = cfg.Profile(name)
		if err != nil {
			return Target{}, err
		}
	}

	pc := project.Resolve(manifest, profile.Project, lookup)
	proxy := pc.ProxyHost
	if profile.ProxyHost != "" {
		// The legacy flat field predates the project block; explicit config
		// wins over the default either way.
		proxy = profile.ProxyHost
	}

	host := firstNonEmpty(flagHost, profile.Host, envValue(lookup, "KAMPODRA_HOST"))
	key := firstNonEmpty(flagKey, profile.SSHKey, envValue(lookup, "KAMPODRA_SSH_KEY"))

	return Target{
		HostSpec:    transport.HostSpec{Host: host, SSHKey: key},
		ProxyHost:   proxy,
		ProfileName: name,
		ProfileInit: profile.Init,
		Project:     pc,
		Cloud:       profile.Cloud,
	}, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func envValue(lookup func(string) (string, bool), key string) string {
	v, _ := lookup(key)
	return v
}
