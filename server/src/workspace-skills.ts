import { isAbsolute } from "node:path";
import { durableID, object, WireError } from "./wire.js";

export const workspaceSkillsVersion = 1;
export const workspaceSkillDraftVersion = 2;
export const skillName = (name: unknown): name is string => typeof name === "string" &&
  /^[a-z0-9][a-z0-9-]{0,62}$/.test(name) && !name.endsWith("\n");
export const skillToken = (token: unknown): token is string => typeof token === "string" &&
  /^[A-Za-z0-9_-]{44,512}$/.test(token) && !token.endsWith("\n");
export const skillCommand = (text: string) => /^\/skill:([a-z0-9][a-z0-9-]{0,62})(?:[ \t\n]+([\s\S]*))?$/.exec(text);

export type WorkspaceSkill = { name: string; description: string; source: "workspace" | "personal"; token: string };
export type WorkspaceSkillScope = { threadId: string } | { cwd: string };
export type WorkspaceSkillsVersion = typeof workspaceSkillsVersion | typeof workspaceSkillDraftVersion;
export interface WorkspaceSkillsHost {
  list(scope: WorkspaceSkillScope, signal: AbortSignal): Promise<{ skills: WorkspaceSkill[] }>;
}

export function parseWorkspaceSkillList(fields: Record<string, unknown>, version: WorkspaceSkillsVersion): WorkspaceSkillScope {
  if (Object.keys(fields).length === 1) {
    if (durableID(fields.threadId)) return { threadId: fields.threadId };
    const { cwd } = fields;
    if (version === workspaceSkillDraftVersion && typeof cwd === "string" && isAbsolute(cwd) && cwd.isWellFormed() &&
        Buffer.byteLength(cwd) <= 4096 && !/[\x00-\x1f\x7f-\x9f]/.test(cwd)) return { cwd };
  }
  throw new WireError("invalid_request");
}

export function parseWorkspaceSkillSubmit(fields: Record<string, unknown>) {
  if (!object(fields.input) || !skillToken(fields.token) || typeof fields.input.text !== "string" ||
      !skillCommand(fields.input.text) || Object.keys(fields).some(key => !["input", "token", "environment"].includes(key))) {
    throw new WireError("invalid_request");
  }
  return { input: fields.input, token: fields.token, environment: fields.environment };
}
