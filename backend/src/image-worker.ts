import { readFileSync } from "node:fs";
import { createRequire } from "node:module";
import { parentPort, workerData } from "node:worker_threads";
import { createPhotonImages } from "@earendil-works/pi-durable/images";
import { fitImage } from "@earendil-works/pi-durable/images/fit";
import type { ImageLimits } from "@earendil-works/pi-durable/tools";
import type { ImageBytes } from "../../server/src/images.js";

const MAX_AXIS = 8192;
const MAX_PIXELS = 24_000_000;

function boundedImage(raw: ImageBytes): boolean {
  const d = Buffer.from(raw.data.buffer, raw.data.byteOffset, raw.data.byteLength);
  let w = 0, h = 0;
  switch (raw.mimeType) {
    case "image/png":
      if (d.length < 24 || d.subarray(0, 8).toString("hex") !== "89504e470d0a1a0a" ||
          d.readUInt32BE(8) !== 13 || d.toString("ascii", 12, 16) !== "IHDR") break;
      w = d.readUInt32BE(16); h = d.readUInt32BE(20);
      for (let start = 8; start + 8 <= d.length;) {
        const chunk = d.toString("ascii", start + 4, start + 8);
        if (chunk === "acTL") return false; // animated PNG isn't portable provider input
        if (chunk === "IDAT") break;
        start += d.readUInt32BE(start) + 12;
      }
      break;
    case "image/jpeg":
      if (d[0] !== 0xff || d[1] !== 0xd8) break;
      for (let i = 2; i + 9 <= d.length && d[i] === 0xff;) {
        const marker = d[i + 1];
        if (marker === 0xff) { i++; continue; }
        const length = d.readUInt16BE(i + 2);
        if (length < 2 || i + 2 + length > d.length || marker === 0xda || marker === 0xd9) break;
        if (marker >= 0xc0 && marker <= 0xcf && ![0xc4, 0xc8, 0xcc].includes(marker)) {
          if (length < 8) break;
          h = d.readUInt16BE(i + 5); w = d.readUInt16BE(i + 7);
          break;
        }
        i += 2 + length;
      }
      break;
    case "image/gif":
      if (d.length < 10 || !["GIF87a", "GIF89a"].includes(d.toString("ascii", 0, 6))) break;
      w = d.readUInt16LE(6); h = d.readUInt16LE(8);
      break;
    case "image/webp":
      if (d.length < 30 || d.toString("ascii", 0, 4) !== "RIFF" || d.toString("ascii", 8, 12) !== "WEBP") break;
      switch (d.toString("ascii", 12, 16)) {
        case "VP8 ": w = d.readUInt16LE(26) & 0x3fff; h = d.readUInt16LE(28) & 0x3fff; break;
        case "VP8L": {
          const bits = d.readUInt32LE(21);
          w = (bits & 0x3fff) + 1; h = ((bits >>> 14) & 0x3fff) + 1;
          break;
        }
        case "VP8X":
          if ((d[20] & 2) !== 0) return false; // animated WebP
          w = d.readUIntLE(24, 3) + 1; h = d.readUIntLE(27, 3) + 1;
          break;
      }
      break;
  }
  return w > 0 && h > 0 && w <= MAX_AXIS && h <= MAX_AXIS && w * h <= MAX_PIXELS;
}

export type FitJob = { images: readonly ImageBytes[]; limits: ImageLimits; wasm?: Uint8Array<ArrayBuffer> };
async function run({ images, limits, wasm }: FitJob) {
  if (!images.every(boundedImage)) return undefined;
  wasm ??= readFileSync(createRequire(import.meta.url).resolve("@silvia-odwyer/photon/photon_rs_bg.wasm"));
  const processor = createPhotonImages(wasm);
  const fitted = [];
  for (const input of images) {
    const result = await fitImage(input.data, input.mimeType, limits, processor);
    if (result.kind !== "fitted") return undefined;
    fitted.push(result.image);
  }
  return { images: fitted };
}

// A short-lived worker bounds Photon memory retention and leaves the shared
// server event loop responsive while an upload is decoded.
if (parentPort) void run(workerData as FitJob).then(value => parentPort!.postMessage(value));
