// The language's runtime in the browser: compiled scripts (WebAssembly modules from the asset service) run here
// as on the servers. Import it by path, @alo/game/engine/language/index.js; it doesn't load three.js.
export {
  loadScript,
  hostObject,
  Instance,
  NativeObject,
  Resolver,
  Script,
  ScriptPromise,
  MAX_PENDING_PROMISES,
  ScriptError,
  ScriptObject,
  type Call,
  type HostModule,
  type LoadOptions,
  type NativeFunc,
  type ScriptClass,
  type ScriptMeta,
  type Value,
} from "./script.js";
export { formatNumber, parseNumber, quote } from "./go.js";
