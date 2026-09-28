/**
 * Proves scripts/dune-import-bans.mjs fails closed.
 *
 * The clean tree must pass. A commented import must not count. Each banned
 * edge must fail and name itself. A missing ledger, a dropped policy edge, an
 * unparsable file, an unresolved import, a stamp without a live edge, and a
 * stamp missing its stamp field must all fail. A stamp clears only the exact
 * edge it names.
 */

import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { describe, test } from "node:test";

const root = join(import.meta.dirname, "..");
const script = join(root, "scripts/dune-import-bans.mjs");
const policyPath = join(root, "scripts/dune-import-bans.policy.json");
const exceptionsPath = join(root, "scripts/dune-import-bans.exceptions.json");
function withFile(path, contents, fn) {
  writeFileSync(path, contents);
  try {
    return fn();
  } finally {
    rmSync(path, { force: true });
  }
}

function run(env = {}) {
  return spawnSync(process.execPath, [script], {
    cwd: root,
    env: { ...process.env, ...env },
    encoding: "utf8",
  });
}

function writeJson(path, value) {
  writeFileSync(path, `${JSON.stringify(value, null, 2)}\n`);
  return path;
}

describe("dune import bans", { concurrency: 1 }, () => {
test("clean tree has no banned Dune edges", () => {
  const result = run();
  assert.equal(result.status, 0, result.stderr || result.stdout);
  assert.match(result.stdout, /dune-import-bans: ok/);
});

test("a commented electron import is not an edge", () => {
  const probe = join(root, "apps/desktop/src/renderer/src/zz-dune-comment-probe.ts");
  withFile(probe, '// import { app } from "electron";\nexport const fine = 1;\n', () => {
    const result = run();
    assert.equal(result.status, 0, result.stderr || result.stdout);
  });
});

test("renderer importing electron is a shortcut IPC skip", () => {
  const probe = join(root, "apps/desktop/src/renderer/src/zz-dune-electron-probe.ts");
  withFile(probe, 'import { ipcRenderer } from "electron";\nexport const channel = ipcRenderer;\n', () => {
    const result = run();
    assert.notEqual(result.status, 0);
    assert.match(result.stderr, /shortcut-ipc/);
    assert.match(result.stderr, /zz-dune-electron-probe\.ts -> electron/);
  });
});

test("renderer importing the daemon token reader is a secret-reader edge", () => {
  const probe = join(root, "apps/desktop/src/renderer/src/zz-dune-secret-probe.ts");
  withFile(
    probe,
    'import { readProfileConfig } from "../../main/daemon-manager";\nexport const leaked = readProfileConfig;\n',
    () => {
      const result = run();
      assert.notEqual(result.status, 0);
      assert.match(result.stderr, /secret-reader-in-ui/);
      assert.match(result.stderr, /unprivileged-to-privileged/);
      assert.match(result.stderr, /shortcut-ipc/);
      assert.match(result.stderr, /daemon-manager/);
    },
  );
});

test("a UI fetch of /api/issues is a parallel board client", () => {
  const probe = join(root, "apps/web/zz-dune-board-probe.ts");
  withFile(
    probe,
    'export async function create() {\n  await fetch("/api/issues", { method: "POST", body: "{}" });\n}\n',
    () => {
      const result = run();
      assert.notEqual(result.status, 0);
      assert.match(result.stderr, /parallel-board-client/);
      assert.match(result.stderr, /zz-dune-board-probe\.ts -> \/api\/issues/);
    },
  );
});

test("mentioning /api/issues without an HTTP call is not a board client", () => {
  const probe = join(root, "packages/views/zz-dune-mention-probe.ts");
  withFile(probe, 'export const note = "The API endpoint /api/issues/123 was updated";\n', () => {
    const result = run();
    assert.equal(result.status, 0, result.stderr || result.stdout);
  });
});

test("missing exception ledger fails closed", () => {
  const result = run({ DUNE_IMPORT_EXCEPTIONS: join(root, "scripts/dune-import-bans.exceptions.missing.json") });
  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /exception ledger missing/);
});

test("a policy that drops an edge fails closed", () => {
  const dir = mkdtempSync(join(tmpdir(), "dune-policy-"));
  const path = join(dir, "policy.json");
  const policy = JSON.parse(readFileSync(policyPath, "utf8"));
  policy.edges = policy.edges.filter((edge) => edge !== "parallel-board-client");
  writeJson(path, policy);
  const result = run({ DUNE_IMPORT_POLICY: path });
  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /policy\.edges/);
  rmSync(dir, { recursive: true, force: true });
});

test("an unused stamp fails closed", () => {
  const dir = mkdtempSync(join(tmpdir(), "dune-exceptions-"));
  const path = join(dir, "exceptions.json");
  writeJson(path, {
    schema: "multica.dune-import-exception.v1",
    exceptions: [
      {
        id: "unused-stamp",
        edge: "shortcut-ipc",
        from: "apps/desktop/src/renderer/src/does-not-exist.ts",
        specifier: "electron",
        reason: "This stamp matches no live edge and must not linger.",
        stamp: "01a0e5b2-e7d3-7dda-9c88-0c348a62797a",
      },
    ],
  });
  const result = run({ DUNE_IMPORT_EXCEPTIONS: path });
  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /unused stamped exception unused-stamp/);
  rmSync(dir, { recursive: true, force: true });
});

test("an exception without a stamp fails closed", () => {
  const dir = mkdtempSync(join(tmpdir(), "dune-unstamped-"));
  const path = join(dir, "exceptions.json");
  writeJson(path, {
    schema: "multica.dune-import-exception.v1",
    exceptions: [
      {
        id: "unstamped",
        edge: "shortcut-ipc",
        from: "apps/desktop/src/renderer/src/zz-dune-electron-probe.ts",
        specifier: "electron",
        reason: "Tried to allow a shortcut without a stamp.",
        stamp: "todo",
      },
    ],
  });
  const result = run({ DUNE_IMPORT_EXCEPTIONS: path });
  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /stamp/);
  rmSync(dir, { recursive: true, force: true });
});

test("a stamp clears only the exact edge", () => {
  const probe = join(root, "apps/desktop/src/renderer/src/zz-dune-stamped-probe.ts");
  writeFileSync(probe, 'import { ipcRenderer } from "electron";\nexport const channel = ipcRenderer;\n');
  try {
  const dir = mkdtempSync(join(tmpdir(), "dune-stamp-"));
  const path = join(dir, "exceptions.json");
  writeJson(path, {
    schema: "multica.dune-import-exception.v1",
    exceptions: [
      {
        id: "stamped-electron-probe",
        edge: "shortcut-ipc",
        from: "apps/desktop/src/renderer/src/zz-dune-stamped-probe.ts",
        specifier: "electron",
        reason: "Test-only stamp for the exact renderer electron import.",
        stamp: "01a0e5b2-e7d3-7dda-9c88-0c348a62797a",
      },
    ],
  });
  const allowed = run({ DUNE_IMPORT_EXCEPTIONS: path });
  assert.equal(allowed.status, 0, allowed.stderr || allowed.stdout);

  writeJson(path, {
    schema: "multica.dune-import-exception.v1",
    exceptions: [
      {
        id: "wrong-specifier",
        edge: "shortcut-ipc",
        from: "apps/desktop/src/renderer/src/zz-dune-stamped-probe.ts",
        specifier: "electron/main",
        reason: "Stamp names a different specifier than the live import.",
        stamp: "01a0e5b2-e7d3-7dda-9c88-0c348a62797a",
      },
    ],
  });
  const mismatch = run({ DUNE_IMPORT_EXCEPTIONS: path });
  assert.notEqual(mismatch.status, 0);
  assert.match(mismatch.stderr, /shortcut-ipc/);
  assert.match(mismatch.stderr, /unused stamped exception wrong-specifier/);
  rmSync(dir, { recursive: true, force: true });
  } finally {
    rmSync(probe, { force: true });
  }
});

test("an unparsable file fails closed", () => {
  const probe = join(root, "apps/desktop/src/renderer/src/zz-dune-parse-probe.ts");
  withFile(probe, "export const broken = ;\n", () => {
    const result = run();
    assert.notEqual(result.status, 0);
    assert.match(result.stderr, /parse error/);
    assert.match(result.stderr, /zz-dune-parse-probe\.ts/);
  });
});

test("an unresolved relative import fails closed", () => {
  const probe = join(root, "apps/desktop/src/renderer/src/zz-dune-unresolved-probe.ts");
  withFile(
    probe,
    'import { missing } from "./zz-dune-does-not-exist";\nexport const value = missing;\n',
    () => {
      const result = run();
      assert.notEqual(result.status, 0);
      assert.match(result.stderr, /unresolved import/);
    },
  );
});

test("the checked-in ledger is empty and schema-valid", () => {
  const ledger = JSON.parse(readFileSync(exceptionsPath, "utf8"));
  assert.equal(ledger.schema, "multica.dune-import-exception.v1");
  assert.deepEqual(ledger.exceptions, []);
  const policy = JSON.parse(readFileSync(policyPath, "utf8"));
  assert.equal(policy.doctrine.card, "AEGI-178");
  assert.equal(policy.doctrine.card_id, "01a0e545-edc9-793d-be5f-2b1258ad9449");
  assert.equal(policy.doctrine.baton, "01a0e5b2-e7d3-7dda-9c88-0c348a62797a");
  assert.equal(policy.sanctioned_board_client, "packages/core/api/client.ts");
});
});
