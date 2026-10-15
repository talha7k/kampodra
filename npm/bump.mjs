#!/usr/bin/env node
// bump: release-prep for the Go+npm CLI. The release version lives in TWO
// hand-edited places — npm/package.json and the cmd/kampodra/main.go source
// pin — and a missed pin edit is only caught by the pin test at publish
// time. This script edits that pair in one validated step: strict semver,
// forward-only (published versions and git tags are immutable), clean tree.
// The four platform packages and the root optionalDependencies pins are NOT
// hand-edited: `make cross-compile` stamps them from npm/package.json.
//
// `make bump` drives this (stamp -> verify -> commit -> tag); see Makefile.
//
// Zero dependencies. Git and file access are injectable (runGit / io) so the
// tests never shell out against the real repo.
import { execFileSync } from "node:child_process";
import { readFile, writeFile } from "node:fs/promises";
import { dirname, join } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";

const USAGE = `usage: node npm/bump.mjs <version> [--dry-run]

  <version>  strict semver MAJOR.MINOR.PATCH[-PRERELEASE]; build metadata
             (+...) is deliberately rejected — npm publishes here never use it
  --dry-run  run every validation and print the plan; edit nothing

  make bump VERSION=x.y.z[-pre.N] [DRY=1] is the usual entrypoint.`;

/** Thrown for bad CLI usage; the CLI main maps it to exit code 2. */
export class UsageError extends Error {}

// Deliberately NO `+build-metadata` alternative: npm publishes from this repo
// never use it, and the git tag is `v<version>` verbatim — accepting `+...`
// would only invite tag/publish drift.
export const SEMVER_RE = /^\d+\.\d+\.\d+(-[0-9A-Za-z.-]+(\.[0-9A-Za-z.-]+)*)?$/;

/** `{version, dryRun}` from argv; anything else throws with usage text. */
export function parseArgs(argv) {
  const positionals = [];
  let dryRun = false;
  for (const arg of argv) {
    if (arg === "--dry-run") dryRun = true;
    else if (arg.startsWith("-")) throw new UsageError(`unknown flag: ${arg}\n\n${USAGE}`);
    else positionals.push(arg);
  }
  if (positionals.length === 0) throw new UsageError(`missing <version>\n\n${USAGE}`);
  if (positionals.length > 1) {
    throw new UsageError(`expected exactly one <version>, got: ${positionals.join(" ")}\n\n${USAGE}`);
  }
  return { version: positionals[0], dryRun };
}

export function isValidSemver(v) {
  return SEMVER_RE.test(v);
}

// Full semver.org precedence: numeric core fields, then prerelease
// identifiers (numeric compared numerically, alphanumeric lexically
// (ASCII), numeric < alphanumeric, longer set wins when prefix-equal, and
// NO prerelease > any prerelease). No libraries.
export function cmpSemver(a, b) {
  const pa = split(a);
  const pb = split(b);
  for (const k of ["major", "minor", "patch"]) {
    if (pa[k] !== pb[k]) return pa[k] < pb[k] ? -1 : 1;
  }
  if (!pa.pre && !pb.pre) return 0;
  if (!pa.pre) return 1; // no prerelease > any prerelease
  if (!pb.pre) return -1;
  const xs = pa.pre.split(".");
  const ys = pb.pre.split(".");
  for (let i = 0; i < Math.max(xs.length, ys.length); i++) {
    const x = xs[i];
    const y = ys[i];
    if (x === undefined) return -1; // prefix-equal: longer set wins
    if (y === undefined) return 1;
    const xNum = /^\d+$/.test(x);
    const yNum = /^\d+$/.test(y);
    if (xNum && yNum) {
      if (x !== y) return Number(x) < Number(y) ? -1 : 1;
    } else if (xNum) return -1; // numeric < alphanumeric
    else if (yNum) return 1;
    else if (x !== y) return x < y ? -1 : 1;
  }
  return 0;
}

function split(v) {
  const dash = v.indexOf("-");
  const core = (dash === -1 ? v : v.slice(0, dash)).split(".").map(Number);
  return { major: core[0], minor: core[1], patch: core[2], pre: dash === -1 ? null : v.slice(dash + 1) };
}

/** Versions only move forward — tags and published packages are immutable. */
export function assertForward(current, next) {
  if (cmpSemver(next, current) <= 0) {
    throw new Error(
      `refusing to bump ${current} -> ${next}: published versions and git tags are immutable — the next release must be strictly greater than ${current}`,
    );
  }
}

/**
 * Require `git status --porcelain --untracked-files=no` to be empty.
 * Untracked files are deliberately ignored: the release commit stages
 * explicit paths, so untracked files can never leak into it — and this
 * script itself sits untracked in the repo for a while. Git access is
 * injected so tests never shell out against the real repo.
 */
export async function assertCleanTree(runGit) {
  const out = String(await runGit(["status", "--porcelain", "--untracked-files=no"])).trim();
  if (out) {
    const files = out
      .split("\n")
      .map((line) => "  " + line)
      .join("\n");
    throw new Error(
      `dirty tracked files — the release commit must contain exactly the version files:\n${files}\ncommit or clean them, then re-run`,
    );
  }
}

/**
 * Edit the two hand-edited version homes. Both files are read and validated
 * BEFORE either is written: a failed pin rewrite must never leave
 * package.json bumped while main.go lags — that split state is exactly the
 * drift the pin test catches at publish time.
 *
 * package.json is re-serialized with `JSON.stringify(p, null, 2) + "\n"` —
 * byte-identical to `make cross-compile`'s stamping pass, so the later stamp
 * is a no-op on these bytes.
 */
export async function bumpFiles({ npmPackageJsonPath, mainGoPath }, version, io) {
  const pkg = JSON.parse(await io.readFile(npmPackageJsonPath, "utf8"));
  pkg.version = version;

  const mainRaw = await io.readFile(mainGoPath, "utf8");
  const pinRe = /^var version = "[^"]*"$/gm;
  const hits = mainRaw.match(pinRe);
  if (!hits || hits.length !== 1) {
    throw new Error(
      `expected exactly one 'var version = "..."' line in ${mainGoPath}, found ${hits ? hits.length : 0} — fix the file, do not hand-wave the pin`,
    );
  }

  await io.writeFile(npmPackageJsonPath, JSON.stringify(pkg, null, 2) + "\n");
  await io.writeFile(mainGoPath, mainRaw.replace(pinRe, `var version = "${version}"`));
}

const npmPackageJsonPath = join(dirname(fileURLToPath(import.meta.url)), "package.json");
const mainGoPath = join(dirname(fileURLToPath(import.meta.url)), "..", "cmd", "kampodra", "main.go");

function runGit(args) {
  return execFileSync("git", args, { encoding: "utf8" });
}

export async function main(argv = process.argv.slice(2)) {
  const { version, dryRun } = parseArgs(argv);
  if (!isValidSemver(version)) {
    throw new UsageError(
      `not strict semver: ${version}\nexpected MAJOR.MINOR.PATCH[-PRERELEASE]; build metadata (+...) is deliberately rejected\n\n${USAGE}`,
    );
  }
  const current = JSON.parse(await readFile(npmPackageJsonPath, "utf8")).version;
  assertForward(current, version);
  await assertCleanTree(runGit);

  if (dryRun) {
    console.log(`dry-run: bump ${current} -> ${version}`);
    console.log(`  would edit: ${npmPackageJsonPath}`);
    console.log(`  would edit: ${mainGoPath}`);
    console.log(`  then (make bump): cross-compile stamps the 4 platform packages + optionalDeps pins -> release-check -> pin test -> commit "release ${version}" -> tag v${version}`);
    console.log("  no files edited, nothing committed. run for real: make bump VERSION=" + version);
    return;
  }

  await bumpFiles({ npmPackageJsonPath, mainGoPath }, version, { readFile, writeFile });
  console.log(`bump ${current} -> ${version}`);
  console.log(`  edited: ${npmPackageJsonPath}`);
  console.log(`  edited: ${mainGoPath}`);
  console.log(`make bump continues from here: stamps platform pins, commits "release ${version}", tags v${version}`);
  console.log("next:");
  console.log(`  git push origin main v${version}`);
  console.log("  make publish");
}

// Only run when executed directly — importing this file must stay
// side-effect-free (the tests import it against the real repo paths).
if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  main().catch((err) => {
    console.error(err.message);
    process.exit(err instanceof UsageError ? 2 : 1);
  });
}
