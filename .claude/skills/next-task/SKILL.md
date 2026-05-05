---
name: next-task
description: Advances the wezsesh build by one task. Reads PROJECT.md, picks the next task (preferring in-progress > needs-review > T-DOC > T-XXX ready), dispatches the listed Owner agent in isolation, runs the listed acceptance gates, runs design-conformance-reviewer on the diff, commits via jj, and updates PROJECT.md. Resumable across sessions because state lives in PROJECT.md and the jj op log.
---

# next-task

Advance the wezsesh build by exactly one task. Each invocation is self-contained
and resumable from a fresh `claude` session.

The repo is **jj-colocated** (`.jj/` + `.git/` side by side). This skill uses
`jj` for all VCS operations. Pre-existing git tooling (CI, IDE, GitHub) sees
the colocated `.git/` and continues to work normally.

## Preconditions

Run these before doing anything else. If any fails, report the failure verbatim and stop.

1. `test -d .jj && test -d .git` — the repo must be jj-colocated.
2. `test -f PROJECT.md` — the ledger must exist.
3. `test -f docs/design.md` — the design spec must exist.
4. **Working-copy commit must be empty.** Run:
   ```
   jj log -r '@ & ~empty()' --no-graph -T 'change_id ++ "\n"'
   ```
   The output MUST be empty. If it returns a change_id, the working-copy
   commit has uncommitted changes — list them with `jj diff --name-only`,
   stop, and tell the user to either:
   - resume with `/next-task` (if a previous iteration left the in-progress
     work to be picked up — the change is described and committed by the
     resuming task), OR
   - run `jj abandon` to discard, OR
   - inspect with `jj op log` and `jj op restore <op-id>` to roll back.
5. **Working copy is on or descended from `main`.** Run:
   ```
   jj log -r 'main..@-' --no-graph -T 'change_id'
   ```
   Empty output means @'s parent IS main. Non-empty means @ is on a side
   stack — ask before proceeding.

## Pick the task

Pick exactly one task from `PROJECT.md`, in this priority order:

1. The first task with `Status: in-progress` (a previous session was interrupted).
2. The first task with `Status: needs-review` (regardless of phase order).
3. The first `Status: ready` task in the `## Doc-update tasks (T-DOC-NNN)`
   section (spec drift gets corrected before more build tasks pile on top of it).
4. The first `Status: ready` task in the build phases (T-XXX), scanning
   top-to-bottom.

If neither exists:
- If any tasks remain `blocked`, list which dependency is missing for the
  earliest blocked task and stop with "no ready tasks; blocked on T-XXX".
- If everything is `done`, congratulate the user and stop.

If a single task can be picked, **set its `Status: in-progress`** in `PROJECT.md`
and commit BEFORE doing implementation work:

```
# edit PROJECT.md to flip status to in-progress
jj describe -m "chore(project): pick T-XXX"
jj bookmark set main -r @
jj new
```

This three-step (`describe` → `bookmark set` → `new`) is the standard commit
flow throughout this skill. It (1) describes the working-copy commit with the
message, (2) advances `main` to point at it, (3) starts a fresh empty
working-copy on top so the precondition stays satisfied for the next call.

If the session crashes mid-task, the next `/next-task` sees `in-progress` and
picks up where it left off (highest priority in pick order).

## Build the brief for the Owner agent

Extract the task's section from `PROJECT.md` verbatim. Construct a self-contained
prompt for the agent named in `Owner`:

```
You are implementing task <T-XXX> for wezsesh. Full task brief follows.

<paste the entire task section verbatim>

Additional context:
- Read the cited §sections of `docs/design.md` first. § references are durable;
  line numbers may have drifted. Use ripgrep / `grep -n "## §<N>"` to locate.
- The PRD (`docs/prd.md`) carries rationale; consult it only when a `(P §x.y)`
  ref appears in the spec section.
- This project's invariants live in `CLAUDE.md`. Read it.
- Stay strictly within the task's `Files` list. If you need a file outside it,
  STOP and report — do not silently expand scope.
- Tests are not optional. Every gate in `Acceptance gates` must have a Go test
  (or Lua spec) that exercises it; the test must pass locally.
- After implementing, run the test commands from `CLAUDE.md` and report results.
- Do not run jj or git. The /next-task driver handles the commit.
- The repo is jj-colocated; your file edits are auto-snapshotted into the
  working-copy commit. You do not need to "stage" anything.

Return when:
- All listed Files exist and compile / parse.
- All listed Acceptance gates have a corresponding test that passes.
- You have run the appropriate `go test ./...` / lua spec command(s) and they are green.
- You have NOT modified any file outside the task's `Files` list.

Spec gaps and ambiguities (AUTONOMOUS RESOLUTION — never end with a question):
- If the spec is out of date with the upstream world (a pinned version no longer
  publishes under the listed module path, an upstream API was renamed, etc.) AND
  there is exactly one obviously-correct local substitution: make it, land it as
  part of this task's diff, document it under "Accepted findings" in the task
  body. The /next-task driver will queue a T-DOC-NNN task automatically (see the
  driver's "Spec drift handling" section). Do NOT ask the user.
- If the spec is internally inconsistent (says X here, Y there) and you can pick
  the load-bearing reading: do so, document the choice, let the driver queue a T-DOC.
- If neither applies — the spec is silent on a required decision, or there's a
  hard environmental blocker (network down, declared dependency yanked, test
  infra missing) — STOP. Return with `BLOCKED: <one-line reason>` as the first
  line of your report. The driver will mark the task `needs-review` and stop the
  loop. NEVER end your output with a question to the user; this loop is
  unattended and your final text is the only signal that survives.
- Never silently expand the task's `Files` list. If you need a file not listed,
  that is a `needs-review` outcome — return with `BLOCKED: out-of-scope file
  needed: <path>`.
```

Dispatch this brief to the agent named in the task's `Owner` field. Use the
`Agent` tool with `subagent_type` set to that exact name.

## Verify the work

After the implementation agent returns, the working-copy commit `@` now contains
all of the agent's edits (jj snapshots automatically; no `git add` needed).

1. **Compile / vet check.** Run `go build ./...` and `go vet ./...`. Both must
   pass. For Lua-only tasks, run `cmd/lualint` against the changed files.
2. **Diff allowlist check (replaces git's explicit-staging discipline).**
   Run `jj diff --name-only`. The output MUST be a subset of the task's
   declared `Files` list ∪ `{PROJECT.md}`. If a non-listed file appears:
   - DO NOT advance the task to `done`.
   - Edit PROJECT.md to set `Status: needs-review` and append a checklist
     item: `- Out-of-scope file modified: <path>`.
   - Commit `chore(project): T-XXX needs-review (out-of-scope file)`.
   - Stop and report.
3. **Run the named acceptance gates.** Each gate in `Acceptance gates` names a
   §17.3 test. Identify the corresponding `*_test.go` (or `*_spec.lua`) and run it.
   If a gate names a §17.4 / §16.5 lint, run that lint.
4. **Locale-sensitive tests.** If the task touches `internal/canonicaljson` or any
   plugin module, also run `LC_ALL=C go test ./internal/canonicaljson/...
   ./plugin/...` per §16.4.

## Conformance review

Dispatch `design-conformance-reviewer` with the diff scope:

```
Review the diff for task T-XXX (see PROJECT.md for the brief). The diff lives
in the working-copy commit `@`; use `jj diff` or `jj show @` to see it. The
parent commit (the pre-task baseline) is `@-`. Walk every checklist section
in your loadout, citing concrete spec quotes for any finding. Output the
standard punch list.
```

If the reviewer reports any `CRITICAL` or `HIGH` finding:
- Edit PROJECT.md to set `Status: needs-review` and append a checklist of the
  findings to the task body (not the index).
- Commit:
  ```
  jj describe -m "chore(project): T-XXX needs-review (<N> findings)"
  jj bookmark set main -r @
  jj new
  ```
- Stop and report to the user.

If the reviewer is clean OR reports only `MEDIUM`/`LOW`:
- For `MEDIUM`/`LOW`, append a one-line "Accepted findings:" note to the task
  body explaining the call.
- Continue to commit.

## Spec drift handling

If the implementation agent's "Accepted findings" or the conformance reviewer's
findings name a spec section (typically a `§X.Y` ref in `docs/design.md` or
`docs/prd.md`) AND the implementation diverged from what the spec literally says,
the spec is drifted. Queue a doc-update task BEFORE the final commit:

1. Scan PROJECT.md's "Doc-update tasks (T-DOC-NNN)" section for the next free id.
2. Append the new task with this skeleton (under the `## Doc-update tasks` heading):

   ```markdown
   ### T-DOC-NNN · <one-line description of the drift>
   **Status:** ready
   **Owner:** general-purpose
   **Depends-on:** —
   **Spec:** `docs/design.md` §X.Y (and/or `docs/prd.md` §X.Y)
   **Files:** `docs/design.md` (and/or `docs/prd.md`)
   **Discovered in:** T-XXX
   **Acceptance gates:**
   - The §X.Y section reflects the implementation reality.
   - `rg -F '<old-string>' docs/` finds no stale refs (or only refs in
     `docs/archive/` if those are explicitly excluded).
   **Done when:** the spec quote matches the implementation; no other §section
   carries a stale reference.
   ```

3. The PROJECT.md edit is auto-included in the working-copy commit (jj snapshot).
   The T-DOC task ships in the SAME commit as the implementation that discovered
   it. Note that PROJECT.md is implicitly always in the diff allowlist.

T-DOC tasks have no `Depends-on` and are always `ready` at creation. They are
the ONLY tasks allowed to edit `docs/design.md` / `docs/prd.md`.

## Commit

One commit per task. The commit covers BOTH the implementation diff AND the
PROJECT.md status flip to `done`.

1. Edit PROJECT.md: flip the task's `Status: in-progress` → `Status: done`.
   Append "Accepted findings:" line if any were carried.
2. Commit:
   ```
   jj describe -m "<message>"
   jj bookmark set main -r @
   jj new
   ```

Message format:

```
<type>(<scope>): T-XXX <task-title>

<one-or-two-line summary of what changed and why>

Acceptance gates:
- <name>: pass
- <name>: pass
...
Conformance review: clean (or: 2 MEDIUM findings accepted — see PROJECT.md T-XXX)
```

`<type>` is `feat` for new packages / files, `chore` for tooling, `test` only if
the task is purely test-side (rare). `<scope>` is the package or surface:
`safefs`, `plugin`, `cmd/wezsesh`, etc.

DO NOT push. The build-loop driver runs `jj git push` separately if configured.

## Update unblocked downstream tasks

After the commit lands, scan `PROJECT.md` for tasks with `Status: blocked` whose
`Depends-on` list now consists entirely of `done` tasks. Flip those to `Status: ready`.

Commit:
```
jj describe -m "chore(project): unblock T-AAA, T-BBB after T-XXX"
jj bookmark set main -r @
jj new
```

If no tasks become unblocked, skip this step (do NOT create an empty commit).

## Report

Output a short summary:

```
Task: T-XXX <title>
Owner: <agent>
Files changed: N
Tests added: M
Acceptance gates: K passed
Conformance: clean | <N> findings (severity)
Newly ready: T-AAA, T-BBB
Commit: <change-id-short> (<commit-id-short>)
```

Get the change-id and commit-id with:
```
jj log -r main --no-graph -T 'change_id.short() ++ " " ++ commit_id.short()'
```

## Recovery (when called by the user, not by the loop)

If a previous iteration crashed and left the working-copy commit non-empty
(precondition #4 fails) and the user invokes this skill specifically to recover:

- `jj op log` — show the operation history. Each `claude` operation appears.
- `jj op restore <op-id>` — restore the entire repo to a snapshot from any
  prior op. Atomically undoes commits, working-copy state, and bookmark moves.
  Use this to wipe a bad iteration cleanly.
- `jj abandon @` — discard the current working-copy changes only.
- `jj squash` — fold the working-copy changes into the parent commit (if you
  want to keep the changes but not as a separate commit).

After recovery, re-run /next-task. The skill itself NEVER invokes these
destructive operations on its own.

## Guardrails

- NEVER run `jj git push` from this skill.
- NEVER use destructive jj operations (`jj abandon`, `jj op restore`, `jj rebase`,
  `jj split`) from automated execution. They appear under "Recovery" only,
  invoked explicitly by the user.
- NEVER modify `docs/design.md` or `docs/prd.md` from a regular `T-XXX` build task.
  Spec drift discovered during a build task is queued as a `T-DOC-NNN` task (see
  "Spec drift handling"), which is the ONLY context where docs may be edited.
- NEVER end with a clarifying question. The loop is unattended; questions never
  reach a human. If you can't decide autonomously, set `Status: needs-review`
  and stop. The user will see it in PROJECT.md / build.log.
- NEVER mark a task `done` if any acceptance gate failed or any out-of-scope
  file appears in `jj diff --name-only`.
- If two tasks could run in parallel (no shared deps, different agents), this skill
  still does ONE per invocation. Parallelism is a separate session-management
  concern (separate worktrees), not a skill-level concern.
- If `Owner` is `general-purpose`, this skill MAY do the implementation inline
  (without dispatching a subagent) — but isolation is still preferred for
  reviewability. Default to dispatching a subagent unless the task is purely
  scaffolding (T-000, T-001, T-002).
