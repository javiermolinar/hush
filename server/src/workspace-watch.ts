// Opaque, boot-local observation token shared by the workspace watch extensions.
export const watchRevision = (value: unknown): value is string =>
  typeof value === "string" && value.length === 64 && /^[0-9a-f]{64}$/.test(value);
