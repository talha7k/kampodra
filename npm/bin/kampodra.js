#!/usr/bin/env node
// kampodra bin shim (the esbuild-style platform-binary pattern): resolve
// this platform's native binary from the matching @kampodra/<platform>
// optionalDependency and hand it the argv verbatim. Clear, actionable error
// when the binary is missing — never a raw stack trace.
import { spawnSync } from "node:child_process";
import { existsSync } from "node:fs";
import { createRequire } from "node:module";

const SUPPORTED = new Set([
  "darwin-arm64",
  "darwin-x64",
  "linux-arm64",
  "linux-x64",
]);

const platform = `${process.platform}-${process.arch}`;
const require = createRequire(import.meta.url);

let bin;
try {
  bin = require.resolve(`@kampodra/${platform}/bin/kampodra`);
} catch (err) {
  console.error(`kampodra: no native binary for ${platform}.`);
  if (!SUPPORTED.has(platform)) {
    console.error(
      `  ${platform} is not supported (supported: ${[...SUPPORTED].sort().join(", ")}).`,
    );
  } else {
    console.error(
      `  The optionalDependency @kampodra/${platform} was not installed.`,
    );
    console.error(
      `  npm installs optional dependencies by default — check --omit=optional / "optional": false in your config, then reinstall.`,
    );
  }
  if (process.env.KAMPODRA_DEBUG) {
    console.error(`  resolve error: ${err?.message ?? err}`);
  }
  process.exit(1);
}

if (!existsSync(bin)) {
  console.error(
    `kampodra: @kampodra/${platform} is installed but ${bin} is missing — the package is broken, reinstall it.`,
  );
  process.exit(1);
}

const result = spawnSync(bin, process.argv.slice(2), { stdio: "inherit" });
if (result.error) {
  console.error(`kampodra: ${result.error.message}`);
  process.exit(1);
}
process.exit(result.status ?? 1);
