// A disposable subprocess for the recovery proof. The test kills only this fixture.
import { setTimeout } from "node:timers/promises";
import { fauxAssistantMessage, fauxToolCall } from "@earendil-works/pi-ai";
import { openNodeSqliteStorage } from "@earendil-works/pi-durable/storage/sqlite/node";
import { context, openNative } from "./native-probe.mjs";

const storage = await openNodeSqliteStorage(process.argv[2]);
const setup = await openNative({ storage, async beforeToolResult() {
  // execute() is entered only after Durable records the replay-unsafe intent.
  process.stdout.write("tool-entered\n");
  await setTimeout(120_000);
  throw new Error("recovery fixture was not killed");
} });
try {
  setup.faux.setResponses([fauxAssistantMessage([fauxToolCall("fixture_echo", {}, { id: "crash-call" })], { stopReason: "toolUse" })]);
  const receipt = await setup.conv.submit({ type: "input", requestId: "crash-prompt", content: "before crash" }, context);
  await receipt.wait(context);
} finally { await setup.close(); }
