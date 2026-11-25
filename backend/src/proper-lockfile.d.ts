declare module "proper-lockfile" {
  const lockfile: {
    lock(path: string, options: { stale: number; update: number; onCompromised: () => void; fs: object }): Promise<() => Promise<void>>;
  };
  export default lockfile;
}
