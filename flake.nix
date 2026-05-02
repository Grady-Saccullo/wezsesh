{
  description = "wezsesh - WezTerm session manager TUI";

  inputs = {
    # nixpkgs-unstable tracks wezterm closely (currently
    # 0-unstable-2026-03-31). To test against an older wezterm rev, pin or
    # override this input:
    #   nix develop --override-input nixpkgs github:NixOS/nixpkgs/<rev>
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-parts.url = "github:hercules-ci/flake-parts";
    # darwin/linux × arm64/amd64 — matches PRD_V7 §3 supported targets.
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
        # PRD_V7 §8.1 pins Go 1.26.2.
        go = pkgs.go_1_26;

        # Plain Lua 5.4 — wezterm embeds a Lua 5.4 (mlua-rs) runtime, so
        # local syntax/unit tests of canonical_json.lua / hmac.lua etc.
        # should run against the same major.
        lua = pkgs.lua5_4;

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

        # ── Build-loop driver ─────────────────────────────────────────────
        # Runs `claude -p '/next-task'` repeatedly in fresh processes so each
        # iteration starts with a cold context (no prompt-cache carry-over,
        # no conversation memory, no PATH bleed). State lives in PROJECT.md;
        # the loop stops when a /next-task invocation produces no new commit.
        #
        # Usage:
        #   nix run .#build-loop           — one shot (50 iter cap)
        #   MAX_ITERS=5 nix run .#build-loop
        #   wezsesh-build-loop             — same, from inside `nix develop`
        #
        # `claude` must be on PATH; this driver does NOT install it.
        buildLoop = pkgs.writeShellApplication {
          name = "wezsesh-build-loop";
          runtimeInputs = [pkgs.git pkgs.coreutils];
          text = ''
            # We manage exit codes per-iteration; don't bail on /next-task rc.
            set +o errexit

            if ! command -v claude >/dev/null 2>&1; then
              echo "error: 'claude' not on PATH" >&2
              exit 1
            fi

            repo_root="$(git rev-parse --show-toplevel 2>/dev/null)" || {
              echo "error: not in a git repository" >&2
              exit 1
            }
            cd "$repo_root"

            if [[ -n "$(git status --porcelain)" ]]; then
              echo "error: working tree is dirty — commit or stash first" >&2
              git status --short | head -10 >&2
              exit 1
            fi

            max_iters="''${MAX_ITERS:-50}"
            log_file="''${LOG:-build.log}"
            extra_args=("$@")  # passed through to `claude` (e.g. --dangerously-skip-permissions)

            echo "wezsesh-build-loop: max_iters=$max_iters log=$log_file"
            echo "stop conditions: HEAD doesn't advance, or iter cap hit, or ctrl-C"

            for ((i=1; i<=max_iters; i++)); do
              before=$(git rev-parse HEAD)
              printf '\n=== iter %d  %s ===\n' "$i" "$(date -Iseconds)" | tee -a "$log_file"

              claude "''${extra_args[@]}" -p '/next-task' 2>&1 | tee -a "$log_file"
              rc=''${PIPESTATUS[0]}
              echo "[loop] claude rc=$rc" | tee -a "$log_file"

              after=$(git rev-parse HEAD)
              if [[ "$before" == "$after" ]]; then
                echo "[loop] no new commits — stopping" | tee -a "$log_file"
                exit 0
              fi
            done

            echo "[loop] MAX_ITERS=$max_iters reached — stopping" | tee -a "$log_file"
          '';
        };

        wezsesh = pkgs.buildGoModule {
          pname = "wezsesh";
          version = "0.0.0-dev";
          inherit src;

          # When go.mod / go.sum first land, `nix build` will fail with the
          # expected hash — copy it in here. Subsequent dep changes require
          # re-pinning. Leave as `lib.fakeHash` to force re-derivation.
          vendorHash = lib.fakeHash;

          subPackages = ["cmd/wezsesh"];

          ldflags = [
            "-s"
            "-w"
            "-X=main.version=${wezsesh.version}"
          ];

          # Reproducible-builds requirement (PRD §8.1).
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
        packages =
          {
            build-loop = buildLoop;
          }
          // lib.optionalAttrs hasGoMod {
            default = wezsesh;
            wezsesh = wezsesh;
          };

        # `nix run`
        apps =
          {
            build-loop = {
              type = "app";
              program = lib.getExe buildLoop;
            };
          }
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
            # Go toolchain (PRD_V7 §8.1)
            go
            pkgs.gopls
            pkgs.gotools # goimports, godoc, etc.
            pkgs.go-tools # staticcheck (PRD §8.1 required check)
            pkgs.golangci-lint
            pkgs.govulncheck # PRD §8.1 required check
            pkgs.delve # debugger

            # Lua side (PRD §6.10 plugin/)
            lua
            pkgs.stylua # formatter
            pkgs.selene # linter (Rust-based, faster than luacheck)
            pkgs.lua-language-server

            # Integration target — TUI manipulates wezterm via `wezterm cli`
            # and OSC 1337 user-vars; having it on PATH means dev-shell
            # invocations match production. Tracks nixpkgs-unstable rev;
            # override the nixpkgs input to pin a different version.
            pkgs.wezterm

            # Supply-chain helpers (PRD §8.1: sha256sum -c on vendored Lua).
            # `coreutils` provides sha256sum on darwin where it isn't system.
            pkgs.coreutils
            pkgs.git
            pkgs.gnumake
            pkgs.jq

            # /next-task loop driver — runs claude -p in fresh processes so
            # each iteration has cold context. See `wezsesh-build-loop --help`.
            buildLoop
          ];

          shellHook = ''
            export GOFLAGS="''${GOFLAGS:-} -trimpath"
            # PRD §6.3: canonical-JSON byte-equality tests must run with
            # LC_ALL=C to remove locale drift in Lua's string `<` ordering.
            export LC_ALL=C

            echo "wezsesh dev shell"
            echo "  go        $(${lib.getExe go} version | awk '{print $3}')"
            echo "  wezterm   $(${pkgs.wezterm}/bin/wezterm --version | awk '{print $2}')"
            echo "  lua       $(${lua}/bin/lua -v 2>&1 | awk '{print $2}')"
            echo ""
            echo "Test against a different wezterm rev:"
            echo "  nix develop --override-input nixpkgs github:NixOS/nixpkgs/<rev>"
            echo ""
            echo "Common tasks:"
            echo "  go build ./cmd/wezsesh         # build binary"
            echo "  go test ./...                  # unit tests"
            echo "  staticcheck ./...              # static analysis"
            echo "  govulncheck ./...              # CVE scan"
            echo "  go mod verify                  # go.sum tamper check"
            echo "  nix build                      # reproducible build"
            echo "  nix flake check                # run all flake checks"
            echo ""
            echo "Drive the build (one /next-task per fresh claude process):"
            echo "  wezsesh-build-loop             # (or: nix run .#build-loop)"
            echo "  MAX_ITERS=5 wezsesh-build-loop"
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
