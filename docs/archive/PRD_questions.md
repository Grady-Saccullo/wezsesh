# PRD Questions

Open questions and gaps surfaced during PRD review. Each item has space for your answer; we'll fold the resolved decisions into `PRD_V1.md`.

Format: every question has **Context**, **Question**, **Options** (where useful), and an **Answer** block for you to fill in. There's also a free-form **General Comments** section at the bottom.

---

## A. Architectural / IPC

### A1. Multi-window IPC routing

**Context.** `wezterm cli set-user-var wezsesh_op '{...}'` sets the var on a single pane, but `user-var-changed` fires the listener in *every* window's Lua context. If a user has 3 wezterm windows open and the TUI emits `{"verb":"switch","name":"foo"}`, all 3 windows will try to handle it.

**Question.** How do we scope an op to "the window that launched the TUI"?

**Options.**
- (a) Include `target_window_id` in every payload; listener compares against `window:window_id()` and ignores mismatches.
- (b) Include `origin_pane_id` in every payload; listener resolves pane → window and compares.
- (c) Set the user-var on the *launching* pane only and rely on user-var locality if wezterm scopes events that way (needs verification).

**Answer.**
> option a

---

### A2. Error reporting back to the user

**Context.** The PRD defines `wezsesh_op_result` for one case (overwrite required on save). Every other failure mode (rename collision, illegal name, restore partial-fail, mux not reachable) is currently silent — the binary exits, the Lua op fails, and the user sees nothing.

**Question.** What's the error/feedback channel?

**Options.**
- (a) `wezsesh_op_result` for *every* op: `{ "ok": true }` or `{ "ok": false, "error": "..." }`. Lua handler logs failures via `wezterm.log_error` + status-line toast.
- (b) Lua side handles errors locally (try/pcall), surfaces via a notification mechanism (toast, status line). Binary doesn't need a return channel.
- (c) Both: structured result for testability + Lua-side toast for UX.

**Answer.**
> option c

---

### A3. Filter mode navigation semantics

**Context.** `/` enters filter, `Esc` exits. Question is what happens to navigation keys *while typing the filter*.

**Question.** How do `j`, `k`, arrow keys, `Enter` behave during filter input?

**Options.**
- (a) Telescope/fzf style: filter is always-on; arrow keys + `Ctrl-N`/`Ctrl-P` navigate; `j`/`k` insert literal characters; `Enter` confirms current selection. (No `/` needed.)
- (b) Modal: `/` enters filter mode where `j`/`k` are literal; only arrow keys + `Ctrl-N`/`P` navigate. `Esc` exits filter mode and returns to nav where `j`/`k` move.
- (c) Modal but `Esc` *clears* the filter; navigation keys always navigate, never insert.

**Answer.**
> option b

---

### A4. Concurrent TUI invocations

**Context.** `is_running()` is exposed in the API but the contract isn't defined. Two panes invoking `Leader+Shift+W` simultaneously → two TUIs, both reading/writing snapshots.

**Question.** What's the policy?

**Options.**
- (a) Hard reject: second invocation flashes a status-line message ("wezsesh already open") and does nothing.
- (b) Lockfile-based: second instance focuses the first one's pane.
- (c) Allow concurrent: each instance is independent; rely on filesystem atomicity for snapshot writes.

**Answer.**
> option c + the loading hashes of snapshots and if it has changed we need to inform the user the snapshot has changed and it's an overwrite

---

### A5. Snapshot filename collisions

**Context.** Resurrect names files via `<workspace name with / → +>.json`. Names `~/code/foo+bar` and `~/code/foo/bar` produce the same file → silent clobber on save.

**Question.** Detect-and-warn, or accept the collision since it's resurrect's bug to own?

**Options.**
- (a) On `save`/`rename`, normalize the proposed name and check if the resulting file exists *for a different workspace name* (we'd need to track the inverse mapping). Block with a confirm.
- (b) Just document the limitation; resurrect owns the name encoding.
- (c) Use our own filename hashing (breaks resurrect compat).

**Answer.**
> option b

---

## B. Missing features

### B1. Create-new-workspace from path

**Context.** §3 says wezsesh doesn't replace `smart_workspace_switcher`'s zoxide picker. But `tmux-sessionizer`/`sesh` are popular precisely because they unify "pick existing" and "create new." Without an `n` (new) action, users juggle two keybindings forever, and wezsesh feels like half a tool.

**Question.** Do we add a `new` action in v0.1? If so, how is the path source configured?

**Options.**
- (a) v0.1 adds `n` → opens a path picker. `new_workspace_command` config defaults to `zoxide query -l` if available, else `fd -t d --max-depth 4 . ~`. User can override with any shell command emitting paths to stdout.
- (b) Defer to v0.2; v0.1 ships without `new`.
- (c) Don't add it; wezsesh stays scoped to managing existing workspaces.

**Answer.**
> option a

---

### B2. Bulk operations for P2 (spring cleaner)

**Context.** P2 deletes stale snapshots weekly. PRD only specs single-row delete with y/N confirm. P2 will hate this in practice.

**Question.** When do we add multi-select?

**Options.**
- (a) v0.1: `Tab`/`Space` toggles a mark; `d` deletes all marked (or current row if none marked).
- (b) v0.2: ship simple delete first, add bulk later.
- (c) Different UX: dedicated "manage" mode behind a key (`m`) where multi-select is enabled.

**Answer.**
> a

---

### B3. Last-used tracking separate from mtime

**Context.** Mtime ≠ recency. A snapshot saved once and re-entered 100 times has stale mtime. `live_first` + mtime sort misranks heavily-used workspaces.

**Question.** Do we track real "last switched to" times?

**Options.**
- (a) Yes, in `~/.local/state/wezsesh/usage.json` (XDG state dir). Bump timestamp on every `switch`. Add `sort = "recent"` mode using this data.
- (b) Hybrid: tracking optional, default off; enabled via `track_usage = true`.
- (c) No; mtime-on-save is good enough.

**Answer.**
> option a

---

### B4. Preview pane timing

**Context.** PRD slots preview pane in v1.0. `choose-tree` and `sesh -p` both ship with previews; without it, wezsesh is feature-behind alternatives at "1.0."

**Question.** Pull preview into v0.2?

**Options.**
- (a) Yes, v0.2: right-side pane shows tabs/CWDs/commands of highlighted workspace, parsed from snapshot JSON.
- (b) Stay in v1.0.
- (c) Even earlier, v0.1: ship MVP with preview from the start.

**Answer.**
> option c

---

### B5. `wezsesh doctor` diagnostic subcommand

**Context.** Without a doctor command, debugging user issues is a back-and-forth ("run this, paste that"). With it, one paste tells us everything.

**Question.** Add `wezsesh doctor` checking: binary version, plugin version match, snapshot dir reachable + writable, resurrect plugin loaded, wezterm CLI reachable, nerdfont detected, `WEZTERM_PANE` set, terminal capabilities.

**Options.**
- (a) v0.1 — table stakes for issue triage.
- (b) v0.2.
- (c) Skip; rely on good error messages.

**Answer.**
> option a + this should be built early to aid in the dog fooding and development process

---

### B6. Auto-install of the binary

**Context.** PRD says plugin detects missing binary and prints an actionable error. But the dual-install (plugin via git URL, binary via go/nix/release) is real friction — likely the #1 bounce reason for new users.

**Question.** Auto-download the binary on first use?

**Options.**
- (a) Yes: on first use, plugin downloads the matching version from GitHub releases (with checksum), caches in `~/.local/share/wezsesh/`. Like nvim treesitter parsers.
- (b) Provide an `install` helper command but don't auto-run it.
- (c) Status quo: error + instructions.

**Answer.**
> we should keep this inline with existing wezterm plugin eco system. we should use nix to aid in development process, but this should be built/installed the same way wezterm plugins work

---

## C. Smaller gaps

### C1. Logging location

**Context.** Not specified for either binary or Lua plugin.

**Question.** Where do logs go?

**Options.**
- (a) Binary: `$XDG_STATE_HOME/wezsesh/wezsesh.log` (or `~/.local/state/wezsesh/wezsesh.log`). Lua: `wezterm.log_info`/`log_error` (visible in debug overlay). Verbosity via `WEZSESH_LOG=debug`.
- (b) Both into a single file.
- (c) No persistent logging; ephemeral stderr only.

**Answer.**
> option a + these should live in the .local/state. log level should be configurable via both lua config and env var. lowest level log is used when config and env differ (e.g. config = info, env = debug, debug is selected)

---

### C2. Theme palette breadth

**Context.** PRD allows `colors = { accent, muted }`. Two colors isn't enough for a TUI with error/success/dim/focus/match-highlight states.

**Question.** Expand the palette?

**Options.**
- (a) Add: `accent`, `muted`, `error`, `success`, `focus_bg`, `match_highlight`, `live_marker`, `saved_marker`. All optional; sensible defaults inherit from terminal.
- (b) Don't let users pick colors; just inherit terminal ANSI palette via semantic roles.
- (c) Keep two; theming isn't a priority.

**Answer.**
> option a

---

### C3. Long path truncation

**Context.** `~/code/some/very/long/project/name` overflows the name column.

**Question.** Truncation strategy?

**Options.**
- (a) Middle-truncate with ellipsis (`~/code/…/name`).
- (b) Left-truncate (`…long/project/name`).
- (c) Hard wrap to next line.
- (d) Configurable via `name_truncate = "middle" | "left" | "none"`.

**Answer.**
> option a

---

### C4. Resurrect schema drift

**Context.** We parse resurrect's JSON for tab/pane counts and preview info. If they rename fields, we break.

**Question.** How tolerant is the parser?

**Options.**
- (a) Tolerant: missing fields → render `--`; warn once per session via log.
- (b) Strict: error on unknown shape; bump our minimum supported resurrect version.
- (c) Tolerant + a known-schema-version field; if unfamiliar, run in degraded mode.

**Answer.**
> needs deeper investigation on resurrect ABI and how they version schema before decision can be made

---

### C5. Empty-state for first-time users

**Context.** §5.5 covers no-workspaces, missing CLI, missing snapshot dir. Doesn't cover: dirs exist but empty + no live workspaces (true first-run).

**Question.** Onboarding nudge?

**Options.**
- (a) Show: "No workspaces yet. Press `n` to create from a directory, or `S` to save the current workspace."
- (b) Reuse the existing "no saved or live workspaces" string.
- (c) Open the new-workspace flow automatically on empty state.

**Answer.**
> option a

---

### C6. Rename atomicity

**Context.** Rename touches both the snapshot file (rename on disk) and the mux state (`mux.rename_workspace`). If one succeeds and the other fails, state diverges.

**Question.** What's the order, and what's the rollback?

**Options.**
- (a) Rename mux first (cheap, in-memory). If success, rename file. If file rename fails, attempt to undo the mux rename and surface error.
- (b) Rename file first. If success, rename mux. If mux fails, rename file back.
- (c) Two-phase commit via a temp file; only swap once both phases prepared. Heavyweight but atomic.

**Answer.**
> option a

---

### C7. Restore-partial-failure behavior

**Context.** P3 cares about "predictable behavior if a snapshot is partial." If `resurrect.restore_state` fails partway (some tabs restored, some failed), what does wezsesh do?

**Question.** Defined behavior?

**Options.**
- (a) Trust resurrect's behavior; just surface whatever error it returns.
- (b) Wrap restore in a transaction: pre-snapshot the workspace, restore, on error roll back.
- (c) Document as undefined; punt to resurrect.

**Answer.**
> we need to investigate this deeper before making a judgment call

---

### C8. Default action on saved-not-live with `default_action = "load"`

**Context.** If user configures `default_action = "load"`, pressing Enter on a saved-not-live entry loads that snapshot *into the current workspace*, clobbering it. Power feature, but dangerous default.

**Question.** Guard rails?

**Options.**
- (a) Require an extra confirm if Enter would clobber a non-empty current workspace.
- (b) `default_action` only applies to live entries; for saved-only, Enter always means "switch + restore."
- (c) Trust the user; they configured it.

**Answer.**
> option a + ability to set a "load_unsafe_no_prompt" or something along those lines

---

### C9. Help overlay content scope

**Context.** `?` toggles help, but content isn't specced.

**Question.** What's in it?

**Options.**
- (a) All keybindings (including the user's configured overrides), grouped by category (nav / ops / modes), plus version + binary path.
- (b) Just keybindings, no metadata.
- (c) Full keybindings + a one-liner usage tip + link to README.

**Answer.**
> option a

---

### C10. "Unsaved" marker visibility

**Context.** `markers.unsaved = "(unsaved)"` is in the config but doesn't appear in the §5.1 mock. When does it render?

**Question.** Always, only in some sorts, or only as a column?

**Options.**
- (a) Always render next to the name when a live workspace has no snapshot.
- (b) Only when `sort = "live_first"` (since that's where it's most relevant).
- (c) Make it a separate column.

**Answer.**
> option a

---

## D. Stretch-but-worth-deciding-now

### D1. Workspace tags / categories

**Context.** Listed as stretch in §8. Worth claiming the data model now so it can land cleanly later.

**Question.** Reserve `tags: string[]` field in the snapshot sidecar (a `.wezsesh.json` next to resurrect's file)? Or never?

**Answer.**
> Yes and this is something we could even add now. Tags saved into a .wezsesh.json would be a day one net benefit

---

### D2. Per-workspace launch hooks

**Context.** Listed as stretch. "Auto-launch commands on restore" — e.g. start a dev server, open a specific file.

**Question.** In scope eventually? If yes, sidecar file or embedded?

**Answer.**
> This isn't scoped yet, if this is genuinely beneficial we should spec this for day one

---

### D3. Search across snapshot contents (CWDs, commands)

**Context.** Power feature — "find the workspace where pgcli was running." Snapshot JSON has the data.

**Question.** Worth shipping? Behind a key (`?` already taken — maybe `Ctrl-/`)?

**Answer.**
> This would be worth shipping potentially, however the feature might not be used. I would use this to find the pane/tab where in wezterm where pgcli is running in the current/other workspaces, that would be genuinely useful.

---

### D4. Pin / favorite workspaces

**Context.** Frequent users want their top 3-5 always at the top.

**Question.** Add `p` to toggle pin? Persist pins where (state file)?

**Answer.**
> Yes this is a good idea. Persist to the state file

---

## General Comments

Free-form notes, reactions, additional ideas, or anything that didn't fit a question above:

> Most of the items we want to ship as part of v0.1. There is the notion of v0.2 and v1.0, but right now we don't know what those are. we can have a deferred section, but we shouldn't be already planning future versions when we don't what this plugin will turn into

---

**Once you've filled this in, I'll fold the answers into `PRD_V1.md`.**
