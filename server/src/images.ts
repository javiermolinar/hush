import type { ImageContent, TextContent } from "@earendil-works/pi-ai";
import type { AgentState } from "@earendil-works/pi-durable";
import { imageSubmit, imageGet, imageSuccess } from "./validators.js";
import { durableID, WireError } from "./wire.js";

export const imagesVersion = 1;
export const maxImageBytes = 5 * 1024 * 1024; // aggregate decoded upload bytes, below the base record limit
const maxBase64 = Math.ceil(maxImageBytes / 3) * 4;
export type ImageBytes = { mimeType: string; data: Uint8Array };
export type ImageRef = { threadId: string; entryId: string; messageIndex: number; blockIndex: number };
type WireImage = Omit<ImageBytes, "data"> & { data: string };
export type ImageSubmission = {
  threadId: string; requestId: string; text: string; whenBusy: "steer" | "followUp" | "reject"; images: ImageBytes[];
};
type WireSubmission = Omit<ImageSubmission, "images"> & { images: WireImage[] };
export interface ImagesHost {
  fit(model: AgentState["model"], images: readonly ImageBytes[], signal: AbortSignal): Promise<{
    images: ImageContent[]; notes?: TextContent[];
  }>;
}

const submit = imageSubmit as (value: unknown) => value is WireSubmission;
const get = imageGet as (value: unknown) => value is ImageRef;

export function parseImageSubmit(value: unknown): ImageSubmission {
  if (!submit(value) || !durableID(value.threadId)) throw new WireError("invalid_request");
  if (value.images.reduce((n, img) => n + img.data.length, 0) > maxBase64 + 12) throw new WireError("limit");
  let total = 0;
  const images = value.images.map(img => {
    const bytes = Buffer.from(img.data, "base64");
    if (bytes.length === 0 || bytes.toString("base64") !== img.data) throw new WireError("invalid_request");
    total += bytes.length;
    return { data: bytes, mimeType: img.mimeType };
  });
  if (total > maxImageBytes) throw new WireError("limit");
  return { ...value, images };
}

export function parseImageRef(value: unknown): ImageRef {
  if (!get(value) || !durableID(value.threadId) || !durableID(value.entryId)) throw new WireError("invalid_request");
  return value;
}

export function imageReply(result: object) {
  if (!imageSuccess(result)) throw new WireError("internal");
  return result;
}

export function projectImageBytes(image: ImageContent): WireImage {
  // Unknown/tool-specific formats remain an omitted marker rather than emitting an invalid reply.
  if (!["image/png", "image/jpeg", "image/gif", "image/webp"].includes(image.mimeType)) throw new WireError("not_found");
  if (image.data.length > maxBase64) throw new WireError("limit");
  const bytes = Buffer.from(image.data, "base64");
  if (bytes.length === 0 || bytes.toString("base64") !== image.data) throw new WireError("not_found");
  if (bytes.length > maxImageBytes) throw new WireError("limit");
  return { mimeType: image.mimeType, data: image.data };
}
