# Dune import bans — multica-jr

Audience: Jack / CoS. Card: **AEGI-178** `01a0e545-edc9-793d-be5f-2b1258ad9449`. Baton: `01a0e5b2-e7d3-7dda-9c88-0c348a62797a`.

**Doctrine.** Skill `dune-electron-doctrine`. Vault narrative: `Architecture/fleet/DUNE-ELECTRON-VAULT-DOCTRINE-20260927.md` §3. Dune's constraint is a dependency graph that fails the build on a cross-boundary import. This tree adapts that to Multica's planes: web UI, docs, shared views, and `packages/core` are unprivileged; Electron main and preload, and `server/`, are privileged. The renderer reaches the main process only through the preload `contextBridge` (`window.desktopAPI`). Shared modules under `apps/desktop/src/shared/` are the legal protocol both sides may import.

Multica is the one assignment plane. The sanctioned board client is `packages/core/api/client.ts`. Electron and the web/desktop UI do not grow a second client that calls `/api/issues`.

## The four edges

| Edge | What fails |
| --- | --- |
| `unprivileged-to-privileged` | UI, shared protocol, docs, or core imports Electron main, preload, or `server/` |
| `shortcut-ipc` | Renderer or shared imports `electron`, an Electron toolkit package, or a Node builtin, or one Electron zone (main / preload / renderer) imports a different zone |
| `secret-reader-in-ui` | Unprivileged code imports `daemon-manager.ts`, `daemon-profile.ts`, a keychain package, or `safeStorage` |
| `parallel-board-client` | A file under `apps/desktop`, `apps/web`, `packages/ui`, or `packages/views` calls `fetch` (or axios/ky/got) with an `/api/issues` URL |

The ledger is empty. A live edge fails CI until `scripts/dune-import-bans.exceptions.json` names that exact file, edge, and specifier, with a reason and a stamp of at least 8 characters. An unused stamp fails too. Deleting the policy, dropping an edge from it, an unparsable file, or an unresolved relative import fails closed.

`pnpm check:dune-imports` runs the gate. CI job `dune-import-graph` in `.github/workflows/ci.yml` is not path-filtered, and the required `frontend` check waits on it.

## What this gate does not cover

`apps/mobile/data/api.ts` is mobile's own issue client. Mobile is outside this gate: it does not share the Electron/UI bundles, and the card's parallel-client ban is Electron and UI. Daemon `GET /api/me` and `POST /api/tokens` in the main process are credential plumbing for the local daemon, not a second board.

Soft-spare. Do not merge until a Multica tip/SHA stamp lands on the PR.
