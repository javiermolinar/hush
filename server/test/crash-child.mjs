import { setTimeout } from "node:timers/promises";
import { openNodeSqliteStorage } from "@earendil-works/pi-durable/storage/sqlite/node";
import { fauxAssistantMessage, fauxToolCall } from "@earendil-works/pi-ai";
import { createCoreServer } from "../../dist/server/src/index.js";
import { openNative } from "../../protocol/v1/native-probe.mjs";

const [, , db, socket, mode] = process.argv;
const setup = await openNative({ storage: await openNodeSqliteStorage(db), async beforeToolResult() {
  process.stdout.write("tool-entered\n");
  await setTimeout(60_000);
} });
setup.faux.setResponses(mode === "crash" ? [
  fauxAssistantMessage([fauxToolCall("fixture_echo", {}, { id: "crash-call" })], { stopReason: "toolUse" }),
] : [fauxAssistantMessage("recovered answer")]);
const server = createCoreServer(setup.h);
await server.listen(socket);
process.stdout.write("listening\n");
setup.h.resume();
process.on("SIGTERM", async () => { await server.close(); await setup.close(); process.exit(0); });
