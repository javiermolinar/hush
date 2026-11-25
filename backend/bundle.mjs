import { build } from "esbuild";
import standaloneValidators from "ajv/dist/standalone/index.js";
import { createHash } from "node:crypto";
import { readFileSync, writeFileSync } from "node:fs";
import { createRequire, isBuiltin } from "node:module";
import { dirname } from "node:path";
import { fileURLToPath } from "node:url";
import { configureValidators } from "../server/src/validators.ts";

process.chdir(fileURLToPath(new URL("..", import.meta.url)));
if (process.argv.length > 2) throw new Error("usage: node backend/bundle.mjs");
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

// Worker JS stays inline. It can run even after the launcher removes the
// extracted backend and its directory; WASM is loaded separately on demand.
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
  worker("backend/src/image-worker.ts", {}, [...common.plugins, photonGlue]),
  worker(fileURLToPath(import.meta.resolve("@earendil-works/pi-codemode/worker"))),
]);
const slice = (bytes, offset) => ({ offset, length: bytes.length, sha256: createHash("sha256").update(bytes).digest("hex") });
const wasmIndex = { photon: slice(photon, 0), quickjs: slice(quickjs, photon.length) };
const wire = configureValidators(true);
const validators = standaloneValidators(wire.ajv, wire.exports);
const stem = "dist/hush-backend";
const entry = stem + ".mjs";
const main = check(await build({ ...common, entryPoints: ["backend/src/entry.ts"], outfile: entry,
  define: {
    HUSH_IMAGE_WORKER: JSON.stringify(imageWorker.url),
    HUSH_CODEMODE_WORKER: JSON.stringify(codemodeWorker.url),
    HUSH_WASM_INDEX: JSON.stringify(wasmIndex),
    HUSH_SYSTEM_PROMPT: JSON.stringify(readFileSync("backend/src/prompts/system.md", "utf8")),
  },
  plugins: [...common.plugins, { name: "precompiled-wire-validators", setup(builder) {
    builder.onLoad({ filter: /[/\\]server[/\\]src[/\\]validators\.ts$/ }, ({ path }) =>
      ({ contents: validators, resolveDir: dirname(path), loader: "js" }));
  } }],
}));
writeFileSync(entry + ".wasm-pack", Buffer.concat([photon, quickjs]));
writeFileSync(stem + "-inputs.json", JSON.stringify({
  main: main.metafile, image: imageWorker.meta, codemode: codemodeWorker.meta, wasm: wasmIndex,
}));
