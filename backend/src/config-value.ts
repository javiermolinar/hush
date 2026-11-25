import { BackendError } from "./config.js";

// Handle Hush profile literals and environment interpolation, never shell
// commands. A missing variable is not sent to a provider as literal text.
export function configValue(text: string, env?: Record<string, string>): string | undefined {
  if (text.startsWith("!")) throw new BackendError("configuration");
  let missing = false;
  const value = text.replace(/\$\$|\$!|\$\{([A-Za-z_]\w*)\}|\$([A-Za-z_]\w*)/g,
    (match, braced: string | undefined, plain: string | undefined) => {
      const name = braced ?? plain;
      if (!name) return match.slice(1);
      const resolved = (env && Object.hasOwn(env, name) ? env[name] : undefined) ||
        (Object.hasOwn(process.env, name) ? process.env[name] : undefined);
      if (!resolved) missing = true;
      return resolved ?? "";
    });
  return missing ? undefined : value;
}
