# Installing wezsesh

`wezsesh` is the Go binary that backs the Lua plugin. The plugin's
default `manager.resolve_binary` (`opts.binary == nil`) calls bare
`"wezsesh"` and relies on the binary being on the user's `PATH`. Any of
the install paths below satisfies that contract.

This document covers four installation paths, in order of recommended
ergonomics: (1) Homebrew tap (easiest on darwin), (2) the `curl | sh`
installer (fastest cross-platform), (3) the Nix flake, and (4)
`go install` from source.

## Homebrew

### End-user install

```bash
brew tap grady-saccullo/wezsesh
brew install wezsesh
wezsesh --version    # prints the installed tag, e.g. "wezsesh v0.1.0"
```

`brew tap grady-saccullo/wezsesh` resolves to the tap repository
`github.com/grady-saccullo/homebrew-wezsesh` (Homebrew's
`<owner>/homebrew-<name>` convention). The Formula downloads the
matching tarball from the main `wezsesh` repository's GitHub Releases
page (produced by `.github/workflows/release.yml`, see
[`docs/release.md`](release.md)), verifies its sha256, and installs the
extracted `wezsesh` binary into the standard Homebrew prefix
(`$(brew --prefix)/bin/wezsesh`).

> **A note on capitalization.** GitHub usernames are case-insensitive
> in URLs, so `grady-saccullo` and `Grady-Saccullo` resolve to the
> same account. The lower-case form is used throughout the Homebrew
> tap (per Homebrew convention for tap repository names); the
> mixed-case `Grady-Saccullo` is preserved for upstream URLs (the
> Go module path at `github.com/Grady-Saccullo/wezsesh` is the
> canonical source-of-truth casing).

Supported platforms in the Formula:

| Platform        | Tarball                                     |
|-----------------|---------------------------------------------|
| `darwin/arm64`  | `wezsesh_${TAG}_darwin_arm64.tar.gz`        |
| `darwin/amd64`  | `wezsesh_${TAG}_darwin_amd64.tar.gz`        |
| `linux/arm64`   | `wezsesh_${TAG}_linux_arm64.tar.gz`         |
| `linux/amd64`   | `wezsesh_${TAG}_linux_amd64.tar.gz`         |

Linux support targets [Linuxbrew / Homebrew on Linux][linuxbrew]; native
Linux package managers (apt, dnf, AUR, etc.) are out of scope for the
tap and will be addressed by separate distribution channels.

[linuxbrew]: https://docs.brew.sh/Homebrew-on-Linux

### Operator: one-time tap repository setup

The tap lives in a sibling repository, **not** in this repo. Homebrew
discovers it via the `homebrew-` prefix in the repo name.

1. Create a public GitHub repository named exactly
   `grady-saccullo/homebrew-wezsesh` (the lower-case form;
   `brew tap` is case-insensitive but the convention is lower-case).
2. Add a single file at `Formula/wezsesh.rb` with the contents of the
   [Formula template](#formula-template) below.
3. Commit and push to `main`. No CI is required on the tap repo —
   `brew install` reads the file directly from `main`.
4. Verify locally:
   ```bash
   brew tap grady-saccullo/wezsesh
   brew install wezsesh
   wezsesh --version
   brew uninstall wezsesh
   brew untap grady-saccullo/wezsesh
   ```

   `brew install` runs the Formula's `sha256` check automatically.
   Optionally, verify the published `SHA256SUMS` end-to-end against
   the upstream tarballs on the same machine before populating the
   tap (this exercises the same checksum that ends up in the
   Formula):
   ```bash
   TAG=v0.1.0   # set to the tag under verification
   curl -fSL -o SHA256SUMS \
     "https://github.com/Grady-Saccullo/wezsesh/releases/download/${TAG}/SHA256SUMS"
   curl -fSL -O \
     "https://github.com/Grady-Saccullo/wezsesh/releases/download/${TAG}/wezsesh_${TAG}_$(uname | tr '[:upper:]' '[:lower:]')_$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/').tar.gz"
   sha256sum -c SHA256SUMS --ignore-missing   # macOS: shasum -a 256 -c
   ```

Step (4) requires that a real release tag has already been published
(see [`docs/release.md`](release.md)) — the Formula's `url` 404s
otherwise.

### Operator: per-release bump

After each tagged release of `Grady-Saccullo/wezsesh`:

1. From the GitHub Release page for the new tag, download
   `SHA256SUMS`. It contains four lines of the form
   `<sha256>  wezsesh_${TAG}_<os>_<arch>.tar.gz`.
2. In the tap repo, edit `Formula/wezsesh.rb`:
   - Update `version "X.Y.Z"` to the new tag string with the leading
     `v` stripped (e.g. `version "0.1.0"` for tag `v0.1.0`).
   - Update each of the four `sha256 "..."` values to match the
     corresponding line from `SHA256SUMS`.
3. Commit with a message like `wezsesh 0.1.0` and push to `main`.

Existing users pick up the bump on their next `brew update && brew
upgrade wezsesh`.

### Formula template

Copy the block below verbatim into `Formula/wezsesh.rb` in the tap
repo. The placeholder `version "0.0.0"` and `<fill at release time>`
sha values must be replaced before the first publish per the
[per-release bump](#operator-per-release-bump) procedure.

```ruby
# Formula/wezsesh.rb — Homebrew tap for Grady-Saccullo/wezsesh.
# See https://github.com/Grady-Saccullo/wezsesh/blob/main/docs/install.md
# and https://github.com/Grady-Saccullo/wezsesh/blob/main/docs/release.md
class Wezsesh < Formula
  desc "Wezterm session manager TUI (sits between smart_workspace_switcher and resurrect)"
  homepage "https://github.com/Grady-Saccullo/wezsesh"
  version "0.0.0"
  # `license :unknown` until a LICENSE file lands at the repo root.
  # See docs/release.md operator pre-flight; once LICENSE is present
  # this should become `license "MIT"` (or whichever SPDX id matches
  # the file's contents). `brew audit --strict` flags a mismatch
  # between this stanza and the actual project license, so resist
  # tightening it here ahead of the source-of-truth file.
  license :unknown

  # URLs are inlined per platform rather than templated through a
  # class-body local variable: `on_macos` / `on_linux` blocks are
  # evaluated under Homebrew's OnSystem DSL via `instance_eval`, which
  # rebinds `self` and severs the lexical scope a `base_url = "..."`
  # would otherwise live in. (The `version` DSL method is visible
  # inside the blocks because it is a method on the Formula class,
  # not a local variable.) Per-release bumps therefore touch four
  # `sha256 "..."` lines alongside the single `version` line; the
  # URL strings themselves do not change between releases.
  #
  # The release workflow at .github/workflows/release.yml names its
  # tarballs `wezsesh_${TAG}_${os}_${arch}.tar.gz`. For stable tags
  # `${TAG}` equals `v${version}` (this Formula's invariant);
  # pre-release tags carry suffixes (`-rc1`, `-beta.4`) that the
  # tap intentionally does not track — Homebrew users get stable
  # releases only.

  on_macos do
    on_arm do
      url "https://github.com/Grady-Saccullo/wezsesh/releases/download/v#{version}/wezsesh_v#{version}_darwin_arm64.tar.gz"
      sha256 "<fill at release time: darwin_arm64>"
    end
    on_intel do
      url "https://github.com/Grady-Saccullo/wezsesh/releases/download/v#{version}/wezsesh_v#{version}_darwin_amd64.tar.gz"
      sha256 "<fill at release time: darwin_amd64>"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/Grady-Saccullo/wezsesh/releases/download/v#{version}/wezsesh_v#{version}_linux_arm64.tar.gz"
      sha256 "<fill at release time: linux_arm64>"
    end
    on_intel do
      url "https://github.com/Grady-Saccullo/wezsesh/releases/download/v#{version}/wezsesh_v#{version}_linux_amd64.tar.gz"
      sha256 "<fill at release time: linux_amd64>"
    end
  end

  def install
    # The release tarball extracts into a directory named
    # wezsesh_v${version}_${os}_${arch}/ containing `wezsesh` plus
    # LICENSE and README.md. Homebrew untars us into that directory's
    # contents, so the binary is at ./wezsesh.
    bin.install "wezsesh"
  end

  test do
    # Smoke check: --version must print the tag string the formula
    # was built against. The release workflow injects the tag into
    # the binary via -X main.version=${TAG}.
    assert_match "v#{version}", shell_output("#{bin}/wezsesh --version")
  end
end
```

### Notes on acceptance gates

Two of T-1001's acceptance gates depend on artefacts that live outside
this repository and therefore cannot be auto-verified by an in-tree
check:

- **"Tap repo `grady-saccullo/homebrew-wezsesh` exists with a
  Formula…"** — the tap repository is a sibling GitHub repo; creating
  it is a user-driven action (see
  [Operator: one-time tap repository setup](#operator-one-time-tap-repository-setup)).
- **"`brew tap grady-saccullo/wezsesh && brew install wezsesh`
  succeeds on darwin/arm64 and darwin/amd64"** — runtime gate that
  passes once both (a) the tap repo exists with the Formula above and
  (b) a real release tag has been published, populating the GitHub
  Release page that the Formula's `url` points at.

The in-tree deliverable for T-1001 is the Formula content itself
(above) plus this install documentation. The two external gates
become mechanical once those preconditions are met.

## Curl install

For shells without Homebrew, the `install.sh` script at the repo root
fetches the matching release tarball, verifies its sha256 against the
published `SHA256SUMS`, and drops the `wezsesh` binary into your
install directory.

```bash
curl -fsSL https://raw.githubusercontent.com/Grady-Saccullo/wezsesh/main/install.sh | sh
```

The script detects your platform (`uname -s` / `uname -m`), maps it to
one of the four supported targets (`{linux,darwin}_{amd64,arm64}`), and
installs to `${WEZSESH_INSTALL_DIR:-$HOME/.local/bin}/wezsesh`.

### Environment overrides

| Variable               | Default              | Effect                                                           |
|------------------------|----------------------|------------------------------------------------------------------|
| `WEZSESH_VERSION`      | (latest stable tag)  | Pin a specific tag, e.g. `v0.1.0`. Leading `v` is auto-prepended.|
| `WEZSESH_INSTALL_DIR`  | `$HOME/.local/bin`   | Destination directory. Created with `mkdir -p` if missing.       |

Examples:

```bash
# Pin to v0.1.0:
curl -fsSL https://raw.githubusercontent.com/Grady-Saccullo/wezsesh/main/install.sh \
  | WEZSESH_VERSION=v0.1.0 sh

# Install into /usr/local/bin (may require sudo depending on perms):
curl -fsSL https://raw.githubusercontent.com/Grady-Saccullo/wezsesh/main/install.sh \
  | WEZSESH_INSTALL_DIR=/usr/local/bin sh
```

### Pre-release tags

`WEZSESH_VERSION` is required for pre-release tags (e.g.
`v0.1.0-rc1`). The default "latest" path resolves the GitHub
[latest-release API endpoint][latest-api], which excludes pre-releases
by definition (the release workflow at
`.github/workflows/release.yml` marks any tag containing `-` as a
pre-release).

[latest-api]: https://docs.github.com/en/rest/releases/releases#get-the-latest-release

```bash
curl -fsSL https://raw.githubusercontent.com/Grady-Saccullo/wezsesh/main/install.sh \
  | WEZSESH_VERSION=v0.1.0-rc1 sh
```

### Verification

Every download is checked against the corresponding line in the
release's `SHA256SUMS` file before extraction. The script uses
`sha256sum` if available and falls back to `shasum -a 256` (macOS
default). Mismatched checksums abort the install with a non-zero exit.

### PATH

The script does **not** modify shell init files (`~/.bashrc`,
`~/.zshrc`, `~/.profile`, etc.). If `${WEZSESH_INSTALL_DIR:-$HOME/.local/bin}`
is not on your `$PATH`, the script prints a one-line warning telling
you how to add it — typically:

```bash
echo 'export PATH="$HOME/.local/bin:$PATH"' >> ~/.profile
```

Re-running the installer overwrites the existing binary in place; it
never appends to your shell init, so repeated runs are idempotent.

## Nix flake

The `flake.nix` at the repo root exposes the `wezsesh` package as
`packages.${system}.wezsesh` (default). The standard one-liner
installs into your active Nix profile:

```bash
nix profile install github:Grady-Saccullo/wezsesh
```

For a temporary shell that drops `wezsesh` on `$PATH` for the
duration of a single command (handy for trying it out without
mutating your profile):

```bash
nix shell github:Grady-Saccullo/wezsesh -c wezsesh --version
```

For users who manage their own flakes (`home-manager`, `nix-darwin`,
NixOS), add this repo as a flake input and reference
`wezsesh.packages.${system}.wezsesh` from your environment package
list. A fully worked home-manager example is out of scope for this
document.

The flake tracks `nixpkgs-unstable` because that is where wezterm
itself stays current. Users on `nixpkgs-stable` may see a different
wezterm version than the one this flake was last built against; if
that mismatch matters for your setup, prefer the Homebrew or curl
paths.

## From source

```bash
go install github.com/Grady-Saccullo/wezsesh/cmd/wezsesh@latest
```

Caveats:

- Requires Go **1.26.2** or newer (matches the pin in `flake.nix`).
- The version string defaults to `dev` (the literal in
  `cmd/wezsesh/version.go`) when the binary is built without
  `-ldflags='-X main.version=...'`, which is the case for
  `go install`-built binaries. To get a real tag string, build from
  a tagged release tarball with the `-trimpath -ldflags` flags shown
  in `CLAUDE.md`'s "Build & test commands" section, or use one of the
  other install paths.
- `${GOBIN:-${GOPATH:-$HOME/go}/bin}` must be on `$PATH`. This is
  the same caveat the curl-installer prints when its install
  directory is not on `$PATH`.

## Configure your wezterm

After the binary is installed via any of the four paths above, wire
the Lua plugin into your `wezterm.lua`:

```lua
local resurrect = wezterm.plugin.require(
    "https://github.com/MLFlexer/resurrect.wezterm")
local wezsesh = wezterm.plugin.require(
    "https://github.com/Grady-Saccullo/wezsesh")
wezsesh.apply_to_config(config, {
    snapshot_dir = "/path/to/snapshots",
    resurrect = resurrect,
})
```

The minimum-viable opts for a working install are:

- **`binary`** — string|nil. **OPTIONAL.** Defaults to bare `"wezsesh"`
  (PATH lookup) when neither `binary` nor `plugin_root` is set. If you
  installed via Homebrew, the curl installer, the Nix flake, or
  `go install` and the binary is on `$PATH`, leave this unset. Set it
  to an absolute path only if you want to pin a specific build (e.g.
  a development checkout).
- **`resurrect`** — table|nil. **REQUIRED in practice.** Pass the
  `resurrect.wezterm` plugin module table here. When set, the plugin
  calls `resurrect.state_manager.change_state_save_dir(...)` so
  resurrect's `save_state_dir` stays in lockstep with wezsesh's
  `snapshot_dir`. (There is a `_G.resurrect` fallback —
  `resurrect.wezterm` publishes itself onto `_G.resurrect` as a
  side-effect of `wezterm.plugin.require`, so any earlier
  `wezterm.plugin.require` of resurrect populates this slot — but
  wiring `opts.resurrect` explicitly keeps the require ordering
  unambiguous.) Note that `opts.resurrect` is the lockstep wiring
  on top of the canonical
  `wezterm.plugin.require("https://github.com/MLFlexer/resurrect.wezterm")`
  call (already in the example above), not a substitute for it: the
  verb-dispatch handlers in the save / load / switch flows resolve
  resurrect via `_G.resurrect` only (§11.5 verb-dispatch caveat), so
  the canonical require must run regardless of how `opts.resurrect`
  is wired.

Every other opt (`keybinding`, `spawn_mode`, `state_dir`,
`runtime_dir`, `data_dir`, `target_window_id`, `force_close`, `sort`,
`default_action`, `default_action_load_no_prompt`, `confirm_delete`,
`confirm_overwrite`, `exclude`, `new_workspace_command`, `preview.*`,
`markers.*`, `columns`, `name_truncate`, `colors.*`, `hooks.*`,
`resurrect_argv_allowlist`, `log_level`, `keys.*`, `on_before_op`,
`on_after_op`, etc.) is documented in [`docs/design.md`](design.md)
§11 — Configuration schema (`apply_to_config(config, opts)`).

## Verify install

Two smoke checks confirm a working install. Run them in order.

1. **`wezsesh --version`** — zero-dependency, runs offline, prints the
   build tag the binary was compiled with (e.g. `wezsesh v0.1.0`).
   This confirms the binary is on `$PATH` and was built with a real
   tag injected via `-X main.version=${TAG}`. A `wezsesh dev` string
   here typically means a `go install @latest` build (see [From
   source](#from-source)).

   ```bash
   wezsesh --version
   ```

2. **`wezsesh doctor --format json | jq`** — comprehensive environment
   check. Walks every check from `docs/design.md` §8.17 / §8.17.1
   (state.dir, snapshot.dir, runtime.dir, data.dir, trust.dir, HMAC
   key, wezterm version + Lua version, terminfo, font hints, etc.) and
   emits a JSON report with three top-level fields: `Critical` (bool,
   true iff any check failed), `Warnings` (bool, true iff any check
   warned), and `Checks` (an array; each entry carries an `ID`, a
   `Status` of `ok` / `warn` / `fail` / `skip`, a `Message`, and an
   optional `Details` map). The CLI exits non-zero whenever `Critical`
   or `Warnings` is true (§8.20). Run it from inside a wezterm pane —
   `doctor` reads environment variables set by wezterm itself, and
   the report's accuracy degrades when run from a non-wezterm shell.
   `jq` is for pretty-printing only and is not strictly required.

   ```bash
   wezsesh doctor --format json | jq
   ```
