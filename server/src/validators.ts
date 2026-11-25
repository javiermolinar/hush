import AjvModule, { type AnySchemaObject, type ValidateFunction } from "ajv/dist/2020.js";
import core from "../../protocol/v1/core.schema.json" with { type: "json" };
import images from "../../protocol/v1/images.schema.json" with { type: "json" };
import intro from "../../protocol/v1/intro.schema.json" with { type: "json" };
import directory from "../../protocol/v1/thread-directory.schema.json" with { type: "json" };

function refs<const N extends string, const S extends { $id: string; $defs: object }>(
    schema: S, names: Record<N, keyof S["$defs"]>) {
  const mapped = {} as Record<N, AnySchemaObject>;
  for (const name of Object.keys(names) as N[]) mapped[name] = { $ref: schema.$id + "#/$defs/" + String(names[name]) };
  return mapped;
}

const { window: stateWindow } = core.$defs.state.properties;
const shapes = {
  ...refs(core, { requestShape: "request", successShape: "success", patchShape: "patch", stateShape: "state", entryShape: "entry" }),
  // Entries are verified when projected. Validate everything else on each feed.
  stateEnvelopeShape: {
    ...core.$defs.state, $defs: core.$defs,
    properties: { ...core.$defs.state.properties,
      window: { ...stateWindow, properties: { ...stateWindow.properties, entries: { ...stateWindow.properties.entries, items: true } } },
    },
  },
  ...refs(images, { imageSubmit: "submit", imageGet: "get", imageSuccess: "successEnvelope" }),
  ...refs(intro, { introGet: "get", introRename: "rename", introRecover: "recover", introSuccess: "successEnvelope", introSuccessV2: "successEnvelopeV2" }),
  ...refs(directory, { dirList: "list", dirListV2: "listV2", dirGet: "get", dirNew: "newThread", dirArchive: "archive",
    dirWorkspace: "listWorkspace", dirWorkspaceSuccess: "successWorkspace", dirSearch: "listWorkspaceSearch",
    dirSearchSuccess: "successWorkspaceSearch", dirSuccess: "successEnvelope", dirSuccessV2: "successEnvelopeV2" }),
};

// The direct server build and the backend's esbuild substitution use the
// same schemas/options. Emit named standalone validators for the backend so
// every process starts without running Ajv's code generator on the main thread.
export function configureValidators(source = false) {
  const ajv = new AjvModule.Ajv2020({ strict: false, ownProperties: true, code: { source, esm: true } });
  for (const schema of [core, images, intro, directory]) ajv.addSchema(schema);
  const exports = {} as Record<keyof typeof shapes, string>;
  for (const name of Object.keys(shapes) as (keyof typeof shapes)[]) {
    ajv.addSchema(shapes[name], name);
    exports[name] = name;
  }
  return { ajv, exports };
}

let config: ReturnType<typeof configureValidators> | undefined;
function validator(name: keyof typeof shapes) {
  let compiled: ValidateFunction | undefined;
  return (value: unknown) => (compiled ??= (config ??= configureValidators()).ajv.getSchema(name)!)(value) === true;
}

export const requestShape = validator("requestShape"), successShape = validator("successShape"), patchShape = validator("patchShape"),
  stateShape = validator("stateShape"), entryShape = validator("entryShape"), stateEnvelopeShape = validator("stateEnvelopeShape"),
  imageSubmit = validator("imageSubmit"), imageGet = validator("imageGet"), imageSuccess = validator("imageSuccess"),
  introGet = validator("introGet"), introRename = validator("introRename"), introRecover = validator("introRecover"),
  introSuccess = validator("introSuccess"), introSuccessV2 = validator("introSuccessV2"),
  dirList = validator("dirList"), dirListV2 = validator("dirListV2"), dirGet = validator("dirGet"), dirNew = validator("dirNew"),
  dirArchive = validator("dirArchive"), dirWorkspace = validator("dirWorkspace"), dirWorkspaceSuccess = validator("dirWorkspaceSuccess"),
  dirSearch = validator("dirSearch"), dirSearchSuccess = validator("dirSearchSuccess"), dirSuccess = validator("dirSuccess"),
  dirSuccessV2 = validator("dirSuccessV2");
