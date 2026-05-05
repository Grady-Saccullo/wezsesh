# V1 Ñ wezsesh as a work-intelligence layer

> **Status:** strategic vision document. Picked up *after* the current
> `PROJECT.md` ledger lands v0.1 (Phase 0Ð9 complete, Phase 10 integration
> bugs closed, Phase 11 release engineering done).
>
> When v0.1 ships, `PROJECT.md` will be deleted and a fresh ledger will be
> generated from this document plus a new `docs/design.md` revision. This
> file is the **bridge** Ñ what to build, why, and what order.

---

## The reframe

Every workspace switcher answers a single question: *"which thing do you
want to be in?"* That's a fine question. It's also a small one. The
interesting questions Ñ the ones that matter to a developer who lives in
the terminal all day Ñ are larger:

- *What was I doing on this when I left?*
- *What changed since I was last here?*
- *Where did I leave that build running?*
- *What did I try yesterday that didn't work?*
- *What am I actually spending time on?*

A workspace switcher answers none of these. A **work journal** Ñ a
time-aware record of your terminal activity Ñ answers all of them.

wezsesh is uniquely positioned to be that, because it sits at a layer no
other tool occupies: it sees both *live wezterm state* (panes, processes,
cwds, scrollback via `wezterm cli`) and *historical snapshots* (resurrect
JSON), and it has a hook system that can intervene at the seams.

**Thesis:** wezsesh is the layer of your terminal that has memory.

The DevX win is not "the picker is prettier." It's that wezsesh remembers
things you forgot, surfaces them at the right moment, and removes the
cognitive overhead of context-switching entirely.

---

## What's broken in v0.1

v0.1 is a faithful implementation of the spec, but the spec frames wezsesh
as a switcher. The result is functional but inert:

- The picker shows a list of names with sparse metadata.
- Saving and loading are explicit user ceremonies.
- There's no record of what happened Ñ every session begins amnesic.
- The TUI's lipgloss/colors config is wired but unused; everything renders
  as default-fg plaintext.
- The preview shows a tab/pane count, not what was *happening* in those
  panes.
- Old workspaces accrete; cleanup is manual; the list grows linearly until
  it's unusable.

None of these are bugs. They're consequences of the v0.1 framing. v1
re-frames.

---

## Foundation Ñ what v1 keeps from v0.1

Before talking about the journal layer, the explicit promise: **every
workspace-management operation v0.1 ships stays first-class in v1.**
The intelligence layer is additive context *on top of* a solid
switcher, not a replacement for it.

If a v0.1 user opens v1 for the first time and ignores every new
tab / column / keybinding, they still have the v0.1 picker Ñ just
prettier and richer. They can adopt v1's intelligence layer at
their own pace.

The full v0.1 operation surface, preserved verbatim:

### IPC verbs (¤6 Ñ wire-roundtrip operations)

| Verb | What it does | v1 enrichment |
|---|---|---|
| `switch` | Switch to a live or saved workspace | Auto-saves the previous workspace; emits `switch_in` / `switch_out` events into the journal. Existing flow / args / reply shape unchanged. |
| `load` | Load a saved snapshot into the current workspace | Records `load` event with source snapshot + timestamp. Existing flow unchanged. |
| `save` | Save the current workspace to a snapshot | Records `save` event; updates the divergence baseline so the next switch's auto-save gate has a recent reference. Existing Phase A/B/C lock-briefly flow (¤13.4) unchanged. |
| `new` | Create a new workspace (cwd-rooted) | Records `create` event. Optional path through a template (Phase 15 Ñ `T-1403`). Existing flow unchanged. |
| `noop` | Liveness check | Unchanged. |

### Binary-only operations (¤13.12)

| Op | What it does | v1 enrichment |
|---|---|---|
| `rename` | Rename live and/or saved workspace, with collision check (¤13.12.1) | Records `rename` event; events log inherits the new name. Existing collision / overwrite semantics unchanged. |
| `delete` | Delete saved snapshots (single or bulk) (¤13.12.2) | Confirmation modal shows cascade preview (events, notes, sidecar will go too). Existing best-effort batch behaviour preserved. |
| `tag` | Set tags on a workspace's sidecar (¤13.12.3) | Fuzzy autocomplete from existing tags across all workspaces; tag-as-filter built into the picker. Existing wire format unchanged. |
| `pin` | Toggle pin (live OR saved, single source of truth via ¤13.11) (¤13.12.4) | Pinned workspaces always at the top of the picker; pin slots 1Ð9 ? numeric quick-jump keybindings registered alongside `<leader>+W`. Existing storage rules unchanged. |

### TUI affordances (preserved, enriched)

- **`/` filter** Ñ fuzzy filter across visible rows. v1 adds match
  count in the input line and per-row match highlighting.
- **Sort modes** Ñ `live_first` (current default), `alpha`, `recent`,
  `usage` (the ¤13.10 comparators). v1 adds `frecency` (becomes the
  new default) plus tier-bucketed dividers (`?? Active ??` /
  `?? Yesterday ??` / `?? This week ??` / `?? Older ??`). Old sort
  modes still selectable.
- **Marks** (`Tab` / `Space`) Ñ multi-select for bulk operations. v1
  adds a visible marker glyph in the row + selection count in the
  footer.
- **Help** (`?`) Ñ context-aware keybinding help. v1 wires it to
  `bubbles.help` so the toggle actually renders something (in v0.1
  the toggle flips the bool but the View doesn't read it).
- **Quit-mid-op confirm** (¤13.8) Ñ inline `[y/N]` overlay for
  in-flight operations; preserved exactly. v1 only restyles it; the
  contract stays.
- **Preview pane** Ñ preserved, enriched: branch + dirty count,
  foreground process, scrollback peek, notes (markdown), divergence
  badge.

### CLI subcommands (preserved)

- `wezsesh doctor [--format json]` Ñ health checks; v1 surfaces these
  inside the TUI's Doctor tab too. Subcommand contract unchanged.
- `wezsesh list [--format json]` Ñ workspace listing for scripting;
  unchanged.
- `wezsesh find <name>` Ñ locate-and-activate; unchanged. The
  two-phase activation sequence in `internal/find/` is load-bearing
  for editor integrations and stays.
- `wezsesh trust [--rebind / --revoke / --list / --prune / --show / --path / --sidecar]`
  Ñ trust management; v1 surfaces these inside the TUI's Trust tab
  too. CLI surface preserved.
- `wezsesh reset` (and `nuke` alias) Ñ wezsesh-managed dir cleanup;
  unchanged.
- `wezsesh keygen` Ñ HMAC key generation; unchanged.
- `wezsesh --version` Ñ version stamp; unchanged.

### Trust + hook system (preserved)

The ¤13.5 trust-check flow, ¤13.5.1 env-scrub, sidecar `on_create` /
`on_restore` hooks, the `wezsesh trust` CLI surface, the
length-prefixed trust-hash construction Ñ **all preserved exactly,
byte-identical to v0.1.** v1 adds visibility: trust badges per row,
`hook_fire` events in the journal (kind, exit code, truncated
stdout), and the Trust tab as a one-stop approve/revoke surface.
The mechanics underneath do not change.

### Wire protocol (preserved)

The IPC wire path Ñ canonical-JSON byte-equality (¤4.1, ¤17.1),
HMAC field-removal sequence (¤4.3), OSC ² 256 B forward path
(spike #3), Unix-socket reverse path (¤3.4), the ¤3.3 request
envelope shape, the ¤6 verb-keyed shape table Ñ **stable across
v0.1 ? v1.** v1 adds new verbs (`note-edit`, `journal-list`,
`peek`, `search`) but does not redefine, retag, or rewire any
existing one. A v0.1 plugin talking to a v1 binary still works for
every v0.1 verb (subject to the same-major compatibility rule in
`manager.compatible`).

---

The principle: **v1 is v0.1 plus memory.** Anyone who landed on
v0.1 and likes its workspace-management ergonomics will recognize
every keystroke. v1's new primitives (events log, auto-save,
journal, activity heatmap, content search, notes) are layered
*underneath* the existing affordances, not in place of them.

---

## The day-in-the-life test

The right way to evaluate v1 is whether the tool earns its keystroke at
three specific moments in a developer's day.

### 9:00am Ñ "Where was I?"

You open the laptop. resurrect restores something. You press
`<leader>+W`. The picker opens, but instead of a list of names, you see
*your context*:

```
?? Workspaces ?????????????????????????????????????????????????????????????
? ?? Active ????????????????????????????????????????????????????????????? ?
? ?  api-server     feature/auth +2 dirty   cargo watch    3m             ?
? ?  docs           main                    helix          7m             ?
?                                                                         ?
? ?? Yesterday ?????????????????????????????????????????????????????????? ?
? ?  refactor-spike wip/store-cleanup       (paused)      18h             ?
?    onboarding-doc main                    (paused)      22h             ?
?                                                                         ?
? ?? This week ?????????????????????????????????????????????????????????? ?
?    customer-bug-2 hotfix/race                            3d             ?
?                                                                         ?
? ?? Older ?????????????????????????????????????????????????????????????? ?
?    legacy-svc     master                                 31d            ?
???????????????????????????????????????????????????????????????????????????
```

You scan that in 0.5 seconds. You know `api-server` is the daily driver.
You know `refactor-spike` was yesterday and is paused (saved-but-not-live).
You know `customer-bug-2` is from earlier this week. You haven't read a
single name yet Ñ the **layout did the work**.

You hover `api-server`. The preview shows:

```
?? api-server ??????????????????????????????????????????????????????????????
? branch    feature/auth (dirty, +2 commits ahead)                         ?
? process   cargo watch (running 5m)                                       ?
? panes     7 across 3 tabs                                                ?
? saved     3h ago                  live ?  saved ?  divergence: minor     ?
? ???????????????????????????????????????????????????????????????????????  ?
? Last activity (active pane scrollback):                                  ?
?   running 12 tests                                                       ?
?   test auth::handler::bearer_token_empty ... FAILED                      ?
?   test auth::handler::bearer_token_valid ... ok                          ?
?   thread 'auth::handler::bearer_token_empty' panicked at:                ?
?     'expected None, got Some("Bearer ")'                                 ?
? ???????????????????????????????????????????????????????????????????????  ?
? Notes:                                                                   ?
?   TODO: handle empty Bearer token. Started Monday.                       ?
?   - Rejected at parser level: header_value::parse rejects whitespace     ?
?   - Maybe normalize before parse?                                        ?
????????????????????????????????????????????????????????????????????????????
```

**You don't need to switch yet Ñ you already know what you were doing.**
This preview is a complete context recovery in one screen.

### 3:00pm Ñ "Slack ping. Drop everything."

A message arrives: "can you look at customer-bug-2?" You hit
`<leader>+W`. wezsesh sees you're currently in `api-server`. You navigate
to `customer-bug-2`. You hit Enter.

What happens behind the scenes:

1. wezsesh **auto-saves** `api-server` (overwrite-with-confirmation only
   if structurally divergent; silent otherwise).
2. wezsesh writes a **journal event**:
   `15:02:13 ? switched away from api-server (auto-saved, branch=feature/auth+2)`.
3. wezsesh switches to `customer-bug-2`.
4. wezsesh writes another event:
   `15:02:14 ? switched to customer-bug-2 (loaded snapshot, branch=hotfix/race)`.
5. The preview pane on the new workspace shows what *you saw last time
   you were here*: the scrollback peek.

You're now answering Slack with full context, having lost zero work, and
you didn't think about a single thing wezsesh did. **The save/load
ceremony is gone.**

### 6:00pm Ñ "What did I do today?"

You hit `<leader>+W`, then `j` (journal):

```
?? Journal á today ?????????????????????????????????????????????????????????
? 09:02 ?  switched to api-server                                          ?
? 09:18    git commit abc123 "draft bearer-token fix"                      ?
? 09:31    cargo test failed (auth::handler::*)                            ?
? 10:45 ?  switched to docs                                                ?
? 11:02    note added: "publish guide WIP"                                 ?
? 11:30 ?  switched back to api-server                                     ?
? 12:14    cargo test passed                                               ?
? 12:14    git commit def456 "test: empty bearer-token regression"         ?
? 13:00    (laptop closed)                                                 ?
? 14:30    (laptop opened)                                                 ?
? 15:02 ?  switched to customer-bug-2                                      ?
? 15:48    git commit fed987 "race: hold mutex across read+write"          ?
? 16:30 ?  switched to api-server                                          ?
? 17:20    note added: "TODO: handle empty Bearer token"                   ?
?                                                                          ?
? ?? Summary ????????????????????????????????????????????????????????????  ?
? time:    api-server  3h 14m   docs  45m   customer-bug-2  1h 28m         ?
? commits: 3                                                               ?
????????????????????????????????????????????????????????????????????????????
```

A **standup-ready timesheet with no manual journaling**. The data was
free Ñ wezsesh saw every switch, every save, every hook fire.

### Day 14 Ñ "Where was that test that was failing?"

You forgot the failure mode. You only remember it was something with
`auth::handler` in the output. `<leader>+W`, `Ctrl-/` (content search):

```
?? Search á "auth::handler FAILED" ?????????????????????????????????????????
?  api-server   today, 09:31    test auth::handler::bearer_token_e... FAIL ?
?  api-server   day 11, 14:22   test auth::handler::oauth2_flow ... FAILED ?
?  spike-7      day 19, 10:08   test auth::handler::session_expir... FAIL  ?
????????????????????????????????????????????????????????????????????????????
```

You jump to `spike-7` from day 19. wezsesh restores the snapshot from
that day. You're literally looking at the screen you saw 19 days ago,
scrollback intact, with the failing test on it.

This is **time travel for terminal work** Ñ something no other tool can
offer because no other tool combines wezterm's pane-state capture with
resurrect's snapshot persistence.

---

## What this requires architecturally

The vision asks for things that aren't in `docs/design.md` today. Honestly
naming them so v1's spec revision can capture them properly:

### 1. Append-only `events` log per workspace

Right now `state.Store` records a switch count and a last-switch time.
v1 needs a tail-appended event log:

```go
type Event struct {
    At     time.Time
    Kind   string  // "switch_in", "switch_out", "save", "hook_fire",
                   // "git_commit", "process_start", "process_exit",
                   // "note_added", "tag_changed"
    Detail map[string]any
}
```

**Storage:** a per-workspace `events.jsonl` next to the snapshot.
Append-only (cheap), bounded by rotation (last 30 days or 10k lines).
Hooked from:

- The dispatcher (every `switch` / `save` / `delete` verb).
- The Lua handler (every hook fire, with `exit_code` + truncated stdout).
- An optional shell-prompt integration that writes `git_commit` events
  when a commit lands in a tracked workspace cwd. This is the only piece
  needing user setup; everything else is automatic.

### 2. Auto-save on switch with structural-divergence gate

The "do I save?" decision is the worst part of every session manager.
Make it disappear:

- On switch-out, wezsesh inspects the current state vs. the most-recent
  saved snapshot.
- If they're **structurally identical** (same tab/pane count, same cwds,
  no scrollback delta beyond a threshold) ? save silently in background.
  No prompt.
- If they're **structurally divergent** (you closed tabs, opened new
  ones, changed cwds) ? toast `saved api-server (3 ? 5 panes)` so you
  know it happened.
- If you didn't *want* to save (e.g., experimental session you're
  abandoning), `Ctrl-Z` undo-toast within 5 seconds reverts the
  auto-save.

The structural-divergence comparison is a recursive walk over
`snapshots.WorkspaceState`. Cheap. The undo path needs the previous
snapshot retained; rotate to `.prev` instead of overwrite.

### 3. Live process / git inspection

A `wcli.PaneInfo` extension that returns
`{paneID, cwd, foreground_process_name, foreground_process_args}` Ñ
wezterm CLI exposes this. Combined with a per-cwd
`git rev-parse --abbrev-ref HEAD` + `git status --porcelain | wc -l`,
you have everything needed for the live status column.

**Budget:** 50 ms total, parallelised, cached for 30 s. If it doesn't
return in time, the picker renders without the column rather than
waiting.

### 4. Snapshot scrollback indexing for content search

When a workspace is saved with `restore_text = true`, the scrollback is
in the snapshot JSON. To support `Ctrl-/` search, build a tiny inverted
index: per-snapshot, tokenize the scrollback into words, store
`{word ? [snapshot_id, line_number]}` in a sidecar `.idx` file.

SQLite would be the standard answer, but `internal/safefs.AtomicWriteFile`
+ a flat-file index Å 200 lines of code and stays within the existing
single-host invariants.

**Search:** tokenize query, intersect postings, rank by recency. Fast even
at 1000 snapshots.

### 5. Notes (markdown) per workspace

A `notes.md` next to the snapshot. Renders via `glamour` in the preview
pane. Edited via `e` key, which spawns `$EDITOR` against the file
(wezterm `mux.spawn_window` works for this). Notes survive across
sessions, are versioned via the events log, and are searchable.

This is the explicit-journaling layer for thoughts that don't show up in
commits.

---

## TUI design

lazygit-style layout, btop-density, atuin-color-encoding, glamour-
richness. Single screen, multiple panels, all keyboard-navigable:

```
?? Workspaces á 12 ????????????????????????? ?? api-server ?????????????????????????
? ?? Active ?????????????????????????????? ? ? feature/auth +2 dirty               ?
? ?  api-server   feature/auth   cargo  3m ? ? ?live  ?saved  divergence: minor    ?
? ?  docs         main           helix  7m ? ? saved 3h ago                        ?
?                                          ? ? ??????????????????????????????????? ?
? ?? Yesterday ??????????????????????????  ? ? Last activity:                      ?
? ?  refactor     wip            (paused)  ? ?   test auth::bearer_token_empty     ?
?                                          ? ?   ... FAILED                        ?
? ?? This week ??????????????????????????  ? ?   panicked: expected None, got      ?
?    customer-bug hotfix/race    (paused)  ? ?   Some("Bearer ")                   ?
?                                          ? ? ??????????????????????????????????? ?
? ?? Older ??????????????????????????????  ? ? Notes (TODO):                       ?
?    legacy      master          Ñ     31d ? ? - normalize Bearer header at parse  ?
?                                          ? ?   level, then re-test               ?
?                                          ? ? ??????????????????????????????????? ?
? ?? ???????????? activity á 30d           ? ? Trust: ? on_create  ? on_restore    ?
???????????????????????????????????????????? ???????????????????????????????????????
?? Status ?????????????????????????????????????????????????????????????????????????
? today: 3h api-server á 45m docs á 1h customer-bug   |   ? trust  ? resurrect     ?
???????????????????????????????????????????????????????????????????????????????????
[1] Workspaces  [2] Activity  [3] Journal  [4] Trust   /  search   :  command   ?  help
```

Five top-level views (numbered tabs):

| View | What it shows | Why it earns its keystroke |
|---|---|---|
| **1. Workspaces** | the screen above Ñ picker, frecency-tiered, rich preview | The default. 90% of usage. |
| **2. Activity** | 7-row ? 24-col heatmap (workspace ? hour-of-day or ? day-of-month). Cells colored by switch density. | Reveals work patterns. "I always start in `api-server` mornings; `docs` is a Wednesday thing." |
| **3. Journal** | the 6pm timeline view Ñ chronological event stream with grouping | Standup. Time-tracking. "What did I do?" |
| **4. Trust** | every workspace's hooks, fingerprints, last-fired status, output | Security visibility. Approve/revoke without leaving the TUI. |
| **5. Doctor** | `wezsesh doctor` rendered live, each check pass/fail-badged, click-to-expand | Self-healing surface. When something's broken, this is where you go. |

Across all views: `/` filters in-view, `Ctrl-/` searches *content*
(snapshot scrollback) globally, `:` opens command palette, `?` is
contextual help.

---

## Charm library mapping

Concrete component-to-feature mapping. Everything except the last two
rows is already pinned in `go.mod`:

| Charm | Used for |
|---|---|
| `lipgloss.NewStyle().Border(RoundedBorder()).BorderTitle(...)` | Every panel border + title |
| `lipgloss.JoinVertical / JoinHorizontal` | Three-region layout (workspaces, preview, status) |
| `lipgloss.Table` | Picker rows (column-aligned, styled) |
| `lipgloss.Tree` | Pane hierarchy in preview |
| `bubbles.list` (custom delegate) | The picker Ñ replaces hand-rolled `visibleRows`/`cursor` |
| `bubbles.viewport` | Preview pane (scrollable), Journal view, Activity heatmap |
| `bubbles.textinput` | Filter input, search query, palette command, save-as / rename / tag prompts |
| `bubbles.help` + `bubbles.key` | Footer hints (replaces hand-rolled `[enter=switch É]` strings); auto-context-aware |
| `bubbles.spinner` | In-flight op indicator (per-row, not in footer) |
| `bubbles.progress` | Save Phase A/B/C bar |
| `huh` | Multi-step modals: new-from-template, delete-with-cascade-confirm, trust-approval, tag-editor |
| `charmbracelet/glamour` *(net-new)* | Markdown notes rendering in preview |
| `charmbracelet/harmonica` *(net-new)* | Cursor-move easing, panel-focus transitions, post-action "pop" feedback |
| Custom (built on `lipgloss`) | Sparklines, heatmap cells, frecency dividers, journal time-grouping |

Adding `glamour` and `harmonica` gets the polish ceiling. Everything else
is already paid for.

---

## The compounding effect

These features aren't a list Ñ they **compound**:

- Auto-save on switch makes the journal *complete* (no gaps from
  forgotten saves).
- Per-row process column gives the journal *richness* (you see what was
  running, not just that you switched).
- Notes in markdown make the journal *intentional* (your thoughts
  captured, not just your switches).
- Content search makes the journal *queryable* (retroactive memory).

Each feature alone is "nice-to-have." Together, they make wezsesh **the
layer of your terminal that has memory**.

Nobody else has that. tmux-sessionizer is amnesia-by-design. zellij
sessions are a data type, not a journal. resurrect is a save/restore
tool, not a memory layer. Even VSCode's "recent workspaces" is just a
list Ñ no events, no time, no context.

This is wezsesh's category. Not a fancier picker. **Work intelligence.**

---

## Build plan

Four phases. Not Phase 10 / 11 Ñ those are v0.1 finishing work. These
are v1 phases.

### Phase 12 Ñ work-intelligence foundation (Å 2 weeks)

The data layer. None of the v1 TUI features can land without these.

| Task | What |
|---|---|
| T-1100 | Events log on `state.Store` (append-only, rotated `events.jsonl`) |
| T-1101 | Dispatcher hooks events on switch / save / delete |
| T-1102 | Lua handler hooks events on hook fire (with truncated stdout) |
| T-1103 | Per-row live process + git column (cached, parallel, 50 ms budget) |
| T-1104 | Scrollback peek in preview (via `wezterm cli get-text`) |
| T-1105 | `notes.md` sidecar + `e` key + glamour render |

**Spec capture (T-DOC):** events schema, divergence semantics, peek
budget, notes file layout. These all need new sections in
`docs/design.md` before code lands; queue T-DOC entries against each.

### Phase 13 Ñ TUI re-architecture (Å 2 weeks)

The UI scaffolding. Rebuilds the picker on top of `bubbles.list`,
introduces tabbed views, replaces the hand-rolled footer.

| Task | What |
|---|---|
| T-1200 | `bubbles.list` migration for the picker |
| T-1201 | `bubbles.viewport` for the preview |
| T-1202 | Panel layout with `lipgloss.Border` + status line |
| T-1203 | `bubbles.help` + `bubbles.key` integration (replaces footer) |
| T-1204 | Tabbed views Ñ Workspaces, Activity, Journal, Trust, Doctor |

### Phase 14 Ñ the killers (Å 3 weeks)

The features that distinguish wezsesh from any session manager that
currently exists. Each is a real engineering project.

| Task | What |
|---|---|
| T-1300 | Auto-save on switch with structural-divergence gate + Ctrl-Z undo |
| T-1301 | Journal viewport with time grouping + summary |
| T-1302 | Activity heatmap (workspace ? hour-of-day, workspace ? day-of-month) |
| T-1303 | Scrollback content index + `Ctrl-/` search |

### Phase 15 Ñ polish (ongoing)

Aesthetic and ergonomic. Lands incrementally; never blocks the
preceding phases.

| Task | What |
|---|---|
| T-1400 | `harmonica` animations (cursor easing, panel-focus transitions) |
| T-1401 | Theme presets + auto-detect from `wezterm.color_scheme` |
| T-1402 | Command palette `:` |
| T-1403 | Workspace templates / new-from-template wizard |

**Total:** roughly 6Ð8 weeks of focused implementation alone.

---

## Distinctions that matter

What v1 wezsesh is NOT, deliberately:

- **Not a tmux replacement.** It sits on top of wezterm; it doesn't
  multiplex.
- **Not an IDE workspace manager.** VSCode workspaces edit files;
  wezsesh's workspaces are terminal sessions. Different category.
- **Not a remote session tool.** No `wish`, no SSH server. Single-host
  trust model is part of the threat model in `docs/design.md` and stays.
- **Not networked / synced.** `atuin` owns that space. Single-machine
  journal is simpler, faster, more secure.
- **Not opinionated about your editor.** Notes are plain markdown;
  scrollback peek is editor-agnostic; hooks run *anything*.
- **Not a logging product.** Events log is for the user's own memory,
  not for telemetry, not for debugging, not for export.

---

## Migration from v0.1

When the current `PROJECT.md` ledger lands its final task and v0.1
ships:

1. Tag `v0.1.0` (Phase 11's release workflow takes it from there).
2. Delete `PROJECT.md`.
3. Author `docs/design.md` revision Ñ fold in the events / divergence /
   peek / notes / index / search architecture; bump the schema versions
   on `state.json` and the snapshot sidecar to `2`.
4. Author `docs/prd.md` revision Ñ fold in the journal thesis, the
   day-in-the-life vision, the new tabbed UX.
5. Generate a fresh `PROJECT.md` from this V1.md plus the revised spec.
   The new ledger's Phase 12Ð15 maps 1:1 to the build plan above.
6. Pick T-1100. Run `/next-task`.

The `state.json` and snapshot-sidecar schemas are bumped at exactly one
moment: when the events log lands. The ¤10.6 `wezterm.GLOBAL`
schema-version stamp the plugin already enforces will catch
mid-migration users; v1 stamps `wezsesh_plugin_version = "1.0.0"` and
the cross-version wipe handles cleanup.

The IPC wire protocol (`docs/design.md` ¤3.x) does **not** change. The
`canonicaljson` byte-equality contract, the HMAC framing, the OSC ² 256 B
ceiling, the Unix-socket reverse path Ñ all stable. v1 adds new verbs
(`note-edit`, `journal-list`, `peek`, `search`) but does not break the
existing ones.

The trust system, the hook execution environment, the ¤13.5.1 env-scrub
set, and the ¤17.4 lualint contract are all preserved. v1 is additive;
v0.1 deployments survive their migration cleanly.

---

## Why this is the right next direction

Three reasons, in order:

1. **It's the only direction where wezsesh has a moat.** Frecency sort,
   colored markers, fancier panels Ñ every other session manager will
   eventually have these. The journal-shaped wezsesh is *uniquely
   possible* because of the wezterm + resurrect + trust triple-vision.
2. **The data is already free.** Every architectural piece Ñ switch
   counts, snapshot states, hook outcomes, pane scrollback Ñ already
   exists or is one CLI call away. v1 mostly *surfaces* data wezsesh
   already has access to.
3. **It compounds.** Each Phase 12 / 13 / 14 task makes the others
   stronger. Auto-save makes the journal complete; the journal makes
   activity-heatmap meaningful; activity-heatmap makes content-search
   discoverable; content-search makes notes valuable. That's not a
   feature list Ñ it's a product.

Build it.