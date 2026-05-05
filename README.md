# wezsesh

A workspace/session manager for [WezTerm](https://wezterm.org). Terminal-native
TUI plus a thin Lua plugin that brings tmux-sessionizer-style ergonomics to
wezterm workspaces.

WezTerm has no first-class session manager today. `smart_workspace_switcher`
covers fuzzy picking; `resurrect.wezterm` covers persistence. Tmux users have
`sesh` and `tmux-sessionizer` to unify those. WezTerm has the gap. wezsesh
fills it: one keybind opens a TUI listing every workspace — live in the mux
**and** saved on disk — with switch / load / rename / delete / save / new /
pin / tag / bulk-delete and a live preview pane. Sub-200ms cold start,
keyboard-only.

```
┌─ wezsesh ──────────────────────────────────────────────────────┬─ preview ──────────────┐
│ filter: ▌                                                      │ ~/code/foo             │
├────────────────────────────────────────────────────────────────┤  Tab 1: nvim            │
│ ▶ ~/.dotfiles             live    3 tabs    2m ago    [pinned] │   └ pane: ~/code/foo   │
│ ● ~/code/foo              live    5 tabs    1h ago    #api     │  Tab 2: pgcli           │
│   ~/code/bar              saved   3d ago              #db      │   └ pane: ~/code/foo   │
│   default                 saved   1w ago                       │                        │
│   ~/code/scratch          live  (unsaved)              --      │ Last used: 1h ago      │
│ ✓ ~/code/old              saved   42d ago             #archive │ Tags: api              │
├────────────────────────────────────────────────────────────────┴────────────────────────┤
│ s switch  l load  r rename  d delete  S save  n new  p pin  t tag  Tab mark  ? help     │
└─────────────────────────────────────────────────────────────────────────────────────────┘
```

The layout above is illustrative; actual columns and marker glyphs follow
the §11 `columns` defaults plus your `markers.*` overrides.

## Quick start

Install the binary (see [`docs/install.md`](docs/install.md) for all four
paths — Homebrew, curl, Nix, source), then in your `wezterm.lua`:

```lua
-- The plugin URL is the GitHub HTTPS URL — wezterm.plugin.require's contract.
local resurrect = wezterm.plugin.require(
    "https://github.com/MLFlexer/resurrect.wezterm")
local wezsesh = wezterm.plugin.require(
    "https://github.com/Grady-Saccullo/wezsesh")
wezsesh.apply_to_config(config, {
    snapshot_dir = "/path/to/snapshots",
    resurrect = resurrect,
})
```

Then reload wezterm and hit the default keybinding (`LEADER` + `SHIFT+W`) to
open the TUI. The keybinding is configurable via `opts.keybinding` — see §11.

## Documentation

- **Install:** [`docs/install.md`](docs/install.md) — Homebrew tap, curl
  installer, Nix flake, and `go install` paths, plus `wezsesh doctor` smoke
  checks.
- **Configuration:** the full `apply_to_config(config, opts)` schema lives in
  [`docs/design.md`](docs/design.md) §11 — every option, default, and
  validation rule.
- **Hooks.** Per-workspace `on_create` / `on_restore` shell hooks are gated
  by a trust file — see [`docs/design.md`](docs/design.md) §13.5 (hook
  trust check). The `on_before_op` / `on_after_op` callback opts surrounding
  the dispatch lifecycle are listed in §11; the save-flow state machine
  itself lives in §13.4 (lock-briefly + in-process serialisation).
- **Design rationale:** [`docs/prd.md`](docs/prd.md) for the product
  positioning; [`docs/design.md`](docs/design.md) for the normative technical
  spec.

## Status

Pre-release. Iteration backlog and progress live in
[`PROJECT.md`](PROJECT.md).
