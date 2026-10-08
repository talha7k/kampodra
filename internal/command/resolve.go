package command

import (
	"github.com/talha7k/kampodine-go/internal/adapter/state"
	"github.com/talha7k/kampodine-go/internal/adapter/transport"
)

// defaultProxyHost is APP_HOST_HEADER's default (the shell scripts'
// constant).
const defaultProxyHost = "app.example.com"

// Target is a fully resolved run target: where to ssh, what the public
// edge hostname is, and the profile that contributed values.
type Target struct {
	HostSpec    transport.HostSpec
	ProxyHost   string
	ProfileName string
	ProfileInit string // cached init verdict from the profile (probe skip)
}

// ResolveTarget is the pure profile_resolve port shared by every host-aware
// command:
//
//	profile selection: --profile > KAMPODINE_PROFILE > config defaultProfile
//	host:  --host flag > profile host > KAMPODINE_HOST
//	key:   --ssh-key flag > profile sshKey > KAMPODINE_SSH_KEY
//	proxy: profile proxyHost > APP_HOST_HEADER > app.example.com
//
// An explicit per-invocation flag always wins; the profile beats the legacy
// env it replaces (the shell's documented contract). An unknown profile
// name fails closed naming the known set.
func ResolveTarget(cfg *state.Config, flagHost, flagKey, flagProfile string, lookup func(string) (string, bool)) (Target, error) {
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

	host := firstNonEmpty(flagHost, profile.Host, envValue(lookup, "KAMPODINE_HOST"))
	key := firstNonEmpty(flagKey, profile.SSHKey, envValue(lookup, "KAMPODINE_SSH_KEY"))
	proxy := firstNonEmpty(profile.ProxyHost, envValue(lookup, "APP_HOST_HEADER"), defaultProxyHost)

	return Target{
		HostSpec:    transport.HostSpec{Host: host, SSHKey: key},
		ProxyHost:   proxy,
		ProfileName: name,
		ProfileInit: profile.Init,
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
