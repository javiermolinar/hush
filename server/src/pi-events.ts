import { randomUUID } from "node:crypto";
import AjvModule from "ajv/dist/2020.js";
import schema from "../../protocol/v1/pi-1.6/pi-events.schema.json" with { type: "json" };
import { cleanJSON, durableID, encode, MAX_RECORD, object, WireError } from "./wire.js";

export const piEventsVersion = 1;
export type PiService = "models" | "titles" | "resources" | "piSessions" | "images";
export type PiOpen = {
  cwd: string; threadId?: string; session?: string; continue?: true; fork?: true;
  provider?: string; model?: string; thinking?: "off" | "minimal" | "low" | "medium" | "high" | "xhigh" | "max";
  models?: string[]; name?: string; systemPrompt?: string; appendSystemPrompt?: string;
};
export type PiHeader = { type: "session"; version: 3; id: string; timestamp: string; cwd: string; parentSession?: string };
export type PiCommand = Readonly<{ type: string; id?: string } & Record<string, unknown>>;
export type PiResponse = { type: "response"; command: string; id?: string } & (
  { success: true; data?: unknown } | { success: false; error: string });
export interface PiEventSink {
  emit(event: object): void;
  fail(): void;
}
export interface PiBinding {
  readonly threadId: string;
  readonly header: PiHeader;
  readonly services: readonly PiService[];
  command(command: PiCommand, successor: () => PiEventSink): Promise<PiCommandAction>;
  close(): void;
}
export type PiCommandAction = { response: PiResponse; next?: PiBinding } | { wait: Promise<PiResponse> };
export interface PiEventsHost {
  open(options: PiOpen, sink: PiEventSink, signal: AbortSignal): Promise<PiBinding>;
  catalog(signal: AbortSignal): Promise<object>;
}

const ajv = new AjvModule.Ajv2020({ strict: false, ownProperties: true }).addSchema(schema);
const def = (name: keyof typeof schema.$defs) => ajv.compile({ $ref: schema.$id + "#/$defs/" + name });
const validOpen = def("open"), validCatalog = def("catalog"), validCall = def("call"),
  validReady = def("ready"), validResult = def("callResult"), validCatalogResult = def("catalogResult"), validEvent = def("event");
const MAX_EVENTS = 256;
const MAX_WAITS = 16;
type EndReason = "source_failed" | "overflow";
type Output = { write(line: string): boolean; backpressured(): boolean; end(): void; destroy(): void };

// Match Pi's serializer: optional undefined object fields are omitted. Reject the
// normalized record if it violates the native wire constraints or the additive schema.
function withoutUndefined(value: unknown, depth = 0): unknown {
  if (depth > 256) throw new WireError("internal");
  if (Array.isArray(value)) return value.map(v => withoutUndefined(v, depth + 1));
  if (object(value)) return Object.fromEntries(Object.entries(value).filter(([, v]) => v !== undefined)
    .map(([k, v]) => [k, withoutUndefined(v, depth + 1)]));
  return value;
}

function piValue(value: object): Record<string, unknown> {
  const v = withoutUndefined(value);
  if (!object(v) || !cleanJSON(v)) throw new WireError("internal");
  return v;
}

class PiFeed implements PiEventSink {
  readonly epoch = randomUUID();
  #binding?: PiBinding;
  #records: string[] = [];
  #bytes = 0;
  #seq = 0;
  #armed = false;
  #closed = false;
  #reason?: EndReason;
  #scheduled?: NodeJS.Immediate;

  constructor(private readonly out: Output, private readonly limit: number) {}

  ready(binding: PiBinding) {
    this.#binding = binding;
    const v = { threadId: binding.threadId, epoch: this.epoch, seq: 0, header: binding.header, services: binding.services };
    if (this.#reason || !durableID(binding.threadId) || !validReady(v)) throw new WireError("internal");
    return v;
  }

  get alive() { return !this.#closed && !this.#reason; }

  emit(event: object) {
    if (!this.alive) return;
    if (!this.#binding) return this.fail();
    if (this.#records.length >= this.limit || this.#seq >= Number.MAX_SAFE_INTEGER) return this.#lose("overflow");
    try {
      // Serialize on intake so nested provider objects cannot mutate after this boundary.
      // Do not write to the socket from a Harness observer or provider tap.
      const value = piValue(event);
      if (!validEvent(value)) return this.fail();
      const frame = encode({ type: "pi_event", threadId: this.#binding.threadId, epoch: this.epoch, seq: this.#seq + 1, event: value });
      const bytes = Buffer.byteLength(frame);
      if (this.#bytes + bytes > MAX_RECORD + 1) return this.#lose("overflow");
      this.#bytes += bytes;
      this.#seq++;
      this.#records.push(frame);
      this.schedule();
    } catch (err) { this.#lose(err instanceof WireError && err.code === "limit" ? "overflow" : "source_failed"); }
  }

  fail() { this.#lose("source_failed"); }
  #lose(reason: EndReason) {
    if (!this.alive) return;
    this.#reason = reason;
    this.#records = [];
    this.#bytes = 0;
    this.schedule();
  }

  arm() { this.#armed = true; this.schedule(); }
  schedule() {
    if (!this.#armed || this.#closed || this.#scheduled || !(this.#reason || this.#records.length && !this.out.backpressured())) return;
    this.#scheduled = setImmediate(() => { this.#scheduled = undefined; this.flush(); });
  }

  // Prior event records cannot appear after the reply that switches streams (or
  // overtake an already-produced command event such as a title change).
  flush(beforeReply = false): boolean {
    if (!this.#armed || this.#closed) return false;
    if (this.#reason) {
      this.out.write(encode({ type: "pi_end", threadId: this.#binding!.threadId, epoch: this.epoch, reason: this.#reason }));
      this.close();
      this.out.end();
      return false;
    }
    while (this.#records.length && (beforeReply || !this.out.backpressured())) {
      const record = this.#records.shift()!;
      this.#bytes -= Buffer.byteLength(record);
      if (!this.out.write(record)) { this.close(); return false; }
    }
    return true;
  }

  close() {
    this.#closed = true;
    if (this.#scheduled) clearImmediate(this.#scheduled);
    this.#records = [];
    this.#bytes = 0;
    this.#binding?.close();
    this.#binding = undefined;
  }
}

/** Peer-local handshake, exclusive binding and bounded live output. No history cursor or replay. */
export class PiPeer {
  #binding?: PiBinding;
  #feed?: PiFeed;
  #waiting = 0;

  constructor(private readonly host: PiEventsHost, private readonly out: Output,
    private readonly signal: AbortSignal, private readonly limit = MAX_EVENTS) {}

  drain() { this.#feed?.schedule(); }
  close() { this.#feed?.close(); }

  async request(operation: string, params: Record<string, unknown>, reply: (result: object) => boolean) {
    if (operation === "catalog") {
      if (!validCatalog(params)) throw new WireError("invalid_request");
      const result = piValue(await this.host.catalog(this.signal));
      if (!validCatalogResult(result)) throw new WireError("internal");
      reply(result);
      return;
    }
    if (operation === "open") {
      if (this.#feed || !validOpen(params) || (params.threadId !== undefined && !durableID(params.threadId))) {
        throw new WireError("invalid_request");
      }
      const feed = new PiFeed(this.out, this.limit);
      const binding = await this.host.open(params as PiOpen, feed, this.signal);
      try {
        if (!reply(feed.ready(binding))) { feed.close(); return; }
      } catch (error) { feed.close(); throw error; }
      this.#binding = binding;
      this.#feed = feed;
      feed.arm();
      return;
    }
    if (operation !== "command") throw new WireError("unknown_method");
    const active = this.#feed;
    if (!active || !validCall(params) || params.epoch !== active.epoch) throw new WireError("invalid_request");
    if (this.#waiting >= MAX_WAITS) throw new WireError("limit");
    if (!active.alive) { active.flush(); return; }
    const command = params.command as PiCommand;
    let feed: PiFeed | undefined;
    const finish = (response: PiResponse, binding?: PiBinding) => {
      try {
        const ready = binding && feed?.ready(binding);
        if (Boolean(binding) !== Boolean(ready) || ready && !response.success || response.id !== command.id || response.command !== command.type) {
          throw new WireError("internal");
        }
        const result = piValue({ response, ready });
        if (!validResult(result)) throw new WireError("internal");
        if (!active.flush(true) || !reply(result)) { binding?.close(); return; }
        if (binding && feed) {
          active.close();
          this.#binding = binding;
          this.#feed = feed;
          feed.arm();
        }
      } catch {
        // The profile command might have run. A failed serialization is loss,
        // not an opportunity to issue a correlated Pi response a second time.
        binding?.close();
        this.out.destroy();
      }
    };
    if (this.#waiting && (command.type === "new_session" || command.type === "fork" || command.type === "clone")) {
      finish({ type: "response", command: command.type, id: command.id, success: false, error: "A session operation is still pending" });
      return;
    }
    let answer: PiCommandAction;
    try {
      answer = await this.#binding!.command(command, () => {
        if (feed) throw new WireError("internal");
        return feed = new PiFeed(this.out, this.limit);
      });
    } catch {
      active.fail();
      active.flush();
      return;
    }
    if ("wait" in answer) {
      this.#waiting++;
      void answer.wait.then(response => { this.#waiting--; finish(response); }, () => {
        this.#waiting--;
        active.fail();
        active.flush();
      });
    } else finish(answer.response, answer.next);
  }
}
