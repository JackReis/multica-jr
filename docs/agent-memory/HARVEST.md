# Agentic memory harvest — multica-jr

Audience: Jack / CoS. Date: 2026-09-27. Tree: `JackReis/multica-jr` `main` at `3551e72e7`.

**Doctrine.** Multica is the assignment source of truth. An agent trusts the board and the claim model. Private memory, a provider transcript, and a chat thread are caches. They do not assign work, do not prove a run is in flight, and do not outrank the issue.

Product words, from `apps/docs/content/docs/developers/conventions.mdx`: an **issue** is the board card. A **run** is one execution (`task` / `agent_task_queue` in the API). One issue carries many runs. Two different uses of “claim” show up in this tree; both are binding, and they are not the same record.

| Word in this harvest | Record | What it decides |
| --- | --- | --- |
| Board | Issue row: assignee, status, parent/stage, comments, properties, PR links | Who owns the work, what state it is in, what was decided |
| Ownership claim | Assignee write plus the comment history, with `--no-start` when the run is already underway | Recording ownership without starting a second run |
| Queue claim | `ClaimAgentTask`: one daemon takes the next queued run | Who may execute, under a prepare lease |

## The two records an agent must re-read

### Board

The assignee is polymorphic: `assignee_type` plus `assignee_id` point at a member, an agent, or a squad. A squad issue runs the leader, not the squad id (`server/internal/service/issue_trigger.go`, `WillEnqueueRun`).

`WillEnqueueRun` is the single predicate for “will this issue write start a run, and for whom”. It is shared by the write path and the preview. Backlog is the parking lot: assigning into a backlog-category status records the assignee and enqueues nothing. Leaving backlog for an active status enqueues, unless the caller is the agent re-triggering its own running task. A trusted self-assignment of the exact `(issue, agent)` pair that already has a non-terminal task still updates ownership and suppresses the duplicate enqueue. A handoff onto a fresh issue still starts a run. Cross-issue serial chains depend on that.

Status is a fact about the issue, written when the work changes that fact. It is not the run’s open/close lifecycle, and it is not gated on being the assignee. The runtime brief says so in `writeWorkflowIssue` (`server/internal/daemon/execenv/runtime_config_sections.go`). Agents deliver to `in_review`. `done` stays human. A turn that produced none of the issue’s own deliverable writes nothing, which is what keeps concurrent runs from flapping the column. The activity indicator shows the run; columns, filters, and sorting read status.

Comments are the conversation. Step 2 of every issue turn is a mandatory bounded scan (`--roots-only --summary`, then `--thread <id> --tail 30`). The brief’s own measurement: on 537 comment-triggered runs, 1 in 10 scans opened a thread the prompt had not named, and 0 of 36 non-scanning runs did. Typed workflow state a human should filter on lives in custom properties. Run narrative lives in the result comment. The issue metadata KV bag was retired as a scratchpad (MUL-6966); the brief no longer teaches it.

`source_context` on the issue JSON is read-only history captured at create time. Title, description, and comments are the instructions.

PR state is the link table (`multica issue pull-requests`). The skill forbids inferring it from branch names, GitHub search, memory, or a stale value an earlier run left on the issue (`references/issues.md`).

Onboarding already states the handoff the CoS needs (`multica-onboarding/SKILL.md`): the issue body must be enough for an assignee that never saw the chat. The chat coordinates. The issue performs the work. Return the identifier, the assignee, and the status.

### Queue claim

`ClaimAgentTask` (`server/pkg/db/queries/agent.sql`) claims the next `queued` row for one agent on one healthy runtime:

- The agent’s current `runtime_id` must still match the task. A rebind does not leave the old runtime as authority.
- A private runtime executes its owner’s agents.
- The runtime must be `online` inside the freshness window.
- Per `(issue, agent)` serialization: no sibling task for that pair may already be `dispatched`, `running`, or `waiting_local_directory`. Different agents may run on the same issue. Chat tasks serialize on `chat_session_id`. Quick-create tasks (no issue, chat, or autopilot) serialize against any other quick-create for that agent.
- Order is priority, then `created_at`, then id, with `FOR UPDATE SKIP LOCKED`.
- The claim sets `dispatched` and a prepare lease (`prepareLeaseDuration` = 45s in `server/internal/service/task.go`). `claimResponseRecoveryWindow` is 90s. A claim whose response never reached the daemon is reclaimed only after that window and only once the prepare lease has expired. `RequeueAgentTaskAfterClaimFailure` returns that exact `dispatched_at` generation to `queued`.
- `idx_one_pending_task_per_issue_agent_v2` (and the thread variant) is the unique slot for a not-yet-started run. A second insert no-ops. Auto-retry consults the slot and still uses `ON CONFLICT DO NOTHING`.
- Capacity is `max_concurrent_tasks` on the agent. The daemon default cap is a separate process limit (`MULTICA_DAEMON_MAX_CONCURRENT_TASKS`, default 20).
- `AgentReadiness` (`server/internal/service/agent_ready.go`) is the shared admission verdict. An offline machine still queues on the direct-agent path, because that wait ends by itself. Archived agents and agents with no runtime do not.

`multica issue runs --active` and `--siblings` are advisory. The skill says they reserve nothing and serialize nothing. A run visible now may finish a second later. Coordination after the read goes through comments. The family read is capped at 20.

Mentions are a third enqueue path, distinct from `WillEnqueueRun`. `@all` announces and suppresses the assignee’s implicit on-comment trigger. An explicit `@agent` or `@squad` in the same comment still fires. A `deferred` outcome means the input is recorded for a follow-up run, not injected into a prompt that is already running. `trigger_outcomes` on the response is the record of what the server did.

## Memory planes

These exist in the tree today. Only the first two rows may assign work.

| Plane | Scope | Lifetime | Assignment authority |
| --- | --- | --- | --- |
| Issue board | Workspace issue | Durable, human-visible | Yes. Assignee, status, stages, properties, comments |
| Run queue | `(issue, agent)` or chat session or quick-create | Until the run reaches a terminal status; pending slot is unique | Yes. Who may execute |
| Issue / chat transcript | The issue’s comments, or `chat_message` / channel history | Durable on Multica | Reconstructs the conversation. Does not assign |
| Provider session | Codex: `~/.codex/multica-sessions/<profile-hash>/<agent>/<issue or chat_…>`. Hermes: `<profile>/hermes-sessions/<agent>/<hermes-profile>/<conversation>/state.db` | Codex store follows the Codex session TTL (Hermes session default is 14d, `MULTICA_GC_HERMES_SESSION_TTL=336h`). A running task is never reclaimed | Working memory of turns. Resume aid. Lost session → continuity notice, then re-read the board |
| Hermes long-term memory | `<profile>/hermes-state/<agent>/<hermes-profile>/` linked in as `memories/` | 90d idle (`MULTICA_GC_HERMES_MEMORY_TTL=2160h`). Runtime-local. Does not follow the agent to another machine | No. Preferences and learned notes only. Last-writer-wins across that agent’s concurrent tasks |
| Codex native memories | `$CODEX_HOME/memories/` | Disabled inside daemon tasks unless `MULTICA_CODEX_MEMORY=1` | No. Opt-in reopens a cross-workspace leak |
| Host `~/.hermes` | The machine user’s Hermes home | Until the user deletes it | No. Skill-less agents still run here (open: MUL-5969). Shared across every skill-less agent on that runtime |
| Task env root | `MULTICA_WORKSPACES_ROOT/<workspace>/<task>` plus `.task_roots/<hash>/root.json` | Issue dir GC: 24h after done/cancelled; completed-task TTL 14d on Cloud, off elsewhere | No. Checkout and logs. The index freezes the physical path; a later claimant re-reads the record |
| Agent instructions, workspace context, skills | Agent row, workspace prompt, bound skills | Edited by humans | Behavior contract. Precedence can forbid status writes. Not an assignment ledger |
| Retired issue metadata | KV on the issue | CLI still exists; brief and skill no longer teach it | No. MUL-6966 wound the scratchpad down |

### What the daemon already tells the model

The runtime brief (`runtime_config_sections.go`, the only brief after MUL-4297) is cache-stable. Per-run facts ride in the user message (MUL-5377).

- Every issue turn: read the issue, scan comments, set `in_progress` when this turn will produce part of the issue’s own ask, do the work, post the result comment, confirm status before exit.
- Before self-assigning, check the comment history for an existing claim. `--no-start` records ownership for work already underway. The default start behavior is for handing off fresh work.
- Session continuity, issue variant: the issue and its comment history are the authoritative version of the conversation. What is gone is the agent’s unrecorded working memory from the turns that did not come back. Re-derive it. Do not claim continuity the record cannot back up. Channel and web-chat variants point at `multica chat history` the same way. The unrecoverable variant is a fallback no current surface uses.
- A resume may hand back an older session with the gap flagged (MUL-5305). The agent can hold earlier turns while the latest turn is gone. Both are still settled by re-reading the record.
- Quick-create has no issue yet. Exactly one `issue create`, then exit. Autopilot run-only has no assigned issue unless the autopilot instructions say to create or update one. Until an issue exists, there is nothing on the board to trust; the create payload is the contract, and the created issue becomes the record immediately after.

### Failures this tree already paid for

- Hermes skill overlay made `memories/` task-local, so binding a skill looked like amnesia. The store is now agent-scoped. Concurrent tasks of one agent still last-writer-wins because Hermes rewrites the files whole (`hermes_memory.go`, issue #6638).
- Skill-less Hermes agents share `~/.hermes` with each other. That is the remaining cross-agent memory line, deferred to `memory_scope` (MUL-5969).
- Codex auto-memory injected another project’s raw memories into a new Multica issue (`github.com/multica-ai/multica#3130`). The daemon writes a managed `config.toml` block that sets `features.memories`, `generate_memories`, and `use_memories` off. The user’s global Codex config is untouched (`codex_memory.go`).
- Codex `sessions/` used to symlink the whole `~/.codex/sessions`, and `initialize` backfilled thousands of rollouts (MUL-4424). Sessions are scoped per conversation.
- Hermes `state.db` inside the task overlay made `session/resume` succeed against an empty database, so the daemon kept re-sending a dead id (GH #6806). The shard is now per conversation, which is also the unit the queue serializes, so the SQLite file has one writer at a time.
- A status rule that lived outside the numbered workflow steps did not fire (MUL-6460). Placement in the brief is load-bearing. A CoS prompt that says “trust the board” has to sit on the step that reads the board, not in a sidebar.
- Lifecycle status writes oscillated under concurrent runs (MUL-6300). Fact writes converge.
- Issue metadata as a run scratchpad recruited a second, invisible board. MUL-6966 removed it from the brief and from `references/issues.md`.

## CoS operating rule

1. Route by creating or updating an issue: assignee, status (`todo` starts, `backlog` parks), parent, stage, and a body the assignee can execute without this chat.
2. Read assignment back from `multica issue get` and the comment scan. Read in-flight work from `multica issue runs`. Read “may this agent run” from the queue’s behavior (one pending slot, one active run per issue and agent), not from memory files.
3. Write decisions into comments and properties. A fact that never landed on the issue dies with the provider session.
4. Treat Hermes `memories/` and any opted-in Codex memories as non-assignment notes. Do not store owners, statuses, or “I already claimed this” there.
5. Pass `--no-start` when the write only records ownership. Omit it when the write is a fresh handoff.
6. After a continuity notice, re-derive from the issue. Do not announce a lost discussion when the comments are intact.

Machine index: [`INDEX.json`](./INDEX.json). Forks: [`BRANCHES.md`](./BRANCHES.md). Collapses: [`SIMPLIFICATIONS.md`](./SIMPLIFICATIONS.md).
