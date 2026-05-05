# wezsesh release runbook

Operator-facing checklist for cutting a tagged release. The release
workflow lives at `.github/workflows/release.yml`; this document covers
the human steps around it.

## Pre-flight checklist

Before pushing a tag, confirm each of the following:

- [ ] Working tree is clean (no uncommitted changes on `main`).
- [ ] CI is green on the commit you are about to tag (see the `ci`
      workflow on the GitHub Actions page).
- [ ] `LICENSE` file exists at the repository root. The release
      workflow packages it into every tarball; if absent, the workflow
      emits a `::warning::` and ships the tarball without it. Add a
      `LICENSE` file before the first public tag.
- [ ] `README.md` exists at the repository root (same handling as
      `LICENSE`).
- [ ] `M.VERSION` in `plugin/init.lua` matches `M.VERSION` in
      `plugin/wezsesh/manager.lua`, and **both equal the tag string
      without the leading `v`** (e.g. for tag `v0.1.0`, both files set
      `M.VERSION = "0.1.0"`). The Lua plugin asserts this equality at
      `apply_to_config` time — see "Version-drift enforcement" below.
- [ ] `CHANGELOG` (if present) has been updated with the new tag's
      notes.

## Bumping the plugin version

The plugin carries its own version string, surfaced as
`WEZSESH_PLUGIN_VERSION` over the wire (Appendix A of `docs/design.md`)
and used by §10.7. It is duplicated in two files for layering reasons,
and the duplication is asserted at runtime:

- `plugin/init.lua` → `M.VERSION = "X.Y.Z"`
- `plugin/wezsesh/manager.lua` → `M.VERSION = "X.Y.Z"`

Both must be bumped in the same commit, in lockstep with the tag. The
trailing `apply_to_config` body in `plugin/init.lua` raises
`WEZSESH_VERSION_DRIFT` if the two values diverge — so a forgotten bump
will fail loudly the first time a user reloads their wezterm config
against the new release. There is no separate CI grep gate for this
equality; the runtime assert is the enforcement point.

## Tag and push

```bash
# From a clean main with the version bumps committed:
git tag v0.1.0
git push origin v0.1.0
```

The release workflow triggers on any tag matching
`v[0-9]+.[0-9]+.[0-9]+*` — stable releases (`v0.1.0`) and pre-release
suffixes (`v0.1.0-rc1`, `v1.2.3-beta.4`) both match. Pre-release tags
are auto-marked as "pre-release" on the GitHub Releases page (the
workflow detects the hyphen in the tag name).

## What the release workflow does

1. **build** matrix (`darwin/amd64`, `darwin/arm64`, `linux/amd64`,
   `linux/arm64`) — runs on the same runner pinning as the CI test
   matrix (`macos-13`, `macos-14`, `ubuntu-24.04`, `ubuntu-24.04-arm`)
   so released binaries come from the same hardware as tested
   binaries. Each job runs:
   ```
   go build -trimpath -ldflags="-s -w -X main.version=${TAG}" ./cmd/wezsesh
   ```
   then packages `wezsesh`, `LICENSE` (if present), and `README.md`
   into `wezsesh_${TAG}_${OS}_${ARCH}.tar.gz` (e.g.
   `wezsesh_v0.1.0_darwin_arm64.tar.gz`).

2. **checksums** — downloads every tarball, computes
   `sha256sum`, writes a single `SHA256SUMS` file in
   `sha256sum --check` format.

3. **release** — creates a GitHub Release for the tag and uploads the
   four tarballs and `SHA256SUMS`.

## Post-release verification

After the workflow completes, exercise the published artefacts:

```bash
TAG=v0.1.0
# Pick the tarball for your host and download alongside SHA256SUMS:
gh release download "${TAG}" --repo <owner>/wezsesh \
    --pattern 'wezsesh_*_darwin_arm64.tar.gz' --pattern 'SHA256SUMS'

# Verify the checksum:
sha256sum -c SHA256SUMS --ignore-missing   # macOS: shasum -a 256 -c

# Extract and confirm --version reports the tag string:
tar -xzf wezsesh_${TAG}_darwin_arm64.tar.gz
./wezsesh_${TAG}_darwin_arm64/wezsesh --version
# Expected output: wezsesh ${TAG}  (e.g. "wezsesh v0.1.0")
```

If `--version` prints `wezsesh dev` instead of `wezsesh ${TAG}`, the
`-X main.version` ldflag did not land — open an issue and treat the
release as failed.

## Failure recovery

If the workflow fails partway through the matrix (one job succeeds,
another fails), the GitHub Release will not be created (it is gated on
the `release` job, which depends on `checksums`, which depends on every
`build` matrix entry). To recover:

1. Inspect the failing job's logs and identify the root cause.
2. If the cause was transient (runner flakiness, network hiccup), use
   the GitHub Actions UI to re-run failed jobs.
3. If the cause was a bug in the build, fix it on `main`, then:
   ```bash
   git tag -d vX.Y.Z              # delete the local tag
   git push origin :refs/tags/vX.Y.Z   # delete the remote tag
   # If a partial GitHub Release was created, delete it via:
   gh release delete vX.Y.Z --repo <owner>/wezsesh
   ```
   then re-tag the corrected commit and push.

A successful release that turns out to ship a broken binary cannot be
rewritten — yank it instead by marking the GitHub Release as
"draft" or deleting it, and cut a new patch tag (`vX.Y.(Z+1)`).

## Future work

- **Cosign keyless signing of `SHA256SUMS`.** The current workflow
  ships an unsigned `SHA256SUMS` file; downstream consumers
  (Homebrew, curl-installer) verify integrity via the SHA itself. For
  defence-in-depth before going public, add a step using
  `sigstore/cosign-installer@v3` and `cosign sign-blob --yes
  SHA256SUMS` (with `id-token: write` permission on the release job
  for the OIDC token), and upload `SHA256SUMS.sig` and
  `SHA256SUMS.pem` alongside the existing artefacts. Deferred for
  this initial workflow because (a) keyless verification ergonomics
  for end users are non-trivial, and (b) the SHA itself is the
  load-bearing integrity check for Homebrew/curl-installer flows.
