import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { test } from "node:test";
import { formatNumber, parseNumber, quote } from "./go.js";
import {
  hostObject,
  loadScript,
  NativeObject,
  ScriptError,
  ScriptObject,
  type Call,
  type HostModule,
  type Resolver,
  type Value,
} from "./script.js";

// The fixtures are the Go compiler's: the shared cases (server/internal/engine/language/testdata/scripts) compiled to
// WebAssembly, and how Go formats and parses numbers (scripts/language-fixtures.mjs writes them before the tests).
interface Fixtures {
  cases: { name: string; module: string; want?: string; logs?: string; eval?: string }[];
  modules: Record<string, string>;
  hosts: Record<string, string>;
  formats: { bits: string; text: string }[];
  parses: { text: string; bits: string | null }[];
  quotes: Record<string, string>;
}

const dir = new URL("./fixtures/", import.meta.url);
const fixtures: Fixtures = JSON.parse(readFileSync(new URL("fixtures.json", dir), "utf8"));
const moduleBytes = (file: string) => readFileSync(new URL(file, dir));

function float(bits: string): number {
  const view = new DataView(new ArrayBuffer(8));
  view.setBigUint64(0, BigInt("0x" + bits));
  return view.getFloat64(0);
}

/** What a case's natives get, as script_test.go's caseData: what it logs, and the promises to settle in order */
interface CaseData {
  logs: string[];
  pending: (() => void)[];
}

/** A native's promise that the harness settles with settle once main returned */
function later(call: Call, settle: (r: Resolver) => void): Value {
  const [p, r] = call.newPromise();
  (call.data as CaseData).pending.push(() => settle(r));
  return p;
}

/** The cases' "test" host module, as script_test.go has it */
const testHost: HostModule = {
  name: "test",
  source: fixtures.hosts.test,
  natives: {
    log: (call, _self, [message]) => {
      const data = call.data as CaseData | string[] | undefined;
      if (Array.isArray(data)) data.push(message as string);
      else data?.logs.push(message as string);
    },
    later: (call, _self, [v]) => later(call, (r) => r.resolve(v)),
    laterText: (call, _self, [s]) => later(call, (r) => r.resolve(s)),
    failLater: (call, _self, [code]) => later(call, (r) => r.reject(hostObject(testHost, "ProbeError", code))),
    "ProbeError.code": (_call, self) => (self as NativeObject).payload as string,
    probe: (_call, _self, [name]) => hostObject(testHost, "Probe", name),
    "Probe.name": (_call, self) => (self as NativeObject).payload as string,
    "Probe.twice": (_call, _self, [n]) => {
      if ((n as number) < 0) throw new Error("no negative numbers");
      return (n as number) * 2;
    },
  },
};

/** A value written as the cases' want sections write it (show in script_test.go) */
function show(v: Value): string {
  if (typeof v === "string") return quote(v);
  if (typeof v === "number") return formatNumber(v);
  if (typeof v === "boolean") return String(v);
  if (v === null) return "null";
  if (Array.isArray(v)) return "[" + v.map(show).join(", ") + "]";
  if (v instanceof Map) return "{" + [...v].map(([k, x]) => show(k) + ": " + show(x)).join(", ") + "}";
  if (v instanceof ScriptObject || v instanceof NativeObject) return v.className;
  return "?";
}

for (const c of fixtures.cases) {
  test(`case ${c.name}`, async () => {
    const script = await loadScript(moduleBytes(c.module), { hosts: [testHost] });
    const data: CaseData = { logs: [], pending: [] };
    const logs = data.logs;
    let got: Value = null;
    let err: unknown = null;
    try {
      got = script.call("main", data);
      // then the promises its natives made, in order, as they are made
      while (data.pending.length > 0) data.pending.shift()!();
    } catch (e) {
      err = e;
    }
    if (c.eval !== undefined) {
      assert.ok(err instanceof ScriptError, `main() = ${show(got)}, want an evaluation error`);
      assert.ok(err.message.includes(c.eval), `${err.message} doesn't say ${c.eval}`);
    } else {
      assert.equal(err, null);
    }
    if (c.want !== undefined) assert.equal(show(got), c.want);
    if (c.logs !== undefined) assert.equal(logs.join("\n"), c.logs);
  });
}

test("numbers are written as Go writes them", () => {
  for (const f of fixtures.formats) assert.equal(formatNumber(float(f.bits)), f.text, `bits ${f.bits}`);
});

test("numbers are read as Go reads them", () => {
  for (const p of fixtures.parses) {
    const v = parseNumber(p.text);
    const ok = v !== null && Number.isFinite(v);
    if (p.bits === null) {
      assert.ok(!ok, `${JSON.stringify(p.text)} read as ${v}, but Go refuses it`);
    } else {
      assert.ok(ok, `${JSON.stringify(p.text)} wasn't read`);
      assert.ok(Object.is(v, float(p.bits)), `${JSON.stringify(p.text)} = ${v}, want ${float(p.bits)}`);
    }
  }
});

test("strings are quoted as Go quotes them", () => {
  for (const [s, q] of Object.entries(fixtures.quotes)) assert.equal(quote(s), q);
});

test("an instance takes props, keeps its fields, and calls its host", async () => {
  const logs: string[] = [];
  const host: HostModule = {
    name: "host",
    source: fixtures.hosts.host,
    natives: { log: (call, _self, [line]) => void (call.data as string[]).push(line as string) },
  };
  const script = await loadScript(moduleBytes(fixtures.modules.door), { hosts: [host] });
  const cls = script.class();
  assert.equal(cls?.name, "Door");
  const door = script.new(cls!, logs);
  assert.equal(door.fieldKind("speed"), "number");
  assert.equal(door.fieldKind("opened"), null);
  assert.throws(() => door.set("speed", "fast"));
  door.set("speed", 2);
  assert.ok(door.hasMethod("onUpdate") && !door.hasMethod("onStart"));
  door.call("onUpdate", 0.5);
  door.call("onUpdate", 0.5);
  assert.deepEqual(logs, ["hi 1", "hi 2"]);
  assert.equal(door.get("greeting"), "hi");
  assert.throws(() => door.call("onUpdate", "x"));
});

test("a script compiled against another host module is refused", async () => {
  const changed: HostModule = { ...testHost, source: fixtures.hosts.test + "\nexport interface Extra { x: number }" };
  const logging = fixtures.cases.find((c) => c.logs !== undefined)!;
  await assert.rejects(loadScript(moduleBytes(logging.module), { hosts: [changed] }), /another version/);
  await assert.rejects(loadScript(moduleBytes(fixtures.modules.door), { hosts: [] }), /doesn't offer/);
});

test("the collector frees garbage and keeps what the instance and the host hold", async () => {
  const script = await loadScript(moduleBytes(fixtures.modules.churn));
  const churn = script.new();
  for (let i = 0; i < 200; i++) churn.call("churn", i);
  assert.equal(churn.call("label"), "kept 199");
  const item = churn.call("make", "apple");
  churn.keep([item]);
  for (let i = 0; i < 50; i++) churn.call("churn", i);
  assert.equal(churn.callOn(item, "describe"), "apple:new,APPLE");
  const other = churn.call("make", "pear");
  churn.call("label");
  assert.throws(() => churn.callOn(other, "describe"), /isn't an object of this script/);
  // more than the heap budget kept is refused
  assert.throws(() => churn.call("hoard", 3000), /memory/);
});

test("natives take and give objects, arrays and maps, and dropped host objects are released", async () => {
  interface Tag {
    name: string;
  }
  const objects: HostModule = {
    name: "objects",
    source: fixtures.hosts.objects,
    natives: {
      tag: (_call, _self, [name]) => hostObject(objects, "Tag", { name: name as string }),
      "Tag.name": (_call, self) => ((self as NativeObject).payload as Tag).name,
      "Tag.shout": (_call, self) => ((self as NativeObject).payload as Tag).name.toUpperCase() + "!",
      sum: (_call, _self, [xs]) => ((xs as number[] | null) ?? []).reduce((a, b) => a + b, 0),
      counts: (_call, _self, [words]) => {
        const m = new Map<string, Value>();
        for (const w of words as string[]) m.set(w, ((m.get(w) as number | undefined) ?? 0) + 1);
        return m;
      },
      same: (_call, _self, [a, b]) => a === b,
    },
  };
  const script = await loadScript(moduleBytes(fixtures.modules.objects), { hosts: [objects] });
  const user = script.new();
  assert.equal(user.call("run"), "bell X! 6.5 212");
  const bell = hostObject(objects, "Tag", { name: "bell" });
  assert.equal(user.call("hold", bell), "bell");
  assert.equal(user.call("again", bell), true);
  assert.equal(user.call("total", [1, 2.5], new Map<string, Value>([["a", false], ["b", true]])), "3.5 a+b true");
  for (let i = 0; i < 300; i++) user.call("see", hostObject(objects, "Tag", { name: `t${i}` }));
  assert.equal(user.get("last"), "t299");
  assert.ok(user.nativeCount < 100, `the instance still holds ${user.nativeCount} host objects`);
});

test("compiled scripts run fast in the browser's engine", async () => {
  const script = await loadScript(moduleBytes(fixtures.modules.churn));
  const churn = script.new();
  const start = performance.now();
  for (let i = 0; i < 100; i++) churn.call("churn", i);
  // 100 calls of 100 objects and 50 strings each; the Go interpreter compiled to WebAssembly took seconds
  assert.ok(performance.now() - start < 2000);
});
