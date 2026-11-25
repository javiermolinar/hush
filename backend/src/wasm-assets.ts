import { createHash } from "node:crypto";
import { constants } from "node:fs";
import { open, type FileHandle } from "node:fs/promises";
import { BackendError } from "./config.js";

export type WasmAsset = "photon" | "quickjs";
type Slice = { offset: number; length: number; sha256: string };
declare const HUSH_WASM_INDEX: Record<WasmAsset, Slice> | undefined;

// The launcher unlinks its extraction directory after ready. Keep the inode
// open, but don't read, decode, hash or compile any WASM until a tool needs it.
export class WasmAssets {
  private constructor(private readonly file: FileHandle, private readonly index: Record<WasmAsset, Slice>) {}

  static async open(entry: string): Promise<WasmAssets | undefined> {
    if (typeof HUSH_WASM_INDEX === "undefined") return;
    let file: FileHandle | undefined;
    try {
      file = await open(new URL(entry + ".wasm-pack"), constants.O_RDONLY | constants.O_NOFOLLOW | constants.O_NONBLOCK);
      const st = await file.stat();
      const size = HUSH_WASM_INDEX.quickjs.offset + HUSH_WASM_INDEX.quickjs.length;
      if (!st.isFile() || st.size !== size) throw new BackendError("wasm_pack");
      return new WasmAssets(file, HUSH_WASM_INDEX);
    } catch {
      await file?.close().catch(() => {});
      throw new BackendError("wasm_pack");
    }
  }

  async read(asset: WasmAsset): Promise<Buffer<ArrayBuffer>> {
    const { offset, length, sha256 } = this.index[asset];
    const bytes = Buffer.alloc(length);
    for (let n = 0; n < length;) {
      const { bytesRead } = await this.file.read(bytes, n, length - n, offset + n);
      if (!bytesRead) throw new Error("truncated backend WASM pack");
      n += bytesRead;
    }
    // The hash is in the entry that the store gate and release checks bind.
    if (createHash("sha256").update(bytes).digest("hex") !== sha256) throw new Error("bad backend WASM asset");
    return bytes;
  }

  close() { return this.file.close(); }
}
