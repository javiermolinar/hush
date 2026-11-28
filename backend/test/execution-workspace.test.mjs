import assert from "node:assert/strict";
import { test } from "node:test";
import { allowedExecutionPath } from "../../dist/backend/src/execution-workspace.js";

test("restricted reads may enter managed checkouts but not the rest of a protected directory", () => {
  const data = "/state/hush", checkouts = "/state/hush/worktrees", agent = "/home/me/.hush";
  const allowed = (root, path) => allowedExecutionPath(root, path, [agent, data], checkouts);
  assert.equal(allowed("/state/hush/worktrees/hush-x", "src/a.ts"), true);
  assert.equal(allowed("/state/hush/worktrees/hush-x/pkg", "a.ts"), true);
  // A workspace that contains the data or agent directory still can't read into it.
  assert.equal(allowed("/state", "hush/history.sqlite"), false);
  assert.equal(allowed("/home/me", ".hush/auth.json"), false);
  assert.equal(allowed("/home/me/.hush", "auth.json"), false);
  assert.equal(allowed("/state", "hush/worktrees/hush-x/a.ts"), true);
  assert.equal(allowedExecutionPath("/state/hush/worktrees/hush-x", "a.ts", [agent, data]), false, "without the exception");
  assert.equal(allowed("/state/hush/worktrees/hush-x", "../../history.sqlite"), false);
  assert.equal(allowed("/state/hush/worktrees/hush-x", ".git/config"), false);
});
