import { object } from "./wire.js";

export const storeIdentityVersion = 1;
export type StoreIdentity = Readonly<{ storeId: string; generation: string }>;

const uuid = (value: unknown): value is string => typeof value === "string" && value.length === 36 &&
  /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/.test(value);

export function validStoreIdentity(value: unknown): value is StoreIdentity {
  return object(value) && Object.keys(value).length === 2 && Object.hasOwn(value, "storeId") &&
    Object.hasOwn(value, "generation") && uuid(value.storeId) && uuid(value.generation);
}
