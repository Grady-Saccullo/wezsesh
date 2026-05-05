# AI proposal — recommendations for LLM / embedding integration

> **Status:** proposal. Not picked up. This document captures a
> recommended shape for AI integration in wezsesh — what to build, what
> to deliberately not build, and the architectural constraints that the
> threat model imposes. Implementation is deferred until the build
> ledger explicitly nominates it.

---

## The test

Most "AI in tools" features fall into one of three buckets:

1. **Gimmick** — uses an LLM where a regex or classical heuristic would
   be faster, deterministic, and sufficient.
2. **Lossy convenience** — works 80% of the time and frustrates the
   other 20%, where the deterministic alternative would always work.
3. **Real value** — does something a deterministic algorithm genuinely
   cannot: semantic understanding, fuzzy synthesis across many sources,
   language-shaped output from data-shaped input.

A senior engineer's tolerance for AI features in their daily-driver
tools is roughly zero patience for #1 or #2 — they'll turn it off after
three days. #3 features they'll reach for unprompted and miss when
they're gone.

The recommendations below are stack-ranked by which bucket they sit in.
Anything below the line is rejected with reasoning.

---

## What AI can actually work with

These are the data sources AI features in wezsesh would compound on. Some
exist in the codebase today; others would need to be added before AI
features could land:

- **Snapshot scrollback** — unstructured terminal text, error messages,
  REPL sessions, build output, log lines. Captured by resurrect when
  `restore_text = true`.
- **Event stream** — structured, time-ordered, labeled (switch / save /
  commit / hook_fire). Would need a per-workspace append-only log.
- **Notes** — user-authored markdown per workspace. Would need a
  `notes.md` sidecar surface.
- **Cross-workspace activity** — same dev, multiple projects, time-
  correlated. Falls out of the event stream once it exists.
- **Hook outputs** — build/test results captured at workspace transitions.
  Already feasible; would need wiring through the hook execution surface.

That combination — structured events surrounded by unstructured text,
all owned by one user with one local context — is the data shape where
AI earns its keep, *if* the features are chosen carefully.

---

## What to ship

Six features, each one passing the "deterministic alternative would not
suffice" test. Stack-ranked by value-per-effort.

### 1. Semantic content search across scrollback (highest value)

Lexical content search (string-match across saved scrollback) is
necessary but insufficient: developers forget exact strings, they
remember concepts. A semantic search opens a parallel keystroke (e.g.
`Ctrl-Shift-/`) for natural-language queries:

```
╭─ Semantic search · "oauth flow refactor" ─────────────────────────────────╮
│  api-server        day 3, 14:22   bearer-token regression, oauth2_flow ↗  │
│  spike-7           day 19, 10:08  experimenting with PKCE flow ↗          │
│  customer-bug-2    today, 15:48   race condition in oauth2 callback ↗     │
╰───────────────────────────────────────────────────────────────────────────╯
```

What makes this AI-shaped, not regex-shaped: "OAuth thing" matches text
containing `oauth2`, `OIDC`, `bearer token`, `auth provider`, `JWT
validation` — terms that share semantic space, not lexical space.
Embeddings handle this trivially; regex never can.

**Architecture:** local sentence-transformer model (`all-MiniLM-L6-v2`-
class, ~80 MB), embed scrollback in 200-token windowed chunks at save
time, store the vector index in a sidecar. Query: embed the query,
k-NN search across all snapshots, rank by recency-weighted similarity.
~50 ms response at 10 k snapshots. Zero network calls. Zero leakage.

**Why this is high value:** retrieval is the single most important
thing AI does well that classical tools cannot. And wezsesh has the
data to feed it. This alone justifies the whole AI integration.

### 2. Workspace resumption summary

When the user hovers a workspace in the picker, a 1–3 sentence AI-
generated summary of "what was happening here last time" appears
alongside the raw scrollback peek:

```
╭─ api-server ─────────────────────────────────────────────────────────────╮
│ branch    feature/auth (dirty, +2)                                       │
│ process   cargo watch (5m)                                               │
│ saved     3h ago                                                         │
│ ─────────────────────────────────────────────────────────────────────── │
│ ✨ Last session: debugging auth::handler::bearer_token_empty regression │
│    Test was failing with 'expected None, got Some("Bearer ")'. You     │
│    added a TODO note about normalizing the header before parse.        │
│ ─────────────────────────────────────────────────────────────────────── │
│ Last activity (active pane scrollback):                                  │
│   running 12 tests                                                       │
│   test auth::handler::bearer_token_empty ... FAILED                      │
│   ...                                                                    │
╰──────────────────────────────────────────────────────────────────────────╯
```

The summary is generated *once* on save, against the snapshot's
scrollback + recent events + notes, cached in the sidecar. Refreshed
only when notes change or a new save happens. Cost: ~200 output tokens
per workspace per save.

**Why this is high value:** the user already sees the raw scrollback
peek. The AI summary doesn't *replace* that; it *labels* it. One
sentence converts "I see the bytes" into "I remember the situation"
— which is the actual goal of a memory-shaped session manager.

The "✨" prefix is non-negotiable: every AI-generated piece of text in
the TUI gets visually marked. Users always know what came from disk
and what came from a model.

### 3. Daily activity synthesis

The event stream contains every switch, save, commit, hook fire, and
process transition of the day. AI turns that ledger into a paragraph:

```
╭─ Today, in summary ──────────────────────────────────────────────────────╮
│ ✨ Spent 3.5h on api-server (closed the bearer-token regression with    │
│    2 commits, tests passing). Briefly hopped to docs (~45m) to start    │
│    the publish guide. Took the customer-bug-2 ping in the afternoon —   │
│    ran 1.5h, landed a mutex fix for the oauth2 race.                    │
╰──────────────────────────────────────────────────────────────────────────╯
```

Surfaces as a header line above the day's event log, or via a palette
command (e.g. `:standup`) that copies to clipboard for paste into a
Slack standup channel.

**Why this is high value:** standup prep is a daily friction point for
most devs. The data needed is already in the event stream; converting
it to a sentence-paragraph is a near-ideal LLM task (structured input,
language output). One generation per day, ~500 output tokens.

### 4. Friction / anomaly detection

Pattern detection is mostly *classical* (count-based heuristics):
"you've switched between api-server and customer-bug 8 times in the
last hour" or "you've run `cargo test` 12 times in the last 90 minutes,
all failing on the same line." The detection itself is deterministic.

The AI piece is *characterization*:

```
✨ Looks like you've been stuck on auth::handler::bearer_token_empty for
   90 minutes — 12 test runs, all failing the same way. Worth pairing
   with someone, or stepping away for 10 minutes? (dismiss · :friction
   for details)
```

Quiet by default. Surfaces only when patterns cross thresholds. Always
dismissible. Never blocks input.

**Why this is medium-high value:** the pattern detection is helpful
even without AI. The AI characterization makes it land — "stuck on
auth_handler" reads differently from "12 test failures detected,
threshold exceeded." Important: this can be worse than the friction
itself if the model is preachy or wrong, so the bar for shipping is
high.

### 5. Cross-workspace error correlation

When a search query (semantic or lexical) returns results that contain
error stacktraces, surface a *related errors* panel:

```
╭─ Related ─────────────────────────────────────────────────────────────────╮
│ ✨ Same error appeared in `spike-7` 19 days ago, fixed in commit         │
│    fed987 ("normalize Bearer at parser level"). Same fix may apply.      │
╰───────────────────────────────────────────────────────────────────────────╯
```

This falls out of the semantic-search infrastructure (#1) for free at
the `Detail` level: embed each error stacktrace, k-NN, look at neighbors
that have a `git_commit` event in the same workspace shortly after the
error.

**Why this is medium value:** mostly helpful for long-lived projects
with recurring error classes. Less useful for greenfield work.

### 6. Workspace-purpose summary for old/forgotten work

Hover an old workspace. AI reads sparse data (notes, last 5 commits,
last session's scrollback) and generates one sentence: *"PHP 5.6
invoice export service. Last touched 3 months ago. Notes mention
'migrate to v2' but no commits toward it."*

Helpful during cleanup rituals — answers "what was this for?" without
forcing a context-switch. Generated lazily on first hover, cached.

**Why this is low-medium value:** niche but high-leverage when you need
it. Reuses the resumption-summary infrastructure (#2), so basically free
once #2 ships.

---

## What to deliberately NOT ship

Each of these is a reflexive "AI feature" idea that should be rejected.
Reasoning matters more than the list, so the *why* is included for each:

- **Auto-generated commit messages.** Devs want their own voice. Cargo
  cult.
- **Auto-generated notes.** The value of notes is intentional thought,
  not auto-fill. AI assistance *while* writing notes (suggestions in a
  sidebar) is OK; AI-authoring is not.
- **Auto-generated workspace names.** `cwd` basename is fine; AI naming
  is friction with no upside.
- **Auto-suggested hook commands** — *"I see this is a Rust project;
  want me to set `on_restore = cargo build`?"* The trust system exists
  *precisely* to keep arbitrary code suggestions from running. Auto-
  suggestion is a security regression dressed as convenience.
- **Conversational chat interface** — *"ask wezsesh."* Slower than `/`,
  less discoverable than tabs, harder to keyboard-navigate. The picker
  + journal + search are already denser than any chat could be.
- **Generative debugging help** — *"paste this stacktrace, get a fix."*
  Out of scope. ChatGPT / Claude / Cursor exist; wezsesh shouldn't try
  to be one. Stay in our lane.
- **Auto-pruning suggestions with one-click delete.** AI false positives
  + irreversible action = catastrophic. User reads the cleanup summary,
  user clicks delete.
- **Telemetry / "improve our AI."** Single-host trust model. No phone-
  home, ever.
- **Pre-warm context summary on every switch.** Adds latency to the
  load-bearing UX (switching is the most-used action). Summaries
  generate at *save* time, not *switch* time.

---

## Architecture decisions that are non-negotiable

The threat model and the developer-tool-trust contract pin these. They
aren't optional design choices.

### Local-first, cloud opt-in only

Wezsesh's threat model is *single-user host, sensitive content*.
Scrollbacks contain SSH sessions, env vars with secrets, proprietary
code, internal URLs, customer data. **A wezsesh that auto-sends this
to a third-party LLM is dead on arrival.**

Therefore:

- **Embeddings: local always.** Sentence-transformer-class model
  bundled or downloaded on first use. Runs on CPU; ~80 MB; deterministic.
- **Generation: local default (e.g. Ollama with a 7–13 B model), cloud
  opt-in.** Cloud (Anthropic / OpenAI) can be enabled by explicit env
  var + config flag, with a clear "this sends data to <provider>"
  disclosure.
- **No "automatic" cloud calls ever.** Every cloud call is either
  user-initiated (e.g. `:standup --upstream`) or per-event opted-in via
  config.

### Secret scrubbing before any input

Before scrollback / events / notes hit any AI provider (local or cloud),
they pass through a scrubber:

- Env-var-shaped lines: any `*KEY*` / `*TOKEN*` / `*SECRET*` /
  `*PASSWORD*` / `*BEARER*` value redacted.
- Common credential patterns: AWS keys, GitHub tokens, JWT tokens, SSH
  private keys.
- The existing hook-env-scrub set is a starting point; expand from
  there.

This applies to *local* models too — if the local LLM gets compromised
by a future bug or model swap, the data was never there to leak.

### Per-workspace and per-content opt-out

- A `.wezsesh-no-ai` marker file in a workspace cwd disables all AI
  processing of that workspace's content. For client work, NDAs,
  security-sensitive projects.
- A wezsesh config stanza allows global opt-out (`ai.enabled = false`).
  wezsesh stays fully functional without AI; the non-AI features are
  the product floor.

### Provenance discipline

Every AI-generated string in the TUI:

- Visible `✨` glyph prefix (or accent color). Never blends with
  deterministic content.
- Hover/focus shows "based on: <source events / chunks>" — the user can
  audit the inputs.
- Every summary cached with the inputs that produced it. Reproducible
  and auditable.

### Cost visibility

A `wezsesh ai status` subcommand:

```
provider        ollama (local)
model           llama3.1:8b
calls today     14
calls (30d)     342
estimated cost  $0.00 (local)
last error      none
```

For cloud users, real token spend in $. Surfaces when costs creep.

---

## Implementation outline

Ordered for value compounding. Each task is one PR-sized scope.

| Order | Task | What |
|---|---|---|
| 1 | Embeddings infra | Local model bundling, vector index sidecar layout, indexer that consumes saved snapshots and writes vectors. Foundational. |
| 2 | Semantic content search | UI surface (`Ctrl-Shift-/`), query-time embedding, k-NN search, recency-weighted ranking. The first user-visible win. |
| 3 | AI provider abstraction | Config layer for local (Ollama) vs cloud (Anthropic / OpenAI), explicit consent flow for cloud, never-auto-cloud invariant. |
| 4 | Secret-scrubbing pipeline | Regex + entropy-detector for token-shaped strings. Runs on every input to any model. Tested against a corpus of real-world secret shapes. |
| 5 | Resumption summary | Cached per-snapshot summary, regenerated on save / notes-change. Surfaces in preview pane with `✨` provenance marker. |
| 6 | Daily activity synthesis | One generation per day from event stream. `:standup` palette command + clipboard copy. |
| 7 | Friction detection | Classical heuristics (switch frequency, repeated failures, no-commit windows) + AI characterization on threshold crossing. Always dismissible. |
| 8 | Cross-workspace error correlation | Leverages embeddings infra. Surfaces in search results when stacktraces appear. |
| 9 | Provenance + labelling discipline | `✨` glyph, "based on:" hover, label-everywhere lint. Lands alongside or before any user-visible AI feature. |
| 10 | `wezsesh ai status` subcommand | Token spend, last call, provider health. Required before cloud opt-in is shipped. |

Tasks 1 + 2 alone (semantic search) earn the integration; everything
else compounds on the same infrastructure.

---

## The thesis

The features above all share a pattern: AI doing things the *data*
should do — synthesis, retrieval, characterization. The features in
the rejected list all share the inverse pattern: AI doing things the
*user* should do — writing, naming, suggesting code.

That's the line. Stay on the right side of it and AI integration earns
its place. Cross it and wezsesh becomes another tool with an AI gimmick
that everyone disables in week two.

The recommendation is to build the right side, not the left.
