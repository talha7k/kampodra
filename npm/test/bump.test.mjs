// Tests for npm/bump.mjs (node:test, zero deps). Everything with side
// effects is injected: semver math is pure, file I/O hits tmpdir fixtures,
// and git never runs against the real repo from here.
import { test } from "node:test";
import assert from "node:assert/strict";
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { readFile, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

import {
  UsageError,
  assertForward,
  bumpFiles,
  cmpSemver,
  isValidSemver,
  parseArgs,
} from "../bump.mjs";

const io = { readFile, writeFile };

// Mirrors cmd/kampodra/main.go's shape: one `var version = "..."` pin line.
const MAIN_GO = [
  "package main",
  "",
  "// version is stamped at release time via -ldflags \"-X main.version=…\".",
  'var version = "0.7.0-beta.9"',
  "",
  "func main() {}",
  "",
].join("\n");

// Fixtures live in a tmpdir and are cleaned up with the test. The package.json
// fixture is written with the exact serialization `make cross-compile` uses,
// so the byte-identity assertion below measures the bump, not the fixture.
function fixture(t, mainGo) {
  const root = mkdtempSync(join(tmpdir(), "kampodra-bump-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const pkg = {
    name: "kampodra",
    version: "0.7.0-beta.9",
    optionalDependencies: { "@talha7k/kampodra-darwin-arm64": "0.7.0-beta.9" },
  };
  writeFileSync(join(root, "package.json"), JSON.stringify(pkg, null, 2) + "\n");
  writeFileSync(join(root, "main.go"), mainGo);
  return { npmPackageJsonPath: join(root, "package.json"), mainGoPath: join(root, "main.go") };
}

test("isValidSemver: accepts strict core + prerelease forms", () => {
  const accepted = [
    "0.0.0",
    "1.2.3",
    "10.20.30",
    "0.7.0-beta.9",
    "1.0.0-alpha",
    "1.0.0-alpha.1",
    "1.0.0-rc.1",
    "1.0.0-0.3.7",
  ];
  for (const v of accepted) assert.equal(isValidSemver(v), true, v);
});

test("isValidSemver: rejects loose forms, including build metadata (deliberate)", () => {
  const rejected = [
    "",
    "1",
    "1.2",
    "1.2.3.4",
    "v1.2.3",
    "1.2.3-",
    "not-semver",
    " 1.2.3",
    "1.2.3 ",
    // Build metadata: npm publishes from this repo never use `+...`, so the
    // gate rejects it up front instead of letting it drift into the tag.
    "1.2.3+build.1",
    "1.2.3-beta.1+build.1",
  ];
  for (const v of rejected) assert.equal(isValidSemver(v), false, JSON.stringify(v));
});

test("cmpSemver: canonical semver.org precedence ordering, verbatim, pairwise", () => {
  const chain = [
    "1.0.0-alpha",
    "1.0.0-alpha.1",
    "1.0.0-alpha.beta",
    "1.0.0-beta",
    "1.0.0-beta.2",
    "1.0.0-beta.11",
    "1.0.0-rc.1",
    "1.0.0",
  ];
  for (let i = 0; i < chain.length; i++) {
    for (let j = 0; j < chain.length; j++) {
      const expected = i === j ? 0 : i < j ? -1 : 1;
      assert.equal(cmpSemver(chain[i], chain[j]), expected, `${chain[i]} vs ${chain[j]}`);
    }
  }
});

test("cmpSemver: repo-realistic orderings", () => {
  assert.equal(cmpSemver("0.7.0-beta.8", "0.7.0-beta.9"), -1);
  assert.equal(cmpSemver("0.7.0-beta.9", "0.7.0"), -1); // prerelease -> stable
  assert.equal(cmpSemver("0.7.0", "0.8.0-alpha.1"), -1); // stable -> next prerelease
  assert.equal(cmpSemver("1.2.3", "1.2.3"), 0);
  // beta.11 > beta.2 is numeric, not lexical — the exact bug a naive
  // string compare ships.
  assert.equal(cmpSemver("0.7.0-beta.11", "0.7.0-beta.2"), 1);
});

test("assertForward: rejects equal and backwards; accepts forward incl. prerelease->stable", () => {
  assert.throws(() => assertForward("0.7.0-beta.9", "0.7.0-beta.9"), /immutable|greater/);
  assert.throws(() => assertForward("0.7.0-beta.9", "0.7.0-beta.8"), /immutable|greater/);
  assert.throws(() => assertForward("0.7.0", "0.7.0-beta.9"), /immutable|greater/); // stable -> old prerelease
  assert.doesNotThrow(() => assertForward("0.7.0-beta.9", "0.7.0-beta.10"));
  assert.doesNotThrow(() => assertForward("0.7.0-beta.9", "0.7.0")); // prerelease -> stable
  assert.doesNotThrow(() => assertForward("0.7.0", "0.8.0-alpha.1"));
});

test("bumpFiles: rewrites both files; JSON bytes stay in the cross-compile convention (idempotent)", async (t) => {
  const paths = fixture(t, MAIN_GO);
  await bumpFiles(paths, "0.7.0-beta.10", io);
  const raw = readFileSync(paths.npmPackageJsonPath, "utf8");
  const pkg = JSON.parse(raw);
  assert.equal(pkg.version, "0.7.0-beta.10");
  // bump edits ONLY the "version" field — the optionalDependencies pins keep
  // the old version until `make cross-compile` stamps them (its job, not
  // ours; the pin test proves the stamp happened).
  assert.equal(pkg.optionalDependencies["@talha7k/kampodra-darwin-arm64"], "0.7.0-beta.9");
  // Byte-identity: re-serializing what was written reproduces the file
  // exactly — this is the convention `make cross-compile` stamps with, so
  // the later stamping pass is a no-op on these bytes.
  assert.equal(raw, JSON.stringify(pkg, null, 2) + "\n");
  // ...and re-running the same serialization is idempotent.
  assert.equal(JSON.stringify(JSON.parse(raw), null, 2) + "\n", raw);
});

test("bumpFiles: replaces the main.go pin and nothing else", async (t) => {
  const paths = fixture(t, MAIN_GO);
  await bumpFiles(paths, "0.7.0-beta.10", io);
  const raw = readFileSync(paths.mainGoPath, "utf8");
  assert.ok(raw.includes('var version = "0.7.0-beta.10"'), "pin rewritten");
  assert.ok(!raw.includes("0.7.0-beta.9"), "stale pin gone");
  assert.ok(raw.includes('via -ldflags "-X main.version=…"'), "comment line untouched");
  assert.equal(raw.split("var version =").length - 1, 1, "still exactly one pin line");
});

test("bumpFiles: throws on 0 pin matches (main.go drift is a hard stop)", async (t) => {
  const paths = fixture(t, MAIN_GO.replace('var version = "0.7.0-beta.9"\n', ""));
  await assert.rejects(() => bumpFiles(paths, "0.7.0-beta.10", io), /exactly one/);
});

test("bumpFiles: throws on 2 pin matches (ambiguous file is a hard stop)", async (t) => {
  const paths = fixture(t, MAIN_GO.replace("func main() {}", 'var version = "0.7.0-beta.9"\n\nfunc main() {}'));
  await assert.rejects(() => bumpFiles(paths, "0.7.0-beta.10", io), /exactly one/);
});

test("bumpFiles: a failed main.go rewrite leaves package.json untouched (read-then-write)", async (t) => {
  const paths = fixture(t, MAIN_GO.replace('var version = "0.7.0-beta.9"\n', ""));
  await assert.rejects(() => bumpFiles(paths, "0.7.0-beta.10", io));
  const raw = readFileSync(paths.npmPackageJsonPath, "utf8");
  assert.equal(JSON.parse(raw).version, "0.7.0-beta.9");
});

test("parseArgs: exactly one positional plus optional --dry-run", () => {
  assert.deepEqual(parseArgs(["0.7.0-beta.10"]), { version: "0.7.0-beta.10", dryRun: false });
  assert.deepEqual(parseArgs(["0.7.0-beta.10", "--dry-run"]), { version: "0.7.0-beta.10", dryRun: true });
  assert.deepEqual(parseArgs(["--dry-run", "0.7.0-beta.10"]), { version: "0.7.0-beta.10", dryRun: true });
});

test("parseArgs: usage errors throw UsageError with usage text", () => {
  const cases = [
    [], // 0 args
    ["0.7.0-beta.10", "0.8.0"], // 2 positionals
    ["0.7.0-beta.10", "--bogus"], // unknown flag
    ["--dry-run"], // flag only, no version
  ];
  for (const argv of cases) {
    assert.throws(
      () => parseArgs(argv),
      (err) => err instanceof UsageError && /usage:/.test(err.message),
      JSON.stringify(argv),
    );
  }
});

// Read-only canary against the real repo: the file the release flow stamps
// must already be byte-stable under the cross-compile convention, or the very
// first cross-compile after a hand-edit would reformat it.
test("real npm/package.json is byte-stable under the cross-compile serialization", () => {
  const npmDir = join(dirname(fileURLToPath(import.meta.url)), "..");
  const raw = readFileSync(join(npmDir, "package.json"), "utf8");
  assert.equal(raw, JSON.stringify(JSON.parse(raw), null, 2) + "\n");
});
