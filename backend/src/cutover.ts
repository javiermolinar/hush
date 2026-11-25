import { randomUUID } from "node:crypto";
import { constants } from "node:fs";
import { copyFile, link, mkdir, mkdtemp, readdir, realpath, rename, rm, statfs } from "node:fs/promises";
import { join, resolve, sep } from "node:path";
import { validStoreIdentity, type StoreIdentity } from "../../server/src/store-identity.js";
import { object } from "../../server/src/wire.js";
import { BackendError, hushDirectory } from "./config.js";
import { bindingName, checkStoreProfile, historyName, markerName, privateDirectory, privateFile, sidecars,
  storeFormat, storeManifest, storeRegistry, uuid, type StoreManifest } from "./profile.js";
import { readStoreJSON, replaceStoreJSON, sha256File, statStorePath, syncPath } from "./store-files.js";
import { dataLock } from "./store-lock.js";
import { durableSchema, durableVersion, makeSnapshot, predecessorTasks, StoreValidationError, verifySnapshot } from "./store-snapshot.js";
import { checkSocket } from "./store-socket.js";

const baseline = "standalone-rc-2026-11-23";
// Only claim the exact predecessor on a platform for which we can exercise it.
const sourceDigests = new Map([["linux/arm64", "eec512d5bb9755f97f927ffcc2d8da77d59b834e1c5d3d32255ca82a1f17c21f"]]);
const platform = `${process.platform}/${process.arch}`;
const minimumNode = "22.19.0";
const sourceRegistry = "hush-standalone-mvp";
const localLinux = new Set([0xef53, 0x58465342, 0x9123683e, 0x794c7630]); // ext, XFS, Btrfs, overlayfs
const digest = (v: unknown): v is string => typeof v === "string" && /^[a-f0-9]{64}$/.test(v);
const decimal = (v: unknown): v is string => typeof v === "string" && /^[0-9]{1,24}$/.test(v);

function managedPaths(agent: string) {
  const control = join(agent, "store-cutover"), runtime = join(agent, "runtime");
  return { agent, control, runtime, state: join(control, "state.json"), marker: join(runtime, markerName), socket: join(runtime, "hush.sock") };
}
type Paths = ReturnType<typeof managedPaths>;

type State = {
  version: 1;
  transaction: string;
  binding: { device: string; inode: string };
  owner: { device: string; inode: string };
  runtime: { device: string; inode: string };
  sourceArtifact: string;
  targetBundle: string;
  snapshot: string;
  source: StoreIdentity;
  target: StoreIdentity;
  phase: "prepared" | "committed" | "restored" | "rolled-back";
};

const identity = ({ storeId, generation }: StoreIdentity) => ({ storeId, generation });
const fileID = ({ dev, ino }: { dev: bigint; ino: bigint }) => ({ device: String(dev), inode: String(ino) });
const fileIDRecord = (v: unknown) => object(v) && Object.keys(v).length === 2 && decimal(v.device) && decimal(v.inode);
const sameID = (a: State["binding"], b: State["binding"]) => a.device === b.device && a.inode === b.inode;
const fenced = () => new BackendError("store_cutover");
const recovery = "Cutover state is unknown or past the rollback gate. Preserve both stores; inspect and recover forward.";
const fail = (message: string) => new StoreValidationError(message);

async function readState(paths: Paths): Promise<State | undefined> {
  try {
    const raw = await readStoreJSON(paths.state);
    if (raw === undefined) return;
    if (!object(raw)) throw fenced();
    const { version, transaction, sourceArtifact, targetBundle, snapshot, source, target, phase, binding, owner, runtime } = raw;
    if (version !== 1 || !uuid(transaction) || !digest(sourceArtifact) || sourceArtifact !== sourceDigests.get(platform) ||
        !digest(targetBundle) || !digest(snapshot) || !validStoreIdentity(source) || !validStoreIdentity(target) ||
        source.storeId !== target.storeId || source.generation === target.generation ||
        typeof phase !== "string" || !["prepared", "committed", "restored", "rolled-back"].includes(phase) || Object.keys(raw).length !== 11 ||
        !fileIDRecord(binding) || !fileIDRecord(owner) || !fileIDRecord(runtime)) throw fenced();
    return raw as State;
  } catch { throw fenced(); }
}

// Match the physical default path even through an alias such as a symlink to
// HOME. A custom path can never open the prepared/activated store by opting out
// of the interlock; a cutover manifest without this exact gate is refused.
export async function managedRuntime(agentDir: string, dataDir: string) {
  const paths = managedPaths(await realpath(agentDir));
  const requested = resolve(dataDir), lexical = managedPaths(resolve(agentDir));
  if (requested === lexical.runtime) return paths;
  const candidatePath = await realpath(dataDir).catch((err: NodeJS.ErrnoException) => {
    if (err.code !== "ENOENT") throw err;
    return requested;
  });
  for (const control of [paths.control, lexical.control]) {
    if ([requested, candidatePath].some(path => path === control || path.startsWith(control + sep))) throw fenced();
  }
  const runtime = await statStorePath(paths.runtime), candidate = await statStorePath(dataDir);
  return runtime && candidate && runtime.ino === candidate.ino && runtime.dev === candidate.dev ? paths : undefined;
}

export async function managedWriter(paths: Paths) {
  await privateDirectory(paths.control, true);
  return dataLock(paths.control);
}

// Read before creating/opening a runtime directory. Transient kernel lock
// release alone is not permission to boot after an interrupted cutover.
export async function activeCutover(paths: Paths) {
  const state = await readState(paths);
  if (state?.phase === "committed") {
    await verifyOwner(paths, state);
    return state;
  }
  if (state) throw fenced();
  return undefined;
}

const markerMatches = (marker: StoreManifest | undefined, state: State) => marker !== undefined &&
  marker.storeId === state.target.storeId && marker.generation === state.target.generation &&
  sameID(marker, state.binding) &&
  (state.phase === "committed" ? marker.format === storeFormat && marker.cutover === state.transaction : marker.format === 1);

async function syncGate(paths: Paths) {
  await syncPath(paths.state);
  await syncPath(paths.control, true);
  await syncPath(paths.agent, true);
}

export async function allowManagedOpen(paths: Paths | undefined, state: State | undefined,
  manifest: StoreManifest | undefined, entry: string) {
  if (!state && !manifest?.cutover) return;
  if (!paths || state?.phase !== "committed" || !markerMatches(manifest, state) ||
      await sha256File(entry).catch(() => "") !== state.targetBundle) throw fenced();
  await verifyOwner(paths, state);
  // A killed activator might have renamed state.json immediately before its
  // directory fsync. Re-establish durability before this process can act.
  await syncGate(paths).catch(() => { throw fenced(); });
}

const fence = (transaction: string) => ({ profile: "hush", format: 0, cutover: transaction });
function isFence(marker: unknown, transaction: string) {
  return object(marker) && Object.keys(marker).length === 3 && marker.profile === "hush" && marker.format === 0 && marker.cutover === transaction;
}
const backupDir = (paths: Paths, state: State) => join(paths.control, state.transaction);
const archive = (paths: Paths, state: State) => join(backupDir(paths, state), "snapshot.sqlite");

async function ownerIDs(paths: Paths) {
  const runtime = await statStorePath(paths.runtime), owner = await statStorePath(join(paths.runtime, "owner.sqlite"));
  if (!runtime?.isDirectory() || !owner?.isFile() || owner.nlink !== 1n || runtime.dev !== owner.dev) throw fenced();
  return { owner: fileID(owner), runtime: fileID(runtime) };
}

async function verifyOwner(paths: Paths, state: State) {
  const ids = await ownerIDs(paths);
  if (!sameID(state.owner, ids.owner) || !sameID(state.runtime, ids.runtime)) throw fenced();
}

async function retireRestore(paths: Paths, state: State): Promise<State> {
  if (!markerMatches(await checkStoreProfile(paths.runtime, 1), state)) throw fail(recovery);
  const retired = { ...state, phase: "rolled-back" } as const;
  await replaceStoreJSON(paths.state, retired);
  return retired;
}

async function prepare(paths: Paths, from: string, targetBundle: string): Promise<State> {
  const sourceArtifact = sourceDigests.get(platform);
  if (!sourceArtifact || process.versions.node !== minimumNode ||
      await sha256File(from).catch(() => "") !== sourceArtifact) throw fail("Expected the pinned Linux arm64 standalone predecessor and Node 22.19.0.");
  let prior = await readState(paths);
  if (prior?.phase === "restored") {
    // A prior rollback published a verified source binding and may already
    // have done new work. Retire its pending publish before a fresh prepare.
    await verifyOwner(paths, prior);
    prior = await retireRestore(paths, prior);
  }
  if (prior && prior.phase !== "rolled-back") throw fail("A cutover is already recorded for this store; inspect it before proceeding.");
  const original = await checkStoreProfile(paths.runtime, 1);
  if (prior && !markerMatches(original, prior)) throw fail(recovery);
  if (!original || !await privateFile(join(paths.runtime, "owner.sqlite"))) throw fail("No bound format-1 source store.");
  const allowed = new Set([markerName, historyName, bindingName, "owner.sqlite", "owner.sqlite-journal", "hush.sock", "hush.log",
    ...sidecars.map(s => historyName + s)]);
  if ((await readdir(paths.runtime)).some(name => !allowed.has(name))) throw fail("Unknown runtime artifacts or managed worktrees; this source cannot be migrated.");
  const transaction = randomUUID(), directory = join(paths.control, transaction);
  await mkdir(directory, { mode: 0o700 });
  const snapshot = await makeSnapshot(paths.runtime, directory, join(directory, "snapshot.sqlite"));
  await replaceStoreJSON(join(directory, "source-manifest.json"), original);
  await syncPath(paths.control, true);
  await syncPath(paths.agent, true);
  const state = { version: 1, phase: "prepared", transaction, sourceArtifact, targetBundle, snapshot,
    source: identity(original), target: { storeId: original.storeId, generation: randomUUID() },
    binding: { device: original.device, inode: original.inode }, ...await ownerIDs(paths) } as const;

  // Before the legacy refusal marker is durable, the source DB has not
  // changed. After it is durable, neither binary can open the source. A crash
  // between these writes is an unknown cutover, never a rollback authorization.
  await replaceStoreJSON(paths.marker, fence(transaction));
  await replaceStoreJSON(paths.state, state);
  return state;
}

async function verifyPreparedArchive(paths: Paths, state: State) {
  const directory = backupDir(paths, state);
  await privateDirectory(directory, false);
  const saved = await readStoreJSON(join(directory, "source-manifest.json"));
  if (!object(saved) || Object.keys(saved).length !== 6 || saved.profile !== "hush" || saved.format !== 1 ||
      saved.storeId !== state.source.storeId || saved.generation !== state.source.generation ||
      saved.device !== state.binding.device || saved.inode !== state.binding.inode) throw fail("The predecessor manifest does not match the prepared cutover.");
  const snapshot = archive(paths, state);
  await verifySnapshot(snapshot, state.snapshot);
  return snapshot;
}

// Durable's schema is unchanged. Install a fresh, matched copy and binding
// under the legacy refusal marker, leaving its directory and owner inode alone.
async function installSnapshot(paths: Paths, state: State, format: 1 | 2) {
  const snapshot = await verifyPreparedArchive(paths, state);
  const stage = await mkdtemp(join(backupDir(paths, state), "install-"));
  try {
    const history = join(stage, historyName), binding = join(stage, bindingName);
    await copyFile(snapshot, history, constants.COPYFILE_EXCL);
    if (await sha256File(history) !== state.snapshot) throw fail("Staged database did not match the snapshot.");
    await link(history, binding);
    const manifest = await storeManifest(history, state.target, format, format === 2 ? state.transaction : null);
    await syncPath(history);
    // The canonical backup is whole. No WAL from the replaced inode may be
    // interpreted as belonging to the new database.
    for (const suffix of sidecars) {
      const path = join(paths.runtime, historyName + suffix);
      if (await privateFile(path)) await rm(path);
    }
    await rename(history, join(paths.runtime, historyName));
    await rename(binding, join(paths.runtime, bindingName));
    await syncPath(paths.runtime, true);
    await syncPath(stage, true);
    return manifest;
  } finally { await rm(stage, { recursive: true, force: true }); }
}

async function publishManifest(paths: Paths, state: State, format: 1 | 2) {
  const history = join(paths.runtime, historyName);
  // Interrupted after making the decision but before publishing the marker:
  // finish only on the exact installed inode and unchanged SHA. Never recopy
  // a backup on this path, even if the current store cannot be opened.
  const manifest = await storeManifest(history, state.target, format, format === 2 ? state.transaction : null);
  const binding = await statStorePath(join(paths.runtime, bindingName));
  if (!markerMatches(manifest, state) || !binding?.isFile() || String(binding.dev) !== manifest.device ||
      String(binding.ino) !== manifest.inode || await sha256File(history) !== state.snapshot) throw fail(recovery);
  for (const suffix of sidecars) if (await statStorePath(history + suffix)) throw fail(recovery);
  await syncGate(paths);
  await replaceStoreJSON(paths.marker, manifest);
}

async function finish(paths: Paths, action: "activate" | "rollback", targetBundle: string): Promise<State> {
  let state = await readState(paths);
  if (!state || state.targetBundle !== targetBundle ||
      action === "rollback" && state.phase === "committed" || action === "activate" && state.phase === "restored") throw fail(recovery);
  await verifyOwner(paths, state);
  const format = action === "rollback" ? 1 : storeFormat;
  if (state.phase === "rolled-back") {
    if (action === "rollback" && markerMatches(await checkStoreProfile(paths.runtime, 1), state)) return state;
    throw fail(recovery);
  }
  const marker = await readStoreJSON(paths.marker);
  if (!isFence(marker, state.transaction)) {
    if (state.phase === "prepared" || !markerMatches(await checkStoreProfile(paths.runtime, format), state)) throw fail(recovery);
    return state.phase === "restored" ? retireRestore(paths, state) : state;
  }
  if (state.phase === "prepared") {
    if (format === 1) state = { ...state, target: { storeId: state.source.storeId, generation: randomUUID() } };
    const manifest = await installSnapshot(paths, state, format);
    state = { ...state, phase: format === 1 ? "restored" : "committed", binding: { device: manifest.device, inode: manifest.inode } };
    // The point of no return is outside the restored data and precedes any
    // marker that either executable can use. No listeners or Harness involved.
    await replaceStoreJSON(paths.state, state);
  }
  await publishManifest(paths, state, format);
  return state.phase === "restored" ? retireRestore(paths, state) : state;
}

export async function runStoreCommand(argv: string[], entry: string) {
  const [action, flag, from] = argv;
  if (process.env.HUSH_SOCKET || process.env.HUSH_PROFILE && process.env.HUSH_PROFILE !== "hush" ||
      (action === "prepare" ? argv.length !== 3 || flag !== "--from" || !from : argv.length !== 1 ||
        !["inspect", "activate", "rollback"].includes(action ?? ""))) throw fail("Use the local Hush profile: store inspect | prepare --from PINNED-HUSH | activate | rollback.");
  const paths = managedPaths(hushDirectory()), targetBundle = await sha256File(entry);
  const matrix = { platform, source: { build: baseline, artifactSha256: sourceDigests.get(platform) ?? null, format: 1, registry: sourceRegistry },
    target: { bundleSha256: targetBundle, format: storeFormat, registry: storeRegistry }, nodeForPrepare: minimumNode,
    durable: durableVersion, schema: durableSchema, sourceTasks: predecessorTasks };
  const emit = (result: object) => process.stdout.write(JSON.stringify({ matrix, ...result }) + "\n");
  if (action === "inspect") {
    try {
      for (const dir of [paths.agent, paths.runtime, paths.control]) await privateDirectory(dir, false, true);
      const state = await readState(paths), marker = await readStoreJSON(paths.marker);
      const format = object(marker) && typeof marker.format === "number" ? marker.format : null;
      const interrupted = state ? isFence(marker, state.transaction) : format === 0;
      if (state) await verifyOwner(paths, state);
      const matches = state && !interrupted && markerMatches(await checkStoreProfile(paths.runtime,
        state.phase === "committed" ? 2 : 1).catch(() => undefined), state);
      const verified = state && (interrupted ? state.phase !== "rolled-back" : matches && state.phase !== "prepared");
      const orphaned = marker !== undefined && (format !== 1 &&
        (format !== 2 || !object(marker) || marker.cutover !== null));
      const phase = verified ? state.phase : state || orphaned ? "unknown" : "not-prepared";
      const rollbackEligible = !!state && phase === "prepared" &&
        await verifyPreparedArchive(paths, state).then(() => true, () => false);
      emit({ phase, format, rollbackEligible,
        ...(state && { transaction: state.transaction, source: state.source, target: state.target, snapshotSha256: state.snapshot }) });
    } catch { emit({ phase: "unknown", rollbackEligible: false }); }
    return;
  }

  // Refuse non-managed data paths, a nonlocal layout or any active owner. The
  // operator stops the daemon; acquiring both locks proves no supported
  // backend can still be admitting work or joining its scheduler/tool tasks.
  await privateDirectory(paths.agent, false);
  await privateDirectory(paths.runtime, false);
  if (!sourceDigests.has(platform) || !localLinux.has((await statfs(paths.runtime)).type >>> 0)) {
    throw fail("Cutover requires a supported local Linux arm64 filesystem.");
  }
  const unlock = await managedWriter(paths);
  try {
    if (await privateFile(join(paths.agent, "supervisor-v1.json")) &&
        !await privateFile(join(paths.agent, "supervisor-hold-v1"))) {
      throw fail("The login supervisor owns this profile. Run 'hush service pause' before changing the store.");
    }
    const runtime = await statStorePath(paths.runtime), control = await statStorePath(paths.control);
    if (!runtime || !control || runtime.dev !== control.dev) throw fail("Cutover requires runtime and control on the same local filesystem.");
    // Do not create a missing legacy lock in a cutover operation. Its inode
    // is an invariant of the supported runtime, not another stageable file.
    if (!await privateFile(join(paths.runtime, "owner.sqlite"))) throw fail("Missing legacy writer interlock; do not attempt cutover.");
    const release = await dataLock(paths.runtime);
    try {
      await checkSocket(paths.socket, "inspect");
      const state = action === "prepare" ? await prepare(paths, resolve(from!), targetBundle) : await finish(paths, action as "activate" | "rollback", targetBundle);
      emit({ phase: state.phase, transaction: state.transaction, source: state.source, target: state.target, snapshotSha256: state.snapshot });
    } finally { release(); }
  } finally { unlock(); }
}
