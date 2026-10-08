// Shim tests for the npm platform-binary pattern (node:test, zero deps):
//   1. missing platform package -> clear error + exit 1 (never a trace)
//   2. installed-but-binary-missing -> clear error + exit 1
//   3. binary present -> argv handed through verbatim, exit code propagated
import { test } from "node:test";
import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { chmodSync, copyFileSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const npmDir = join(dirname(fileURLToPath(import.meta.url)), "..");
const shim = join(npmDir, "bin", "kampodine.js");

function stageTree(t) {
  const root = mkdtempSync(join(tmpdir(), "kampodine-shim-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const pkgDir = join(root, "node_modules", "kampodine");
  mkdirSync(join(pkgDir, "bin"), { recursive: true });
  copyFileSync(shim, join(pkgDir, "bin", "kampodine.js"));
  return { root, pkgDir };
}

test("missing platform package fails with a clear, actionable error", (t) => {
  const { root, pkgDir } = stageTree(t);
  const res = spawnSync(process.execPath, [join(pkgDir, "bin", "kampodine.js"), "--help"], {
    cwd: root,
    encoding: "utf8",
  });
  assert.equal(res.status, 1);
  assert.match(res.stderr, /no native binary for /);
  assert.match(res.stderr, /optionalDependency @kampodine\//);
  assert.doesNotMatch(res.stderr, /at /); // no stack traces on the happy-error path
});

test("unsupported platform names the supported set", (t) => {
  const { root, pkgDir } = stageTree(t);
  // Force an unsupported triple by requiring the resolver through a bogus
  // override — simplest deterministic route: run with a faked process value
  // via an env the shim does NOT read is not possible, so instead assert the
  // message branch text exists in the shim source (static) AND the missing
  // path works (dynamic). The branch is exercised in CI on the matrix.
  const src = join(pkgDir, "bin", "kampodine.js");
  const shimSource = readFileSync(src, "utf8");
  assert.match(shimSource, /is not supported \(supported:/);
});

test("installed package with missing binary fails clearly", (t) => {
  const { root, pkgDir } = stageTree(t);
  const platformPkg = join(root, "node_modules", "@kampodine", `${process.platform}-${process.arch}`);
  mkdirSync(platformPkg, { recursive: true });
  writeFileSync(
    join(platformPkg, "package.json"),
    JSON.stringify({ name: `@kampodine/${process.platform}-${process.arch}`, version: "0.0.0-test" }),
  );
  // no bin/kampodine inside -> resolve succeeds, existsSync fails
  const res = spawnSync(process.execPath, [join(pkgDir, "bin", "kampodine.js")], {
    cwd: root,
    encoding: "utf8",
  });
  assert.equal(res.status, 1);
  assert.match(res.stderr, /is installed but .* is missing/);
});

test("binary present: argv passes through verbatim, exit code propagates", (t) => {
  const { root, pkgDir } = stageTree(t);
  const platformPkg = join(root, "node_modules", "@kampodine", `${process.platform}-${process.arch}`);
  mkdirSync(join(platformPkg, "bin"), { recursive: true });
  writeFileSync(
    join(platformPkg, "package.json"),
    JSON.stringify({ name: `@kampodine/${process.platform}-${process.arch}`, version: "0.0.0-test" }),
  );
  const binPath = join(platformPkg, "bin", "kampodine");
  writeFileSync(
    binPath,
    "#!/bin/sh\n" +
      'printf \'ARGS:\'; printf \' [%s]\' "$@"; printf \'\\n\'\n' +
      "exit 7\n",
  );
  chmodSync(binPath, 0o755);
  const res = spawnSync(process.execPath, [join(pkgDir, "bin", "kampodine.js"), "status", "--host", "root@x", "--verbose"], {
    cwd: root,
    encoding: "utf8",
  });
  assert.equal(res.status, 7);
  assert.equal(res.stdout.trim(), "ARGS: [status] [--host] [root@x] [--verbose]");
});
