import { createHmac, randomBytes, timingSafeEqual } from "node:crypto";
import { WireError } from "./wire.js";

/** Bounded, stateless cursors. The MAC scopes them to this boot and binds their kind and payload. */
export class Cursors {
  readonly #key = randomBytes(32);

  encode(kind: string, value: unknown): string {
    const body = Buffer.from(JSON.stringify([kind, value])).toString("base64url");
    const cursor = this.#mac(body) + body;
    if (cursor.length > 512) throw new WireError("limit");
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
