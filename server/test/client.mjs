import { EventEmitter, once } from "node:events";
import { createConnection } from "node:net";

export function request(id, method, params) { return { type: "request", id, method, params }; }
export async function dial(path, onFrame = () => {}, { retain = true } = {}) {
  const socket = createConnection(path);
  await once(socket, "connect");
  socket.setEncoding("utf8");
  const events = new EventEmitter();
  events.setMaxListeners(256);
  let partial = "";
  let next = 0;
  let closed = false;
  const frames = [];
  socket.on("data", text => {
    partial += text;
    for (;;) {
      const end = partial.indexOf("\n");
      if (end < 0) break;
      const frame = JSON.parse(partial.slice(0, end));
      partial = partial.slice(end + 1);
      if (retain) frames.push(frame);
      onFrame(frame);
      events.emit("frame", frame);
    }
  });
  const ended = once(socket, "close").then(() => { closed = true; events.emit("end"); });
  // ECONNRESET can be the expected outcome of a malformed input.
  ended.catch(() => {});
  function wait(predicate) {
    const found = frames.find(predicate);
    if (found) return Promise.resolve(found);
    if (closed) return Promise.reject(new Error("socket closed"));
    return new Promise((resolve, reject) => {
      const timeout = setTimeout(() => { cleanup(); reject(new Error("timed out awaiting frame")); }, 5000);
      function cleanup() { clearTimeout(timeout); events.off("frame", on); events.off("end", end); }
      function on(frame) { if (predicate(frame)) { cleanup(); resolve(frame); } }
      function end() { cleanup(); reject(new Error("socket closed")); }
      events.on("frame", on);
      events.on("end", end);
    });
  }
  return {
    socket, frames, ended, wait, close: () => socket.destroy(),
    send(method, params, onRequest = () => {}) {
      const req = request(String(++next), method, params);
      onRequest(req);
      socket.write(JSON.stringify(req) + "\n");
      return wait(frame => frame.id === req.id);
    },
  };
}
