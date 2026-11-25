import { runIfMain } from "./index.js";

export { loadModels, startBackend } from "./index.js";
await runIfMain(import.meta.url);
