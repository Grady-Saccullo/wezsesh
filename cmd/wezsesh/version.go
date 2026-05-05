package main

// version carries the wezsesh build identifier surfaced by `--version`
// (and embedded in diagnostic output by future subcommands). It defaults
// to "dev" for non-release builds and is overridden at link time via:
//
//	go build -ldflags="-X main.version=v$(git describe --tags --always)" ./cmd/wezsesh
//
// per CLAUDE.md "Build & test commands". Keeping the symbol in its own
// file makes the linker target stable across cmd/wezsesh refactors.
var version = "dev"
