import { join } from "node:path";
import { object } from "../../server/src/wire.js";
import { BackendError, hushDirectory } from "./config.js";
import { managedRuntime, managedWriter } from "./cutover.js";
import { privateDirectory, privateFile, uuid } from "./profile.js";
import { readStoreJSON, replaceStoreJSON, statStorePath } from "./store-files.js";
import { dataLock } from "./store-lock.js";
import { checkSocket } from "./store-socket.js";

const claim = "supervisor-v1.json";
const hold = "supervisor-hold-v1";

// This is launch ownership between supported launchers, not authorization
// against the owner of the profile. The kernel writer locks remain mandatory.
export async function checkSupervisor(agent: string | undefined, token: string | undefined) {
  if (!agent) {
    if (token) throw new BackendError("configuration");
    return;
  }
  const owner = await readStoreJSON(join(agent, claim)).catch(() => { throw new BackendError("service_owned"); });
  if (!owner && !token) return;
  if (!object(owner) || Object.keys(owner).length !== 1 || !uuid(owner.token) || owner.token !== token || await privateFile(join(agent, hold))) {
    throw new BackendError("service_owned");
  }
}

// Publish under the managed and (if present) predecessor writer interlocks.
// It does not inspect/upgrade the store or open a writable history connection.
export async function claimSupervisor(token: string | undefined) {
  if (!uuid(token)) throw new BackendError("configuration");
  const agent = hushDirectory();
  await privateDirectory(agent, true);
  const paths = (await managedRuntime(agent, join(agent, "runtime")))!;
  const release = await managedWriter(paths);
  try {
    const exists = await statStorePath(paths.runtime);
    let unlockRuntime: (() => void) | undefined;
    try {
      if (exists) {
        await privateDirectory(paths.runtime, false);
        unlockRuntime = await dataLock(paths.runtime);
        await checkSocket(paths.socket, "inspect");
      }
      await checkSupervisor(paths.agent, undefined);
      if (await privateFile(join(agent, hold))) throw new BackendError("service_owned");
      await replaceStoreJSON(join(agent, claim), { token });
    } finally { unlockRuntime?.(); }
  } finally { release(); }
}
