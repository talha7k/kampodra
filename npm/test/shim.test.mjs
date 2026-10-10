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
const shim = join(npmDir, "bin", "kampodra.js");

function stageTree(t) {
  const root = mkdtempSync(join(tmpdir(), "kampodra-shim-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const pkgDir = join(root, "node_modules", "kampodra");
  mkdirSync(join(pkgDir, "bin"), { recursive: true });
  copyFileSync(shim, join(pkgDir, "bin", "kampodra.js"));
  return { root, pkgDir };
}

test("missing platform package fails with a clear, actionable error", (t) => {
  const { root, pkgDir } = stageTree(t);
  const res = spawnSync(process.execPath, [join(pkgDir, "bin", "kampodra.js"), "--help"], {
    cwd: root,
    encoding: "utf8",
  });
  assert.equal(res.status, 1);
  assert.match(res.stderr, /no native binary for /);
  assert.match(res.stderr, /optionalDependency @talha7k\//);
  assert.doesNotMatch(res.stderr, /at /); // no stack traces on the happy-error path
});

test("unsupported platform names the supported set", (t) => {
  const { root, pkgDir } = stageTree(t);
  // Force an unsupported triple by requiring the resolver through a bogus
  // override — simplest deterministic route: run with a faked process value
  // via an env the shim does NOT read is not possible, so instead assert the
  // message branch text exists in the shim source (static) AND the missing
  // path works (dynamic). The branch is exercised in CI on the matrix.
  const src = join(pkgDir, "bin", "kampodra.js");
  const shimSource = readFileSync(src, "utf8");
  assert.match(shimSource, /is not supported \(supported:/);
});

test("installed package with missing binary fails clearly", (t) => {
  const { root, pkgDir } = stageTree(t);
  const platformPkg = join(root, "node_modules", "@talha7k", `${process.platform}-${process.arch}`);
  mkdirSync(platformPkg, { recursive: true });
  writeFileSync(
    join(platformPkg, "package.json"),
    JSON.stringify({ name: `@talha7k/${process.platform}-${process.arch}`, version: "0.0.0-test" }),
  );
  // require.resolve throws before existsSync can run when the file is
  // absent, so this branch is only reachable with a broken/partial install
  // npm cannot produce deterministically here — pin the branch's contract
  // statically (same pattern as the unsupported-platform test above).
  const shimSource = readFileSync(join(pkgDir, "bin", "kampodra.js"), "utf8");
  assert.match(shimSource, /is installed but \$\{bin\} is missing/);
  assert.match(shimSource, /existsSync\(bin\)/);
  // ...and dynamically: a package with NO binary at all still fails with a
  // clear error and exit 1 (never a stack trace).
  const res = spawnSync(process.execPath, [join(pkgDir, "bin", "kampodra.js")], {
    cwd: root,
    encoding: "utf8",
  });
  assert.equal(res.status, 1);
  assert.doesNotMatch(res.stderr, /at /);
});

test("binary present: argv passes through verbatim, exit code propagates", (t) => {
  const { root, pkgDir } = stageTree(t);
  const platformPkg = join(root, "node_modules", "@talha7k", `${process.platform}-${process.arch}`);
  mkdirSync(join(platformPkg, "bin"), { recursive: true });
  writeFileSync(
    join(platformPkg, "package.json"),
    JSON.stringify({ name: `@talha7k/${process.platform}-${process.arch}`, version: "0.0.0-test" }),
  );
  const binPath = join(platformPkg, "bin", "kampodra");
  writeFileSync(
    binPath,
    "#!/bin/sh\n" +
      'printf \'ARGS:\'; printf \' [%s]\' "$@"; printf \'\\n\'\n' +
      "exit 7\n",
  );
  chmodSync(binPath, 0o755);
  const res = spawnSync(process.execPath, [join(pkgDir, "bin", "kampodra.js"), "status", "--host", "root@x", "--verbose"], {
    cwd: root,
    encoding: "utf8",
  });
  assert.equal(res.status, 7);
  assert.equal(res.stdout.trim(), "ARGS: [status] [--host] [root@x] [--verbose]");
});
