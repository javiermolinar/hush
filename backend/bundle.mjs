import { build } from "esbuild";
import { readFileSync } from "node:fs";
import { createRequire, isBuiltin } from "node:module";
import { fileURLToPath } from "node:url";

process.chdir(fileURLToPath(new URL("..", import.meta.url)));
const common = {
  bundle: true,
  platform: "node",
  target: "node22.19",
  format: "esm",
  legalComments: "eof",
  metafile: true,
  plugins: [{
    name: "no-optional-native-addons",
    setup(builder) {
      builder.onResolve({ filter: /^(bufferutil|utf-8-validate|supports-color)$/ }, args =>
        ({ path: args.path, namespace: "optional-disabled" }));
      // All three upstream callers catch module load failure and use the JS/no-color path.
      builder.onLoad({ filter: /.*/, namespace: "optional-disabled" }, () =>
        ({ contents: 'throw new Error("optional dependency disabled in hush");' }));
    },
  }],
  banner: { js: 'import { createRequire as hushCreateRequire } from "node:module"; const require = hushCreateRequire(import.meta.url);' },
};

function check(result) {
  for (const out of Object.values(result.metafile.outputs)) {
    for (const dep of out.imports) {
      if (dep.external && !isBuiltin(dep.path)) throw new Error("bundle requires external dependency " + dep.path);
    }
  }
  return result;
}

// Keep the backend a single deployable file. Worker assets are available even
// after the launcher removes the extracted backend and its directory.
async function workerURL(entry, define) {
  const worker = check(await build({ ...common, entryPoints: [entry], write: false, define,
    // In a data: URL worker, import.meta.url is not a valid createRequire base.
    banner: { js: 'import { createRequire as hushCreateRequire } from "node:module"; const require = hushCreateRequire(process.execPath);' },
  }));
  return "data:text/javascript;base64," + Buffer.from(worker.outputFiles[0].contents).toString("base64");
}
const require = createRequire(import.meta.url);
const durable = createRequire(require.resolve("@earendil-works/pi-durable/images"));
const photon = readFileSync(durable.resolve("@silvia-odwyer/photon/photon_rs_bg.wasm"));
const codemode = createRequire(import.meta.resolve("@earendil-works/pi-codemode"));
const quickjs = readFileSync(codemode.resolve("quickjs-wasi/quickjs.wasm"));
const [imageWorker, codemodeWorker] = await Promise.all([
  workerURL("backend/src/image-worker.ts", { HUSH_PHOTON_WASM: JSON.stringify(photon.toString("base64")) }),
  workerURL(fileURLToPath(import.meta.resolve("@earendil-works/pi-codemode/worker"))),
]);
check(await build({ ...common, entryPoints: ["backend/src/index.ts"], outfile: "dist/hush-backend.mjs",
  define: {
    HUSH_IMAGE_WORKER: JSON.stringify(imageWorker),
    HUSH_CODEMODE_WORKER: JSON.stringify(codemodeWorker),
    HUSH_QUICKJS_WASM: JSON.stringify(quickjs.toString("base64")),
    HUSH_SYSTEM_PROMPT: JSON.stringify(readFileSync("backend/src/prompts/system.md", "utf8")),
  } }));
