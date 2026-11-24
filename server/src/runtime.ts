export const runtimeVersion = 1;

export interface RuntimeHost {
  /** Begin an orderly shutdown after the requesting socket has flushed its reply. */
  stop(): void;
}
