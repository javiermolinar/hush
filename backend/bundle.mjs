import { build } from "esbuild";
import { createHash } from "node:crypto";
import { readFileSync, writeFileSync } from "node:fs";
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
async function worker(entry, define, plugins = common.plugins) {
  const result = check(await build({ ...common, entryPoints: [entry], write: false, define, plugins,
    // In a data: URL worker, import.meta.url is not a valid createRequire base.
    banner: { js: 'import { createRequire as hushCreateRequire } from "node:module"; const require = hushCreateRequire(process.execPath);' },
  }));
  return {
    url: "data:text/javascript;base64," + Buffer.from(result.outputFiles[0].contents).toString("base64"),
    meta: result.metafile,
  };
}

// Use the verified source build, including its own wasm-bindgen glue. The
// registry WASM has no build inventory tied to its bytes. See app/wasm/README.md.
const glue = fileURLToPath(new URL("wasm/photon_rs.js", import.meta.url));
function pinned(path, want) {
  const bytes = readFileSync(path);
  if (createHash("sha256").update(bytes).digest("hex") !== want) throw new Error(`unexpected Photon asset: ${path}`);
  return bytes;
}
pinned(glue, "f69b97e56075b43a6f0a73447a405afe3f02f1903d056fb10e732dd1c9f52d46");
const photon = pinned(new URL("wasm/photon_rs_bg.wasm", import.meta.url),
  "65f13dd50eade4c615794aca1f9e689374e08a3bf563902eba57ce570c89ba21");
const photonGlue = { name: "pinned-photon-glue", setup(builder) {
  builder.onResolve({ filter: /^@silvia-odwyer\/photon\/photon_rs\.js$/ }, () => ({ path: glue }));
} };
const codemode = createRequire(import.meta.resolve("@earendil-works/pi-codemode"));
const quickjs = readFileSync(codemode.resolve("quickjs-wasi/quickjs.wasm"));
const [imageWorker, codemodeWorker] = await Promise.all([
  worker("backend/src/image-worker.ts", { HUSH_PHOTON_WASM: JSON.stringify(photon.toString("base64")) }, [...common.plugins, photonGlue]),
  worker(fileURLToPath(import.meta.resolve("@earendil-works/pi-codemode/worker"))),
]);
const main = check(await build({ ...common, entryPoints: ["backend/src/index.ts"], outfile: "dist/hush-backend.mjs",
  define: {
    HUSH_IMAGE_WORKER: JSON.stringify(imageWorker.url),
    HUSH_CODEMODE_WORKER: JSON.stringify(codemodeWorker.url),
    HUSH_QUICKJS_WASM: JSON.stringify(quickjs.toString("base64")),
    HUSH_SYSTEM_PROMPT: JSON.stringify(readFileSync("backend/src/prompts/system.md", "utf8")),
  } }));
writeFileSync("dist/hush-backend-inputs.json", JSON.stringify({ main: main.metafile, image: imageWorker.meta, codemode: codemodeWorker.meta }));
