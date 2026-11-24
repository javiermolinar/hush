// Source proof for capability-owned metadata, not the production titles/models operations.
import assert from "node:assert/strict";
import type { Context } from "@earendil-works/chord/context";
import type { Api, Model, ModelThinkingLevel } from "@earendil-works/pi-ai";
import { clampThinkingLevel } from "@earendil-works/pi-ai/models";
import { AgentDoc, defineDoc, type Conversation, type Harness } from "@earendil-works/pi-durable";
import { TraceJournal } from "./ordered-run.mts";

const Title = defineDoc<{ name?: string }>({
  kind: "hush.fixture-profile-title", version: 1, scope: "conversation", history: "latest", fork: "initial",
  initial: () => ({}), checkpointWhen: () => true,
});

export class OrderedMetadata {
  readonly #harness: Harness;
  readonly #conv: Conversation;
  readonly journal = new TraceJournal();

  static async attach(harness: Harness, conv: Conversation, ctx: Context) {
    // Fixture attaches before commands, while no metadata changes can race the seed.
    return new OrderedMetadata(harness, conv, (await conv.agent(ctx)).thinkingLevel);
  }

  private constructor(harness: Harness, conv: Conversation, level: ModelThinkingLevel) {
    this.#harness = harness;
    this.#conv = conv;
    harness.observeCommits({
      advance: batch => {
        if (this.journal.failure) return;
        try {
          // This fixture admits one metadata command per transaction. No intra-commit order is promised.
          const changes = batch.changes.filter(c => c.type === "document" && c.kind === "change" && c.conversationId === conv.id &&
            (c.record.kind === Title.definition.kind || c.record.kind === AgentDoc.definition.kind));
          assert(changes.length <= 1, "metadata proof has no intra-commit ordering rule");
          for (const c of changes) {
            if (c.type !== "document" || c.kind !== "change") continue;
            if (c.record.kind === Title.definition.kind) {
              const name = c.value.name;
              assert(name === undefined || typeof name === "string");
              this.journal.push({ type: "session_info_changed", name });
            } else {
              const next = (c.value.thinkingLevel ?? "off") as ModelThinkingLevel;
              if (next !== level) this.journal.push({ type: "thinking_level_changed", level: next });
              level = next;
            }
          }
        } catch (error) { this.journal.fail(error instanceof Error ? error.message : String(error)); }
      },
      end: () => this.journal.fail("source ended"),
    });
    this.journal.arm();
  }

  setThinking(model: Model<Api>, requested: ModelThinkingLevel, ctx: Context) {
    return this.#conv.configure({ thinkingLevel: clampThinkingLevel(model, requested) }, ctx);
  }

  setTitle(name: string, ctx: Context) {
    return this.#conv.commit(async tx => {
      (await tx.doc(Title, this.#conv.id)).name = name;
    }, ctx);
  }

  async getTitle(ctx: Context) { return (await this.#harness.snapshot(Title, this.#conv.id, ctx))?.name; }
}
