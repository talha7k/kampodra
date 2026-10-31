# Publishing a release

Runbook: `docs/publish.md`. Short version:

1. `make bump VERSION=x.y.z` — bumps versions, stamps, commits, tags.
2. `git push origin main vx.y.z`
3. `make publish` — tests, cross-compile, npm publish (4 platform packages + root).

Auth: `NPM_TOKEN` via varlock → pass (`kampodra/npm-token`). The token must be
a Granular **Automation** type — regular tokens hit npm's publish OTP wall.
Prerelease versions get npm dist-tag `beta`, stable get `latest`.
Publishing after the tag (post-tag commits on HEAD) needs `make publish-tagged`
or `KAMPODRA_ALLOW_POST_TAG=1`.
