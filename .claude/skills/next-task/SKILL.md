---
name: next-task
description: Advances the wezsesh build by one task. Reads PROJECT.md, picks the next task (preferring needs-review over ready), dispatches the listed Owner agent in isolation, runs the listed acceptance gates, runs design-conformance-reviewer on the diff, commits, and updates PROJECT.md. Resumable across sessions because state lives in PROJECT.md, not conversation context.
---

# next-task

Advance the wezsesh build by exactly one task. Each invocation is self-contained
and resumable from a fresh `claude` session.

## Preconditions

Run these before doing anything else. If any fails, report the failure verbatim and stop.

1. `git status --porcelain` — the working tree must be clean. Uncommitted changes
   from a prior interrupted task should be either committed or discarded by the
   user before this skill runs. If dirty, list the dirty paths and stop.
2. `test -f PROJECT.md` — the ledger must exist.
3. `test -f docs/design.md` — the design spec must exist.
4. The current branch must be `main` OR a branch the user explicitly named in args.
   If on a non-`main` branch and the user did not pass an arg, ask before proceeding.

## Pick the task

Pick exactly one task from `PROJECT.md`, in this priority order:

1. The first task with `Status: needs-review` (regardless of phase order).
2. Otherwise, the first task with `Status: ready`, scanning top-to-bottom.

If neither exists:
- If any tasks remain `blocked`, list which dependency is missing for the
  earliest blocked task and stop with "no ready tasks; blocked on T-XXX".
- If everything is `done`, congratulate the user and stop.

If a single task can be picked, **set its `Status: in-progress`** in `PROJECT.md`
in a single commit titled `chore(project): pick T-XXX` BEFORE doing implementation
work. This way, if the session crashes mid-task, the next `/next-task` invocation
sees `in-progress` and picks up where it left off (treat `in-progress` like
`needs-review` for resume purposes).

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
- Do not commit. The /next-task driver handles the commit.

Return when:
- All listed Files exist and compile / parse.
- All listed Acceptance gates have a corresponding test that passes.
- You have run the appropriate `go test ./...` / lua spec command(s) and they are green.
- You have NOT modified any file outside the task's `Files` list.

If you hit a blocker (spec ambiguity, missing dependency, environmental failure):
return immediately with a short report describing what's blocking and what you'd
need to proceed. Do not patch around the issue.
```

Dispatch this brief to the agent named in the task's `Owner` field. Use the
`Agent` tool with `subagent_type` set to that exact name.

## Verify the work

After the implementation agent returns:

1. **Compile / vet check.** Run `go build ./...` and `go vet ./...`. Both must
   pass. For Lua-only tasks, run `cmd/lualint` against the changed files.
2. **Run the named acceptance gates.** Each gate in `Acceptance gates` names a
   §17.3 test. Identify the corresponding `*_test.go` (or `*_spec.lua`) and run it.
   If a gate names a §17.4 / §16.5 lint, run that lint.
3. **Locale-sensitive tests.** If the task touches `internal/canonicaljson` or any
   plugin module, also run `LC_ALL=C go test ./internal/canonicaljson/...
   ./plugin/...` per §16.4.
4. **No-scope-creep check.** Run `git diff --name-only` against the pre-task SHA
   and verify every changed path appears in the task's `Files` list (allow
   PROJECT.md itself; that's the next step). If a non-listed file changed, the
   task is `needs-review` — do NOT advance.

## Conformance review

Dispatch `design-conformance-reviewer` with the diff scope:

```
Review the diff for task T-XXX (see PROJECT.md for the brief). The pre-task SHA
is <SHA>. Walk every checklist section in your loadout, citing concrete spec
quotes for any finding. Output the standard punch list.
```

If the reviewer reports any `CRITICAL` or `HIGH` finding:
- Set the task `Status: needs-review` and append a checklist of the findings to
  the task body (not the index).
- Commit `PROJECT.md` with a message `chore(project): T-XXX needs-review (<N> findings)`.
- Stop and report to the user.

If the reviewer is clean OR reports only `MEDIUM`/`LOW`:
- For `MEDIUM`/`LOW`, append a one-line "Accepted findings:" note to the task
  body explaining the call.
- Continue to commit.

## Commit

One commit per task. The commit covers BOTH the implementation diff AND the
PROJECT.md status flip to `done`. Message format:

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

Stage explicitly — never `git add -A`. Stage exactly the task's listed `Files`
plus `PROJECT.md`. Commit. Do NOT push.

## Update unblocked downstream tasks

After the commit lands, scan `PROJECT.md` for tasks with `Status: blocked` whose
`Depends-on` list now consists entirely of `done` tasks. Flip those to `Status: ready`.
Commit this in a separate `chore(project): unblock T-XXX, T-YYY after T-ZZZ` commit.

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
Commit: <SHA short>
```

## Guardrails

- NEVER push to a remote.
- NEVER force-push or amend a published commit.
- NEVER modify `docs/design.md` or `docs/prd.md` from this skill — if the implementation
  agent reports a spec gap, surface it to the user with a recommendation to update
  the spec in a separate, explicit task.
- NEVER mark a task `done` if any acceptance gate failed.
- NEVER expand a task's `Files` list silently. If the implementation agent says
  it needs another file, that's a `needs-review` outcome, not a green light.
- If two tasks could run in parallel (no shared deps, different agents), this skill
  still does ONE per invocation. Parallelism is a separate session-management
  concern (separate worktrees), not a skill-level concern.
- If `Owner` is `general-purpose`, this skill MAY do the implementation inline
  (without dispatching a subagent) — but isolation is still preferred for
  reviewability. Default to dispatching a subagent unless the task is purely
  scaffolding (T-000, T-001, T-002).
