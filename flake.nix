{
  description = "wezsesh - WezTerm session manager TUI";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-parts.url = "github:hercules-ci/flake-parts";
    systems.url = "github:nix-systems/default";
  };

  outputs = inputs @ {
    flake-parts,
    systems,
    ...
  }:
    flake-parts.lib.mkFlake {inherit inputs;} {
      systems = import systems;

      perSystem = {
        config,
        pkgs,
        lib,
        system,
        ...
      }: let
        go = pkgs.go_1_26;

        # Plain Lua 5.4 — wezterm embeds a Lua 5.4 (mlua-rs) runtime, so
        # local syntax/unit tests of canonical_json.lua / hmac.lua etc.
        # should run against the same major.
        lua = pkgs.lua5_4;

        mkScript = name: deps: body:
          pkgs.writeShellApplication {
            inherit name;
            runtimeInputs = [go pkgs.coreutils pkgs.git] ++ deps;
            text = ''
              repo_root="$(git rev-parse --show-toplevel 2>/dev/null)" || {
                echo "error: not in a git repository" >&2
                exit 1
              }
              cd "$repo_root"
              ${body}
            '';
          };

        scripts = rec {
          verify = mkScript "wezsesh-verify" [] ''
            go mod verify
          '';

          vet = mkScript "wezsesh-vet" [] ''
            go vet ./...
          '';

          staticcheck = mkScript "wezsesh-staticcheck" [pkgs.go-tools] ''
            staticcheck ./...
          '';

          govulncheck = mkScript "wezsesh-govulncheck" [pkgs.govulncheck] ''
            govulncheck ./...
          '';

          crypto = mkScript "wezsesh-crypto" [] ''
            sha256sum -c plugin/wezsesh/vendor/SOURCES.lock
          '';

          codegen = mkScript "wezsesh-codegen" [] ''
            go run ./internal/argvallow/codegen --check plugin/wezsesh/default_allowlist.lua
          '';

          test-canonical = mkScript "wezsesh-test-canonical" [] ''
            LC_ALL=C go test ./internal/canonicaljson/... ./plugin/...
          '';

          test-race = mkScript "wezsesh-test-race" [] ''
            go test -race ./...
          '';

          # Local-parity build. Intentionally OMITS CGO_ENABLED=0 — the
          # release channel (.github/workflows/release.yml) is the source
          # of truth for the published binary; this target exists for
          # quick local verification of the ldflags shape.
          build = mkScript "wezsesh-build" [] ''
            go build -trimpath \
              -ldflags="-s -w -X main.version=v$(git describe --tags --always)" \
              ./cmd/wezsesh
          '';

          # Full local-CI suite — mirrors the required gates in
          # .github/workflows/ci.yml so contributors can pre-flight a PR
          # without round-tripping through GitHub Actions.
          ci = mkScript "wezsesh-ci" [pkgs.go-tools pkgs.govulncheck] ''
            go mod verify
            go vet ./...
            staticcheck ./...
            govulncheck ./...
            sha256sum -c plugin/wezsesh/vendor/SOURCES.lock
            go run ./internal/argvallow/codegen --check plugin/wezsesh/default_allowlist.lua
            LC_ALL=C go test ./internal/canonicaljson/... ./plugin/...
            go test -race ./...
            go build -trimpath \
              -ldflags="-s -w -X main.version=v$(git describe --tags --always)" \
              ./cmd/wezsesh
          '';

          # End-to-end smoke. The test compiles unconditionally under
          # `-tags e2e` and skips at runtime when WEZSESH_E2E is unset;
          # this means the same target runs green on hosts without
          # wezterm.
          #
          #   nix run .#e2e               # gated; skips cleanly without wezterm
          #   WEZSESH_E2E=1 nix run .#e2e # full run (requires wezterm on PATH)
          e2e = mkScript "wezsesh-e2e" [] ''
            go test -tags e2e -count=1 -timeout 5m ./e2e/...
          '';

          e2e-vet = mkScript "wezsesh-e2e-vet" [] ''
            go vet -tags e2e ./e2e/...
          '';

          e2e-build = mkScript "wezsesh-e2e-build" [] ''
            go build -tags e2e ./e2e/...
          '';

          # ── Release pre-flight ───────────────────────────────────────────
          # Mirror what .github/workflows/release.yml does per matrix entry,
          # so an operator can rehearse a tag locally before pushing it.
          # The actual publish (GitHub Release upload, multi-runner native
          # parity) stays in the workflow — see docs/release.md.
          #
          # Tag resolution: use $TAG if set, else `v$(git describe --tags
          # --always)`. The workflow uses ${{ github.ref_name }}, which is
          # the literal tag string with leading `v`; matching that here
          # keeps the tarball name and the embedded version identical to
          # what CI would produce.
          #
          # Output layout (matches the workflow's staging):
          #   dist/wezsesh_${TAG}_${GOOS}_${GOARCH}/wezsesh
          #   dist/wezsesh_${TAG}_${GOOS}_${GOARCH}.tar.gz   (release-package)
          #   dist/SHA256SUMS                                (release-build-all)
          release-build = mkScript "wezsesh-release-build" [] ''
            tag="''${TAG:-v$(git describe --tags --always)}"
            goos="''${GOOS:-$(go env GOOS)}"
            goarch="''${GOARCH:-$(go env GOARCH)}"
            stage="dist/wezsesh_''${tag}_''${goos}_''${goarch}"
            mkdir -p "$stage"
            echo "release-build: tag=$tag goos=$goos goarch=$goarch -> $stage/wezsesh"
            CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" \
              go build -trimpath \
                -ldflags="-s -w -X main.version=''${tag}" \
                -o "$stage/wezsesh" \
                ./cmd/wezsesh
            if [ "$goos" = "$(go env GOOS)" ] && [ "$goarch" = "$(go env GOARCH)" ]; then
              "$stage/wezsesh" --version
            fi
          '';

          release-package = mkScript "wezsesh-release-package" [pkgs.gnutar pkgs.gzip release-build] ''
            tag="''${TAG:-v$(git describe --tags --always)}"
            goos="''${GOOS:-$(go env GOOS)}"
            goarch="''${GOARCH:-$(go env GOARCH)}"
            base="wezsesh_''${tag}_''${goos}_''${goarch}"
            stage="dist/$base"

            TAG="$tag" GOOS="$goos" GOARCH="$goarch" wezsesh-release-build

            if [ -f LICENSE ]; then
              cp LICENSE "$stage/"
            else
              echo "warning: LICENSE absent at repo root; tarball will ship without it (see docs/release.md pre-flight)" >&2
            fi
            if [ -f README.md ]; then
              cp README.md "$stage/"
            else
              echo "warning: README.md absent at repo root; tarball will ship without it" >&2
            fi

            (cd dist && tar -czf "''${base}.tar.gz" "$base")
            echo "release-package: dist/''${base}.tar.gz"
          '';

          # Cross-compile + package all release targets locally and write
          # a SHA256SUMS file. NOTE: this rehearses the workflow's *shape*,
          # not its byte-for-byte output — the published binaries are
          # native builds across four GitHub runners, while this target
          # cross-compiles from one host. Use it for layout/version
          # sanity, NOT as a substitute for the workflow.
          release-build-all = mkScript "wezsesh-release-build-all" [pkgs.gnutar pkgs.gzip release-package] ''
            tag="''${TAG:-v$(git describe --tags --always)}"
            echo "release-build-all: tag=$tag (cross-compiled local pre-flight; see docs/release.md)"

            for target in linux-amd64 linux-arm64 darwin-arm64; do
              goos="''${target%-*}"
              goarch="''${target#*-}"
              TAG="$tag" GOOS="$goos" GOARCH="$goarch" wezsesh-release-package
            done

            (
              cd dist
              if command -v sha256sum >/dev/null 2>&1; then
                sha256sum wezsesh_''${tag}_*.tar.gz | sort -k2 > SHA256SUMS
              else
                shasum -a 256 wezsesh_''${tag}_*.tar.gz | sort -k2 > SHA256SUMS
              fi
              cat SHA256SUMS
            )
          '';
        };

        # ── wezsesh package ────────────────────────────────────────────────
        src = lib.fileset.toSource {
          root = ./.;
          fileset = lib.fileset.unions [
            (lib.fileset.maybeMissing ./go.mod)
            (lib.fileset.maybeMissing ./go.sum)
            (lib.fileset.maybeMissing ./cmd)
            (lib.fileset.maybeMissing ./internal)
            (lib.fileset.maybeMissing ./plugin)
          ];
        };

        hasGoMod = builtins.pathExists ./go.mod;

        wezsesh = pkgs.buildGoModule {
          pname = "wezsesh";
          version = "0.0.0-dev";
          inherit src;

          # When go.mod / go.sum first land, `nix build` will fail with the
          # expected hash — copy it in here. Subsequent dep changes require
          # re-pinning. Leave as `lib.fakeHash` to force re-derivation.
          vendorHash = "sha256-C9eYOouZ2mCw6lJ8WtV5z/hDJZU3K4g2C9XuOmKm21Y=";

          subPackages = ["cmd/wezsesh"];

          ldflags = [
            "-s"
            "-w"
            "-X=main.version=${wezsesh.version}"
          ];

          env.CGO_ENABLED = "0";
          preBuild = ''
            export GOFLAGS="$GOFLAGS -trimpath"
          '';

          nativeBuildInputs = [go];
          inherit go;

          doCheck = true;

          meta = {
            description = "WezTerm session manager TUI";
            homepage = "https://github.com/grady-saccullo/wezsesh";
            license = lib.licenses.mit;
            mainProgram = "wezsesh";
            platforms = lib.platforms.unix;
          };
        };
      in {
        # ── Packages ───────────────────────────────────────────────────────
        # Until go.mod exists, this evaluates fine but builds will fail at
        # the Go compile step — that's the intended signal.
        packages = lib.optionalAttrs hasGoMod {
          default = wezsesh;
          wezsesh = wezsesh;
        };

        # `nix run`
        apps =
          lib.mapAttrs (_: drv: {
            type = "app";
            program = lib.getExe drv;
          })
          scripts
          // lib.optionalAttrs hasGoMod {
            default = {
              type = "app";
              program = lib.getExe wezsesh;
            };
          };

        # `nix fmt` — matches the dotfiles' alejandra style.
        formatter = pkgs.alejandra;

        # ── Dev shell ──────────────────────────────────────────────────────
        devShells.default = pkgs.mkShell {
          name = "wezsesh-dev";

          packages = [
            # Go toolchain
            go
            pkgs.gopls
            pkgs.gotools # goimports, godoc, etc.
            pkgs.go-tools # staticcheck
            pkgs.golangci-lint
            pkgs.govulncheck
            pkgs.delve # debugger

            # Lua side
            lua
            pkgs.stylua # formatter
            pkgs.selene # linter (Rust-based, faster than luacheck)
            pkgs.lua-language-server

            # Integration target — TUI manipulates wezterm via `wezterm cli`
            # and OSC 1337 user-vars; having it on PATH means dev-shell
            # invocations match production. Tracks nixpkgs-unstable rev;
            # override the nixpkgs input to pin a different version.
            pkgs.wezterm

            # `coreutils` provides sha256sum on darwin where it isn't system.
            pkgs.coreutils
            pkgs.git
            pkgs.jujutsu # jj — repo is jj-colocated
            pkgs.jq
          ];

          shellHook = ''
            export GOFLAGS="''${GOFLAGS:-} -trimpath"
            # canonical-JSON byte-equality tests must run with LC_ALL=C
            # to remove locale drift in Lua's string `<` ordering.
            export LC_ALL=C

            echo ""
            echo "wezsesh dev shell"
            echo "  go        $(${lib.getExe go} version | awk '{print $3}')"
            echo "  wezterm   $(${pkgs.wezterm}/bin/wezterm --version | awk '{print $2}')"
            echo "  lua       $(${lua}/bin/lua -v 2>&1 | awk '{print $2}')"
            echo ""
            echo "Test against a different wezterm rev:"
            echo "  nix develop --override-input nixpkgs github:NixOS/nixpkgs/<rev>"
            echo ""
            echo "Common tasks (single source of truth — flake.nix):"
            echo "  nix run .#ci                   # full local-CI suite"
            echo "  nix run .#test-race            # go test -race ./..."
            echo "  nix run .#test-canonical       # LC_ALL=C canonical-JSON tests"
            echo "  nix run .#staticcheck          # static analysis"
            echo "  nix run .#govulncheck          # CVE scan"
            echo "  nix run .#crypto               # vendored Lua sha256sum -c"
            echo "  nix run .#build                # local-parity reproducible build"
            echo "  nix run .#e2e                  # gated end-to-end smoke"
            echo "  nix build                      # nix-built reproducible binary"
            echo "  nix flake check                # all flake checks"
            echo ""
            echo "Release pre-flight (rehearse the workflow locally — see docs/release.md):"
            echo "  TAG=v0.1.0 nix run .#release-build       # one host-native build"
            echo "  TAG=v0.1.0 nix run .#release-package     # build + tarball"
            echo "  TAG=v0.1.0 nix run .#release-build-all   # cross-compile all targets + SHA256SUMS"
            echo ""
            echo "VCS: jj-colocated (.jj/ + .git/). Use jj for commits/diffs;"
            echo "git tooling (CI, IDE) sees the colocated .git/ normally."
            echo ""
          '';
        };

        # ── Checks (`nix flake check`) ─────────────────────────────────────
        checks = lib.optionalAttrs hasGoMod {
          wezsesh-build = wezsesh;

          wezsesh-vet =
            pkgs.runCommand "wezsesh-vet" {
              nativeBuildInputs = [go];
              src = wezsesh.src;
            } ''
              cp -r $src/. ./source
              chmod -R u+w ./source
              cd ./source
              export HOME=$TMPDIR
              export GOFLAGS="-mod=vendor"
              go vet ./... 2>&1 | tee $out
            '';

          wezsesh-staticcheck =
            pkgs.runCommand "wezsesh-staticcheck" {
              nativeBuildInputs = [go pkgs.go-tools];
              src = wezsesh.src;
            } ''
              cp -r $src/. ./source
              chmod -R u+w ./source
              cd ./source
              export HOME=$TMPDIR
              staticcheck ./... 2>&1 | tee $out
            '';
        };
      };
    };
}
