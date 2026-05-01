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
        packages = lib.optionalAttrs hasGoMod {
          default = wezsesh;
          wezsesh = wezsesh;
        };

        # `nix run`
        apps = lib.optionalAttrs hasGoMod {
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
