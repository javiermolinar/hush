import { createCipheriv, createDecipheriv, createHmac, randomBytes, timingSafeEqual } from "node:crypto";
import { WireError } from "./wire.js";

const maxLength = 512;

/** Bounded, stateless cursors. The MAC scopes them to this boot and binds their kind and payload. */
export class Cursors {
  readonly #key = randomBytes(32);

  encode(kind: string, value: unknown): string {
    const body = Buffer.from(JSON.stringify([kind, value])).toString("base64url");
    const cursor = this.#mac(body) + body;
    if (cursor.length > maxLength) throw new WireError("limit");
    return cursor;
  }

  decode<T>(kind: string, cursor: string): T {
    const body = cursor.slice(43);
    const mac = cursor.slice(0, 43);
    if (!body || !/^[A-Za-z0-9_-]+$/.test(cursor) || cursor.endsWith("\n") ||
        !timingSafeEqual(Buffer.from(mac), Buffer.from(this.#mac(body)))) throw new WireError("invalid_cursor");
    const [savedKind, value] = JSON.parse(Buffer.from(body, "base64url").toString());
    if (savedKind !== kind) throw new WireError("invalid_cursor");
    return value;
  }

  #mac(body: string) { return createHmac("sha256", this.#key).update(body).digest("base64url"); }
}

const nonceSize = 12, tagSize = 16, headerSize = nonceSize + tagSize;

/** Stateless cursors whose payload can contain private scan positions. */
export class SealedCursors {
  readonly #key = randomBytes(32);

  encode(kind: string, value: unknown): string {
    const nonce = randomBytes(nonceSize);
    const cipher = createCipheriv("aes-256-gcm", this.#key, nonce, { authTagLength: tagSize });
    cipher.setAAD(Buffer.from(kind));
    const body = cipher.update(JSON.stringify(value), "utf8");
    const end = cipher.final();
    const cursor = Buffer.concat([nonce, cipher.getAuthTag(), body, end]).toString("base64url");
    if (cursor.length > maxLength) throw new WireError("limit");
    return cursor;
  }

  decode<T>(kind: string, cursor: string): T {
    if (cursor.length > maxLength) throw new WireError("invalid_cursor");
    const bytes = Buffer.from(cursor, "base64url");
    if (bytes.length <= headerSize || bytes.toString("base64url") !== cursor) throw new WireError("invalid_cursor");
    try {
      const decipher = createDecipheriv("aes-256-gcm", this.#key, bytes.subarray(0, nonceSize), { authTagLength: tagSize });
      decipher.setAAD(Buffer.from(kind));
      decipher.setAuthTag(bytes.subarray(nonceSize, headerSize));
      const body = Buffer.concat([decipher.update(bytes.subarray(headerSize)), decipher.final()]);
      return JSON.parse(body.toString("utf8"));
    } catch {
      throw new WireError("invalid_cursor");
    }
  }
}
