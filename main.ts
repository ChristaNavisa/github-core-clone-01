// main.ts — thin launcher for the Go forge.
//
// The graded server is a statically linked Go binary (`forge`, committed at
// the repo root; RULES.txt allows a vendored static binary). Node only
// starts it: the Go process owns the whole lifecycle itself (bind an
// ephemeral port, POST {port} to $MAKEFUN_ENV_URL/register, poll /done,
// exit 0). Node is never in the request path, so it adds no latency.
//
// Two robustness details:
//   - Signals (SIGTERM/SIGINT) are forwarded to the child before we exit,
//     so the forge never survives us as an orphan holding the port.
//   - The binary is chmod'ed executable defensively (a no-op when the bit
//     is already set; harmless if /bot is read-only and it fails). If the
//     spawn still fails with EACCES (exec bit lost in transit), we copy the
//     binary to a temp dir and run that copy instead.
import { spawn } from "node:child_process";
import { chmodSync, copyFileSync, existsSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

const forgePath = fileURLToPath(new URL("./forge", import.meta.url));

try {
  chmodSync(forgePath, 0o755);
} catch {
  // /bot may be read-only in the pod; the bit is normally already set.
}

function startForge(path) {
  const child = spawn(path, [], { stdio: "inherit", env: process.env });
  for (const signal of ["SIGTERM", "SIGINT"]) {
    process.on(signal, () => {
      try {
        child.kill(signal);
      } catch {}
    });
  }
  return child;
}

function fallbackPath() {
  const fallback = join(tmpdir(), `forge-${process.pid}`);
  copyFileSync(forgePath, fallback);
  chmodSync(fallback, 0o755);
  return fallback;
}

function watch(child) {
  child.on("error", (err) => {
    if (err.code === "EACCES" && existsSync(forgePath)) {
      try {
        watch(startForge(fallbackPath()));
      } catch (e2) {
        console.error("failed to start forge:", e2);
        process.exit(1);
      }
      return;
    }
    console.error("failed to start forge:", err);
    process.exit(1);
  });
  child.on("exit", (code, signal) => {
    if (signal) {
      process.kill(process.pid, signal);
      return;
    }
    process.exit(code ?? 1);
  });
}

let child;
try {
  child = startForge(forgePath);
} catch (err) {
  if (err && err.code === "EACCES" && existsSync(forgePath)) {
    child = startForge(fallbackPath());
  } else {
    throw err;
  }
}
watch(child);
