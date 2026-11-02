// Writes what the browser runtime is tested against into js/src/fixtures/ (not committed): the shared script cases
// compiled to WebAssembly by the Go compiler, and how Go formats and parses numbers (TestWriteBrowserFixtures in
// language/). The tests run it first, so they need Go (https://go.dev/dl).
import { execFileSync } from "node:child_process";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const root = join(dirname(fileURLToPath(import.meta.url)), "..", "..");

try {
  execFileSync("go", ["test", "./language", "-run", "^TestWriteBrowserFixtures$", "-count=1"], {
    cwd: root,
    env: { ...process.env, KILN_FIXTURES: join(root, "js", "src", "fixtures") },
    stdio: ["ignore", "ignore", "inherit"],
  });
} catch (err) {
  console.error(
    err.code === "ENOENT"
      ? "fixtures: the runtime's tests need Go (https://go.dev/dl) to compile their scripts"
      : "fixtures: writing the fixtures failed"
  );
  process.exit(1);
}
