package envfile

import "testing"

// RemoteTmpPath is shared by `env push` and the deploy pipeline's env step
// — both must upload through the SAME 0600-from-creation temp shape.

func TestRemoteTmpPath(t *testing.T) {
	got := RemoteTmpPath("/etc/kampodra/env", 4242)
	want := "/etc/kampodra/env.tmp.4242"
	if got != want {
		t.Errorf("RemoteTmpPath() = %q, want %q", got, want)
	}
	// A remote path with a trailing slash or different basename must keep
	// the temp in the same DIRECTORY (atomic mv needs same-filesystem).
	if got := RemoteTmpPath("/srv/app/settings", 7); got != "/srv/app/settings.tmp.7" {
		t.Errorf("RemoteTmpPath() = %q, want /srv/app/settings.tmp.7", got)
	}
}
