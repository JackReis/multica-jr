# Simplifications — one ergonomic assignment model

Goal: a CoS agent can route work without keeping a second ledger in its head, its Hermes `memories/`, or a provider transcript. Each item says what is already true in `multica-jr` at `3551e72e7`, and what is still open.

Doctrine and the plane table: [`HARVEST.md`](./HARVEST.md). Forks: [`BRANCHES.md`](./BRANCHES.md).

## S1 — One sentence on every issue turn

Already in the brief, as six numbered steps rather than a slogan. Keep it that way. The MUL-6460 incident showed a status rule parked outside the numbered list did not fire.

The sentence those steps implement:

> Read the issue and its comments, write status when the issue’s fact changes, post the result on the issue, and treat anything you did not write down as gone if this session does not resume.

Do not add a parallel “memory policy” section to the brief. The platform skill test rejects workflow sentences the brief owns, including “Start from the trigger, not from memory”, so a second copy cannot drift from `writeWorkflowIssue`.

## S2 — Assignment facts have one home

| Fact | Home | Stop writing it to |
| --- | --- | --- |
| Owner | `assignee_type` + `assignee_id` | Hermes `MEMORY.md`, Codex memories, chat-only notes |
| Parked versus running | Status category (`backlog` versus `todo` / `in_progress` / …) | A CoS checklist in the orchestrator’s context |
| Order of children | `parent` + `stage`, promote with `issue status … todo` | A chain remembered by the parent agent |
| Filterable workflow | Custom properties | Issue metadata KV |
| What this run found | Result comment | The provider transcript alone |
| Whether a PR is merged | `multica issue pull-requests` | Branch names, search, memory |

Onboarding already requires the issue body to stand alone for a fresh assignee. CoS routing should do the same: create the child, set assignee and status, return the identifier. The parent’s context is not the system of record.

## S3 — Two verbs, picked on purpose

| Verb | Flag | Use |
| --- | --- | --- |
| Hand off | omit `--no-start`, status not in the backlog category | The assignee should get a run |
| Record | `--no-start` on every command in the flow | The run already exists; the write is ownership or progress |

The server covers one case the client forgets: a trusted self-assignment of a pair that already has a non-terminal task does not enqueue a duplicate. It does not cover a sloppy `--no-start` omitted on a fresh issue. CoS playbooks name the verb in the issue body (“record only” versus “start now”) so the assignee does not guess.

## S4 — Keep Codex memories off

Already the daemon default (`codex_memory.go`). CoS runtimes leave `MULTICA_CODEX_MEMORY` unset. The long-term note in that file is the simplification: durable context is a Multica-owned, user-visible, issue-scoped record. Re-enabling Codex auto-memory is the opposite move. The leak it closed was host-project memories appearing inside a new issue (#3130).

## S5 — Hermes notes are not a board

Already isolated per agent and Hermes profile, once the overlay exists, and kept out of the task directory so binding a skill no longer wipes them (#6638).

Still open, in this order:

1. MUL-5969 `memory_scope`: skill-less Hermes still shares `~/.hermes`. That is a cross-agent memory line. A CoS fleet should not depend on the host file for anything assignment-shaped, and the platform fix is to give those agents the same store.
2. Concurrent tasks of one agent last-writer-wins on `memories/`. Until Hermes stops rewriting the files whole, those files cannot hold a claim. The queue already serializes a single conversation; it does not serialize two issues of the same agent. So two issues can clobber one `MEMORY.md`.
3. The store is runtime-local. An agent on two machines has two memory lines and one board. The board is the line that travels.

GC already encodes the right severity: 14 days for transcripts, 90 days for `memories/`, and a running task is never reclaimed. Do not shorten the memory TTL to “clean up assignments.” Assignments are not in that directory.

## S6 — Session loss is a re-read, not a reset of the issue

Already implemented as `SessionContinuityNoticeIssue` and the channel/chat variants. The issue (or the chat transcript) stays. Unrecorded working memory is what dies.

CoS habit that matches the code: if the next agent must know it, it is a comment, a property, or a child issue. “What I tried and ruled out” belongs in the result comment when a later run would otherwise repeat it. The continuity notice tells the agent to raise the gap only where the user refers to reasoning that was never written down.

## S7 — One pending run, looked up rather than remembered

Already enforced: `ClaimAgentTask` serialization, `idx_one_pending_task_per_issue_agent_v2`, prepare lease, and exact-generation requeue. `issue runs` is the human-visible read, and it is advisory.

Simplification for agents: do not keep a private “I am already running” bit. Before a self-assign, read the comment history (the brief’s step). Before a second PR on shared code, read `issue runs --siblings` and then comment. The cap warning on stderr is part of the answer; a short list with a warning is not an empty board.

## S8 — Do not bring the scratchpad back

MUL-6966 is the simplification. Metadata commands still exist so old clients do not break. New CoS instructions, skills, and briefs do not name them. The skill test treats a reintroduction of that vocabulary as a decision that has to be reopened on purpose.

## S9 — Quick-create and run-only autopilot stay outside the board until they aren’t

Already guarded: quick-create performs one create and exits; run-only autopilot does not touch issue commands unless its instructions say so. The simplification is to leave those guards alone. A CoS flow that needs assignment creates the issue first and lets the assignee’s run read the board. It does not ask the quick-create turn to remember the plan.

## S10 — Prompt surface stays single

Already the brief’s contract: cache-stable rules in the runtime brief, per-turn facts in the user message, platform contracts in `multica-platform/references/issues.md`, names only in the skills index. CoS agent instructions (`Agent Identity`) may forbid actions. They should not restate the claim algorithm or the status matrix. Identity precedence skips conflicting workflow steps; a second matrix in the identity will drift from `WillEnqueueRun` the way the old per-handler enqueue copies did (MUL-3375).

## Open platform work, not a CoS workaround

| Id | What it simplifies | CoS stance until it lands |
| --- | --- | --- |
| MUL-5969 | One memory scope for every Hermes agent, including skill-less | Assume host `~/.hermes` is shared and untrusted for assignment |
| Hermes concurrent `memories/` | A real merge, or a per-issue note file | Never store owners or statuses in `MEMORY.md` |
| `memory_scope` called out in `codex_memory.go` | A visible issue-scoped store instead of Codex’s hidden one | Keep the disable flag; put durable facts on the issue |

No code change belongs in this harvest. The ergonomics are already pointed at the board; the remaining leaks are the host Hermes home, last-writer-wins notes, and any operator who sets `MULTICA_CODEX_MEMORY=1`.
