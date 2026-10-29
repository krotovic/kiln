/**
 * The browser's host of compiled scripts: a script's WebAssembly module, as the asset service compiles it
 * (server/internal/engine/language), runs here as it does on the servers, through the same interface
 * (server/internal/engine/language/abi.go): the same imports, exports, memory layout, errors and metadata.
 */
import { formatMessage, formatNumber, parseNumber, quote } from "./go.js";

/** A value crossing between the host and a script */
export type Value =
  | number
  | string
  | boolean
  | null
  | Value[]
  | Map<number | string, Value>
  | ScriptObject
  | NativeObject
  | ScriptPromise;

/** A native function, method ("C.m") or field ("C.f") of a host module; self is the object for a method or field */
export type NativeFunc = (call: Call, self: Value, args: Value[]) => Value | void;

/** What a native knows of the call it serves */
export interface Call {
  /** What the host gave the instance (Script.new) or Script.call this call came from */
  readonly data: unknown;
  /**
   * Makes a pending promise for the native's result, which must be a Promise: the native returns the promise and
   * keeps the resolver to settle it later. It throws when the instance has MAX_PENDING_PROMISES already.
   */
  newPromise(): [ScriptPromise, Resolver];
}

/** The most promises an instance's natives made that the host hasn't settled yet */
export const MAX_PENDING_PROMISES = 64;

/**
 * A module the host offers scripts, by its name (such as "ui"), with its natives. Its definition file (source),
 * when given, must be the one the script was compiled against.
 */
export interface HostModule {
  readonly name: string;
  readonly source?: string;
  readonly natives: Readonly<Record<string, NativeFunc>>;
}

/** An object of a host module's native class, with the host's payload */
export class NativeObject {
  constructor(
    readonly host: HostModule,
    readonly className: string,
    readonly payload: unknown,
  ) {}
}

/** Makes an object of a host module's native class, for passing to scripts */
export function hostObject(host: HostModule, className: string, payload: unknown): NativeObject {
  return new NativeObject(host, className, payload);
}

/** An object of the script, by its handle in its instance */
export class ScriptObject {
  constructor(
    readonly instance: Instance,
    public handle: number,
    readonly classIndex: number,
  ) {}

  get className(): string {
    return this.instance.script.meta.classes[this.classIndex].name;
  }
}

/** A promise a native made, as the script gets it */
export class ScriptPromise {
  constructor(readonly resolver: Resolver) {}
}

/**
 * Settles a promise a native made (Call.newPromise). resolve and reject run, under fresh budgets, what waited for
 * it, so they are calls into the instance: they must come between its other calls, never from a native. A promise
 * settles once; settling it again does nothing.
 */
export class Resolver {
  /** @internal */
  constructor(
    /** @internal */ readonly instance: Instance,
    /** @internal */ public handle: number,
    /** @internal */ readonly value: TypeMeta,
    /** @internal */ readonly err: TypeMeta | undefined,
  ) {}

  /** Whether the promise hasn't settled */
  get pending(): boolean {
    return this.handle !== 0;
  }

  /** Fulfils the promise with v, a value of its type (anything for a Promise<void>); throws the script's error */
  resolve(v: Value): void {
    this.instance.settle(this, v, false);
  }

  /** Fails the promise with e, an object of its error class; throws the script's error */
  reject(e: NativeObject): void {
    this.instance.settle(this, e, true);
  }
}

/** An error while a script runs, naming where; or a compiled script that can't be loaded */
export class ScriptError extends Error {
  constructor(
    readonly module: string,
    readonly line: number,
    readonly col: number,
    readonly msg: string,
  ) {
    super(`${module}:${line}:${col}: ${msg}`);
  }
}

// ---- the interface, as abi.go has it ----

const ABI_VERSION = 2;
const META_SECTION = "alo.script";
const SCRATCH = 16;
const HEADER = 16;
const TAG_ARR_VALS = 2;
const TAG_ARR_REFS = 3;
const TAG_CLASS = 16;
const MAP_STR_KEYS = 1;
const MAP_REF_VALS = 2;

interface TypeMeta {
  k: "n" | "s" | "b" | "v" | "a" | "m" | "c" | "i" | "p";
  e?: TypeMeta;
  key?: TypeMeta;
  c?: number;
  /** A promise's error class */
  err?: TypeMeta;
  name: string;
}

// The kinds of promises the runtime makes (gen_async.go)
const PROMISE_VALUE = 0;
const PROMISE_STRING = 1;
const PROMISE_REF = 2;

interface FieldMeta {
  name: string;
  type: TypeMeta;
  canon: string;
  offset: number;
  private?: boolean;
  readonly?: boolean;
}

interface MethodMeta {
  params: TypeMeta[];
  result: TypeMeta;
  private?: boolean;
  sig: string;
  canon: string;
  export?: string;
}

interface ClassMeta {
  name: string;
  module: string;
  pos: number;
  native?: boolean;
  size: number;
  fields: FieldMeta[];
  methods: Record<string, MethodMeta>;
  ctor: TypeMeta[];
}

interface NativeMeta {
  host: string;
  name: string;
  this?: boolean;
  params: TypeMeta[];
  result: TypeMeta;
}

export interface ScriptMeta {
  abi: number;
  entry: string;
  modules: string[];
  limits: { MaxHeap: number };
  messages: string[];
  positions: number[];
  classes: ClassMeta[];
  natives: NativeMeta[];
  hosts: { name: string; hash: string }[];
  funcs: Record<string, { params: TypeMeta[]; result: TypeMeta; export: string }>;
  exports: Record<string, number>;
}

/** The first 8 bytes of the SHA-256 of a host module's definition file, in hex, as the compiler records it */
async function hostHash(source: string): Promise<string> {
  const sum = new Uint8Array(await crypto.subtle.digest("SHA-256", new TextEncoder().encode(source)));
  return Array.from(sum.slice(0, 8), (b) => b.toString(16).padStart(2, "0")).join("");
}

export interface LoadOptions {
  hosts?: HostModule[];
}

/**
 * Loads a compiled script: the module (its bytes, or a fetch Response that streams them), against the host
 * modules it imports. It refuses a module made for another version of the runtime, one that imports a host
 * module that isn't given, or (when the host module gives its source) one compiled against another version of it.
 */
export async function loadScript(module: BufferSource | Response, opts: LoadOptions = {}): Promise<Script> {
  const compiled =
    module instanceof Response ? await WebAssembly.compileStreaming(module) : await WebAssembly.compile(module);
  const sections = WebAssembly.Module.customSections(compiled, META_SECTION);
  if (sections.length === 0) throw new Error(`not a compiled script: it has no ${META_SECTION} section`);
  const meta = JSON.parse(new TextDecoder().decode(sections[0])) as ScriptMeta;
  if (meta.abi !== ABI_VERSION) {
    throw new Error(`the script was compiled for runtime version ${meta.abi}, not ${ABI_VERSION}: compile it again`);
  }
  const hosts = new Map((opts.hosts ?? []).map((h) => [h.name, h]));
  for (const hm of meta.hosts) {
    const h = hosts.get(hm.name);
    if (!h) throw new Error(`the script imports the host module "${hm.name}", which this host doesn't offer`);
    if (h.source !== undefined && (await hostHash(h.source)) !== hm.hash) {
      throw new Error(`the script was compiled against another version of the host module "${hm.name}": compile it again`);
    }
  }
  const natives: NativeFunc[] = [];
  for (const nm of meta.natives) {
    const fn = hosts.get(nm.host)?.natives[nm.name];
    if (!fn) throw new Error(`the host gives no code for the native ${nm.name} of "${nm.host}"`);
    natives.push(fn);
  }
  return new Script(compiled, meta, natives);
}

/** A class a script exports */
export interface ScriptClass {
  readonly name: string;
  readonly index: number;
  /** How many arguments its constructor takes */
  readonly constructorParams: number;
}

/** A compiled script, whose module each Instance instantiates */
export class Script {
  /** @internal */
  constructor(
    readonly module: WebAssembly.Module,
    readonly meta: ScriptMeta,
    /** @internal */ readonly natives: NativeFunc[],
  ) {}

  /** The script's modules: the entry and everything it imports, host modules aside */
  get modules(): string[] {
    return [...this.meta.modules];
  }

  /** A class the entry exports ("default" for its default export) */
  class(name = "default"): ScriptClass | null {
    const index = this.meta.exports[name];
    if (index === undefined) return null;
    const cm = this.meta.classes[index];
    return { name: cm.name, index, constructorParams: cm.ctor.length };
  }

  /**
   * Makes an object of a class the entry exports with its constructor, which must take no arguments, on an
   * instance of the module of its own; data is what natives get (Call.data) when it calls them.
   */
  new(cls: ScriptClass | string = "default", data?: unknown): Instance {
    const c = typeof cls === "string" ? this.class(cls) : cls;
    if (!c) throw new Error(`${this.meta.entry} exports no class ${String(cls)}`);
    if (c.constructorParams !== 0) throw new Error(`${c.name}'s constructor takes arguments`);
    const inst = new Instance(this, data, c.index);
    inst.initConsts();
    inst.construct();
    return inst;
  }

  /** Calls a function the entry exports, on an instance of the module of its own */
  call(name: string, data: unknown, ...args: Value[]): Value {
    const fm = this.meta.funcs[name];
    if (!fm) throw new Error(`${this.meta.entry} exports no function ${name}`);
    const params = fm.params;
    if (args.length !== params.length) throw new Error(`${name} takes ${params.length} arguments`);
    const inst = new Instance(this, data, -1);
    inst.initConsts();
    return inst.invoke(fm.export, -1, args, params, fm.result);
  }

  /** @internal */
  errorAt(pos: number, msg: string): ScriptError {
    const i = pos * 3;
    if (pos < 0 || i + 2 >= this.meta.positions.length) return new ScriptError(this.meta.entry, 1, 1, msg);
    return new ScriptError(this.meta.modules[this.meta.positions[i]] ?? "", this.meta.positions[i + 1], this.meta.positions[i + 2], msg);
  }
}

type Exports = Record<string, (...args: number[]) => number> & { memory: WebAssembly.Memory };

const encoder = new TextEncoder();
const decoder = new TextDecoder();

function fits(v: Value, t: TypeMeta): boolean {
  switch (t.k) {
    case "n":
      return typeof v === "number";
    case "s":
      return typeof v === "string";
    case "b":
      return typeof v === "boolean";
    case "a":
      return v === null || Array.isArray(v);
    case "m":
      return v === null || v instanceof Map;
    case "c":
    case "i":
      return v === null || v instanceof ScriptObject || v instanceof NativeObject;
    case "p":
      return v instanceof ScriptPromise;
  }
  return true;
}

const isRef = (t: TypeMeta) => t.k !== "n" && t.k !== "b";

function kindOf(v: Value): string {
  if (v === null) return "null";
  if (Array.isArray(v)) return "array";
  if (v instanceof Map) return "map";
  if (typeof v === "object") return "object";
  return typeof v === "boolean" ? "bool" : typeof v;
}

/**
 * An object of a script's class that the host keeps and calls into, with a module instance of its own. Objects
 * of the script it hands out (results, natives' arguments) stay valid until the host's next call into it,
 * unless the host keeps them (keep).
 */
export class Instance {
  private readonly ex: Exports;
  private view: DataView;
  private err: ScriptError | null = null;
  private self = 0;
  private temp: number[] = [];
  private kept: number[] = [];
  private live = new Set<number>();
  private natObjs = new Map<number, NativeObject>();
  private natIdx = new Map<NativeObject, number>();
  private natFree: number[] = [];
  private natNext = 1;
  private classIndices = new Map<string, number>();
  /** The promises natives made that the host hasn't settled yet */
  private pending = 0;

  /** @internal */
  constructor(
    readonly script: Script,
    readonly data: unknown,
    private readonly cls: number,
  ) {
    const imports = {
      alo: {
        fail: (code: number, pos: number) => {
          this.err = this.failure(code, pos);
        },
        native: (id: number, pos: number) => {
          try {
            this.native(id, pos);
            return 0;
          } catch (e) {
            this.err = e instanceof ScriptError ? e : this.script.errorAt(pos, String((e as Error)?.message ?? e));
            return 1;
          }
        },
        fmt_num: (x: number) => this.newString(formatNumber(x)),
        parse_num: (ptr: number, len: number) => {
          const v = parseNumber(this.readBytes(ptr, len));
          if (v === null || !Number.isFinite(v)) return 0;
          this.mem().setFloat64(SCRATCH, v, true);
          return 1;
        },
        fmod: (a: number, b: number) => a % b,
        release: (nh: number) => this.release(nh),
      },
    };
    const instance = new WebAssembly.Instance(script.module, imports);
    this.ex = instance.exports as unknown as Exports;
    this.view = new DataView(this.ex.memory.buffer);
    script.meta.classes.forEach((c, i) => {
      if (c.native) this.classIndices.set(c.module + ":" + c.name, i);
    });
  }

  private mem(): DataView {
    if (this.view.buffer !== this.ex.memory.buffer) this.view = new DataView(this.ex.memory.buffer);
    return this.view;
  }

  private u32(addr: number): number {
    return this.mem().getUint32(addr, true);
  }

  private readBytes(addr: number, n: number): string {
    return decoder.decode(new Uint8Array(this.ex.memory.buffer, addr >>> 0, n >>> 0));
  }

  private readString(p: number): string {
    return this.readBytes(p + HEADER, this.u32(p + 12));
  }

  private newString(s: string): number {
    const bytes = encoder.encode(s);
    const p = this.ex.__str_new(bytes.length) >>> 0;
    new Uint8Array(this.ex.memory.buffer, p + HEADER, bytes.length).set(bytes);
    return p;
  }

  /** Runs an export, turning a trap into the error the script reported */
  private run<T>(fn: () => T): T {
    try {
      return fn();
    } catch (e) {
      if (this.err) throw this.err;
      if (e instanceof ScriptError) throw e;
      throw this.script.errorAt(0, `the script stopped: ${String((e as Error)?.message ?? e)}`);
    }
  }

  private begin(): void {
    this.err = null;
    this.ex.__begin();
  }

  /** Ends a call: it collects when it is time, and fails when the instance keeps more than MaxHeap */
  private end(): void {
    const live = this.run(() => this.ex.__end());
    const limit = this.script.meta.limits.MaxHeap;
    if (live >= 0 && live > limit && this.cls >= 0) {
      const cm = this.script.meta.classes[this.cls];
      throw this.script.errorAt(cm.pos, `${cm.name} keeps more than ${limit} bytes of memory`);
    }
  }

  /** @internal */
  initConsts(): void {
    this.begin();
    this.run(() => this.ex.__init());
    this.end();
  }

  /** @internal */
  construct(): void {
    this.begin();
    this.run(() => {
      const obj = this.ex[`n${this.cls}`]() >>> 0;
      this.self = this.ex.__handle_new(obj);
      this.live.add(this.self);
      this.ex[`c${this.cls}`](obj);
    });
    this.end();
  }

  private dropTemps(): void {
    for (const h of this.temp) {
      this.live.delete(h);
      this.ex.__handle_drop(h);
    }
    this.temp = [];
  }

  /** @internal */
  invoke(exportName: string, self: number, args: Value[], params: TypeMeta[], result: TypeMeta): Value {
    this.begin();
    const raw = this.run(() => args.map((a, i) => this.toWasm(a, params[i])));
    if (self >= 0) raw.unshift(this.ex.__handle_get(self) >>> 0);
    this.dropTemps();
    const fn = this.ex[exportName];
    if (!fn) throw new Error(`the compiled script has no ${exportName}`);
    const out = this.run(() => fn(...raw));
    const value = result.k === "v" ? null : this.run(() => this.fromWasm(out, result));
    this.end();
    return value;
  }

  /** Whether the instance's class has a public method name */
  hasMethod(name: string): boolean {
    const m = this.script.meta.classes[this.cls].methods[name];
    return !!m && !m.private;
  }

  /** Calls the instance's public method name with args, which must have its parameter types */
  call(method: string, ...args: Value[]): Value {
    return this.callOn(this.selfObject(), method, ...args);
  }

  /** Whether obj, an object of this instance's script, has a public method name */
  hasMethodOn(obj: Value, name: string): boolean {
    const ref = this.own(obj);
    const m = ref && this.script.meta.classes[ref.classIndex].methods[name];
    return !!m && !m.private;
  }

  /** Calls the public method of obj, an object of this instance's script it handed out */
  callOn(obj: Value, method: string, ...args: Value[]): Value {
    const ref = this.own(obj);
    if (!ref) throw new Error(`${kindOf(obj)} isn't an object of this script`);
    const cm = this.script.meta.classes[ref.classIndex];
    const m = cm.methods[method];
    if (!m || m.private || !m.export) throw new Error(`${cm.name} has no method ${method}`);
    if (args.length !== m.params.length) throw new Error(`${cm.name}.${method} takes ${m.params.length} arguments`);
    args.forEach((a, i) => {
      if (!fits(a, m.params[i])) throw new Error(`${cm.name}.${method} wants a ${m.params[i].name}, not a ${kindOf(a)}`);
    });
    return this.invoke(m.export, ref.handle, args, m.params, m.result);
  }

  /** The instance's object */
  selfObject(): ScriptObject {
    return new ScriptObject(this, this.self, this.cls);
  }

  /**
   * Which more objects of the script the host holds on to: they stay valid past the next call, and their memory
   * counts as the instance's own. It replaces what keep was given before.
   */
  keep(objs: Value[]): void {
    const kept: number[] = [];
    const refs: ScriptObject[] = [];
    for (const o of objs) {
      const ref = this.own(o);
      if (!ref || ref.handle === this.self) continue;
      kept.push(this.ex.__handle_new(this.ex.__handle_get(ref.handle)));
      refs.push(ref);
    }
    for (const h of this.kept) {
      this.live.delete(h);
      this.ex.__handle_drop(h);
    }
    this.kept = kept;
    kept.forEach((h, i) => {
      this.live.add(h);
      refs[i].handle = h;
    });
  }

  private own(obj: Value): ScriptObject | null {
    return obj instanceof ScriptObject && obj.instance === this && this.live.has(obj.handle) ? obj : null;
  }

  private field(name: string): FieldMeta | undefined {
    return this.script.meta.classes[this.cls].fields.find((f) => f.name === name);
  }

  /** The kind of the instance's public number, string or bool field name, so a host can set it from text */
  fieldKind(name: string): "number" | "string" | "bool" | null {
    const f = this.field(name);
    if (!f || f.private) return null;
    return ({ n: "number", s: "string", b: "bool" } as Record<string, "number" | "string" | "bool">)[f.type.k] ?? null;
  }

  /** Sets the instance's public number, string or bool field name (readonly ones included) */
  set(name: string, v: number | string | boolean): void {
    const f = this.field(name);
    const cm = this.script.meta.classes[this.cls];
    if (!f || this.fieldKind(name) === null) throw new Error(`${cm.name} has no field ${name} of a number, string or bool`);
    if (!fits(v, f.type)) throw new Error(`${cm.name}.${name} is a ${f.type.name}, not a ${kindOf(v)}`);
    this.begin();
    const raw = this.run(() => this.toWasm(v, f.type));
    const addr = (this.ex.__handle_get(this.self) >>> 0) + f.offset;
    this.writeSlot(addr, raw, f.type);
    this.end();
  }

  /** Reads the instance's field name */
  get(name: string): Value | undefined {
    const f = this.field(name);
    if (!f) return undefined;
    const addr = (this.ex.__handle_get(this.self) >>> 0) + f.offset;
    return this.fromWasm(this.readSlot(addr, f.type), f.type);
  }

  // ---- values across the boundary ----

  private writeSlot(addr: number, raw: number, t: TypeMeta): void {
    if (t.k === "n") this.mem().setFloat64(addr, raw, true);
    else this.mem().setUint32(addr, raw >>> 0, true);
  }

  private readSlot(addr: number, t: TypeMeta): number {
    return t.k === "n" ? this.mem().getFloat64(addr, true) : this.u32(addr);
  }

  private toWasm(v: Value, t: TypeMeta): number {
    if (!fits(v, t)) throw new Error(`a ${kindOf(v)} is no ${t.name}`);
    switch (t.k) {
      case "n":
        return v as number;
      case "b":
        return v ? 1 : 0;
      case "s":
        return this.newString(v as string);
    }
    if (v === null) return 0;
    if (t.k === "a") {
      const items = v as Value[];
      const raw = items.map((item) => this.toWasm(item, t.e!));
      const p = this.ex.__arr_new(isRef(t.e!) ? TAG_ARR_REFS : TAG_ARR_VALS, items.length) >>> 0;
      const st = this.u32(p + 16);
      raw.forEach((r, i) => this.writeSlot(st + HEADER + 8 * i, r, t.e!));
      return p;
    }
    if (t.k === "m") {
      const flags = (t.key!.k === "s" ? MAP_STR_KEYS : 0) | (isRef(t.e!) ? MAP_REF_VALS : 0);
      const p = this.ex.__map_new(flags) >>> 0;
      for (const [k, val] of v as Map<number | string, Value>) {
        const kr = this.toWasm(k, t.key!);
        const vr = this.toWasm(val, t.e!);
        this.writeSlot(SCRATCH, kr, t.key!);
        this.writeSlot(SCRATCH + 8, vr, t.e!);
        this.ex.__map_put(p);
      }
      return p;
    }
    if (v instanceof ScriptPromise) {
      const r = v.resolver;
      if (r.instance !== this || r.handle === 0) throw new Error("a promise of another instance, or one settled already");
      return this.ex.__handle_get(r.handle) >>> 0;
    }
    if (v instanceof ScriptObject) {
      if (v.instance !== this) throw new Error("an object of another instance of the script");
      if (!this.live.has(v.handle)) throw new Error("an object of the script from an earlier call, which the host didn't keep");
      return this.ex.__handle_get(v.handle) >>> 0;
    }
    if (v instanceof NativeObject) {
      const cls = this.classIndices.get(v.host.name + ":" + v.className);
      if (cls === undefined) throw new Error(`the script doesn't know the host's class ${v.className}`);
      const known = this.natIdx.get(v);
      if (known !== undefined) return this.ex.__native_get(known) >>> 0;
      const nh = this.natFree.pop() ?? this.natNext++;
      this.natIdx.set(v, nh);
      this.natObjs.set(nh, v);
      return this.ex.__native_new(cls, nh) >>> 0;
    }
    throw new Error(`a ${kindOf(v)} is no ${t.name}`);
  }

  private fromWasm(raw: number, t: TypeMeta): Value {
    switch (t.k) {
      case "n":
        return raw;
      case "b":
        return raw !== 0;
      case "s":
        return this.readString(raw >>> 0);
      case "v":
      case "p":
        // the host doesn't hold the script's promises: an async method's promise is ignored
        return null;
    }
    const p = raw >>> 0;
    if (p === 0) return null;
    if (t.k === "a") {
      const n = this.u32(p + 12);
      const st = this.u32(p + 16);
      const out: Value[] = [];
      for (let i = 0; i < n; i++) out.push(this.fromWasm(this.readSlot(st + HEADER + 8 * i, t.e!), t.e!));
      return out;
    }
    if (t.k === "m") {
      const n = this.u32(p + 12);
      const ks = this.u32(p + 16);
      const vs = this.u32(p + 20);
      const out = new Map<number | string, Value>();
      for (let i = 0; i < n; i++) {
        const k = this.fromWasm(this.readSlot(ks + HEADER + 8 * i, t.key!), t.key!) as number | string;
        out.set(k, this.fromWasm(this.readSlot(vs + HEADER + 8 * i, t.e!), t.e!));
      }
      return out;
    }
    const cls = (this.u32(p) & 0x7fffffff) - TAG_CLASS;
    const cm = this.script.meta.classes[cls];
    if (!cm) throw new Error("the script gave something that isn't an object");
    if (cm.native) {
      const obj = this.natObjs.get(this.u32(p + 16));
      if (!obj) throw new Error("a host object the instance no longer knows");
      return obj;
    }
    const h = this.ex.__handle_new(p);
    this.temp.push(h);
    this.live.add(h);
    return new ScriptObject(this, h, cls);
  }

  private release(nh: number): void {
    const obj = this.natObjs.get(nh);
    if (obj) {
      this.natIdx.delete(obj);
      this.natObjs.delete(nh);
      this.natFree.push(nh);
    }
  }

  // ---- promises natives make ----

  private newPromise(result: TypeMeta): [ScriptPromise, Resolver] {
    if (result.k !== "p") throw new Error("only a native that returns a Promise makes one");
    if (this.pending >= MAX_PENDING_PROMISES) {
      throw new Error(`more than ${MAX_PENDING_PROMISES} promises are waiting for the host`);
    }
    const e = result.e!;
    const kind = e.k === "s" ? PROMISE_STRING : isRef(e) && e.k !== "v" ? PROMISE_REF : PROMISE_VALUE;
    const p = this.ex.__promise_new(kind) >>> 0;
    const h = this.ex.__handle_new(p);
    this.pending++;
    const r = new Resolver(this, h, e, result.err);
    return [new ScriptPromise(r), r];
  }

  /** @internal Settles r's promise with v (or, rejecting, the error e), as one call from the host */
  settle(r: Resolver, v: Value, reject: boolean): void {
    if (r.handle === 0) return;
    let exportName = "__promise_resolve_i";
    let type = r.value;
    if (reject) {
      if (!r.err) throw new Error("the promise can't fail");
      const cls = v instanceof NativeObject ? this.classIndices.get(v.host.name + ":" + v.className) : undefined;
      if (cls !== r.err.c) throw new Error(`a ${kindOf(v)} is no ${r.err.name}`);
      exportName = "__promise_reject";
      type = r.err;
    } else if (r.value.k !== "v") {
      if (!fits(v, r.value)) throw new Error(`a ${kindOf(v)} is no ${r.value.name}`);
      if (r.value.k === "n") exportName = "__promise_resolve_f";
    }
    this.begin();
    // A value that can't go into the script gives the promise up: it never settles, and no longer counts as pending
    let raw = 0;
    let failure: unknown = null;
    try {
      raw = !reject && r.value.k === "v" ? 0 : this.run(() => this.toWasm(v, type));
    } catch (err) {
      failure = err;
    }
    const p = this.ex.__handle_get(r.handle) >>> 0;
    this.ex.__handle_drop(r.handle);
    r.handle = 0;
    this.pending--;
    if (failure) throw failure;
    this.dropTemps();
    this.run(() => this.ex[exportName](p, raw));
    this.end();
  }

  /** How many host objects the instance holds for the script (tests) */
  get nativeCount(): number {
    return this.natObjs.size;
  }

  // ---- the imports ----

  private failure(code: number, pos: number): ScriptError {
    const msg = this.script.meta.messages[code];
    if (msg === undefined) return this.script.errorAt(pos, "the script failed");
    return this.script.errorAt(
      pos,
      formatMessage(msg, (i, kind) => {
        const addr = SCRATCH + 8 * i;
        switch (kind) {
          case "i":
            return String(this.mem().getInt32(addr, true));
          case "n":
            return formatNumber(this.mem().getFloat64(addr, true));
          case "s":
            return this.readString(this.u32(addr));
          default:
            return quote(this.readString(this.u32(addr)));
        }
      }),
    );
  }

  private native(id: number, pos: number): void {
    const s = this.script;
    const nm = s.meta.natives[id];
    if (!nm) throw s.errorAt(pos, `no native ${id}`);
    const member = nm.name.slice(nm.name.lastIndexOf(".") + 1);
    const params = nm.params;
    const first = nm.this ? 1 : 0;
    const raw = params.map((p, i) => this.readSlot(SCRATCH + 8 * (first + i), p));
    let self: Value = null;
    if (nm.this) {
      const obj = this.natObjs.get(this.u32(this.u32(SCRATCH) + 16));
      if (!obj) throw s.errorAt(pos, `${member}: a host object the instance no longer knows`);
      self = obj;
    }
    const args = params.map((p, i) => this.fromWasm(raw[i], p));
    let out: Value | void;
    const call: Call = { data: this.data, newPromise: () => this.newPromise(nm.result) };
    try {
      out = s.natives[id](call, self, args);
    } catch (e) {
      if (e instanceof ScriptError) throw e;
      throw s.errorAt(pos, `${member}: ${String((e as Error)?.message ?? e)}`);
    }
    if (nm.result.k === "v") return;
    if (out === undefined || !fits(out, nm.result)) {
      throw s.errorAt(pos, `${member} gave a ${out === undefined ? "nothing" : kindOf(out)}, not a ${nm.result.name}`);
    }
    this.writeSlot(SCRATCH, this.toWasm(out, nm.result), nm.result);
  }
}
