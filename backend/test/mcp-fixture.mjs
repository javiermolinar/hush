// Tiny local fixture. No inherited credentials, drivers or network access.
import { createInterface } from "node:readline";
for await (const line of createInterface({ input: process.stdin })) {
  const req = JSON.parse(line);
  if (req.id === undefined) continue;
  const result = req.method === "initialize" ? {
    protocolVersion: req.params.protocolVersion, serverInfo: { name: "fixture", version: "1" }, capabilities: { tools: {} },
  } : req.method === "tools/list" ? { tools: [{
    name: "hello", description: "A local MCP fixture tool.", inputSchema: { type: "object", properties: {} },
  }] } : req.method === "tools/call" ? { content: [{ type: "text", text: "mcp fixture ran" }] } : {};
  process.stdout.write(JSON.stringify({ jsonrpc: "2.0", id: req.id, result }) + "\n");
}
