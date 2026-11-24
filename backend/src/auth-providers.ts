import providers from "../../app/internal/authproviders/providers.json" with { type: "json" };

// Shared with the Go TUI and keyring helper. These flows must work with the
// bundled Pi version, without models.json overlays or extension providers.
export const defaultAuthProvider = "openai-codex";
export type AuthProvider = (typeof providers)[number];
export const authProviders: readonly AuthProvider[] = providers;
export const authProvider = (id: string) => authProviders.find(p => p.id === id);

export async function registerBundledOAuthFlows() {
  // A literal import lets esbuild include Pi's flow modules. Delay loading
  // them until after auth removes the inherited callback bind override: some
  // providers capture it at module initialization.
  const { registerBunOAuthFlows } = await import("@earendil-works/pi-ai/bun-oauth");
  registerBunOAuthFlows();
}
