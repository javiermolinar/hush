// Run after the build/release from the checkout root. Optional arguments are
// paths to exact release archives to include in the size report.
import { execFileSync } from "node:child_process";
import { readFileSync, statSync } from "node:fs";
import { sep } from "node:path";

const json = path => JSON.parse(readFileSync(path, "utf8"));
const { packages } = json("package-lock.json");
const { main, image, codemode, wasm } = json("dist/hush-backend-inputs.json");
const pkg = path => /^(.*node_modules\/(?:@[^/]+\/)?[^/]+)\//.exec(path)?.[1];
const rows = Object.entries(packages).filter(([p, meta]) => p.includes("node_modules/") && !meta.link);
const installed = execFileSync("npm", ["ls", "--omit=dev", "--all", "--parseable"], { encoding: "utf8" }).trim().split("\n");
const installedRuntime = new Set(installed.filter(path => path.includes(sep + "node_modules" + sep))
  .map(path => path.slice(path.indexOf(sep + "node_modules" + sep) + 1)).filter(path => packages[path] && !packages[path].dev));
const bundled = new Map();
for (const meta of [main, image, codemode]) {
  for (const output of Object.values(meta.outputs)) {
    for (const [path, info] of Object.entries(output.inputs)) {
      const name = pkg(path);
      if (name && info.bytesInOutput) bundled.set(name, (bundled.get(name) ?? 0) + info.bytesInOutput);
    }
  }
}
// Explicit WASM bytes are not JS contributors in esbuild's metafile.
bundled.set("node_modules/quickjs-wasi", (bundled.get("node_modules/quickjs-wasi") ?? 0) + wasm.quickjs.length);
const piEdges = Object.fromEntries(Object.entries(main.inputs).flatMap(([from, input]) => {
  const imports = input.imports.filter(dep => /node_modules\/@earendil-works\/pi-(coding-agent|tui)\//.test(dep.path) && pkg(dep.path) !== pkg(from));
  return imports.length ? [[from, imports.map(dep => dep.original ?? dep.path)]] : [];
}));
const byBytes = [...bundled].sort((a, b) => b[1] - a[1]);
const entry = "dist/hush-backend.mjs";
console.log(JSON.stringify({ node: process.version, platform: process.platform, arch: process.arch,
  bundleBytes: statSync(entry).size, wasmPackBytes: statSync(entry + ".wasm-pack").size,
  archives: Object.fromEntries(process.argv.slice(2).map(path => [path, statSync(path).size])),
  npm: { lockEntries: rows.length, lockRuntimeEntries: rows.filter(([, meta]) => !meta.dev).length,
    installedRuntimeEntries: installedRuntime.size, shippedContributors: bundled.size },
  contributors: byBytes.map(([path, bytes]) => ({ path, bytes, lockDevelopmentOnly: packages[path]?.dev === true })),
  installedOnly: [...installedRuntime].filter(path => !bundled.has(path)).sort(),
  piEdges,
}, null, 2));
