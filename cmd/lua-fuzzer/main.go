// Command lua-fuzzer drives the §17.5 Lua handler fuzz harness from
// CI. Its job is small: locate `lua` on PATH, locate
// `plugin/wezsesh/fuzz/fuzz_spec.lua` relative to the repo root, exec
// the spec with `--seed=<int>` and (optionally) `--iters=<int>`, and
// propagate exit status. Stdout / stderr stream through unchanged.
//
// Usage:
//
//	lua-fuzzer [--seed=<int>] [--iters=<int>] [--root=<dir>] [--seeds=<file>]
//
// Flags:
//
//	--seed   integer seed for math.randomseed inside fuzz_spec.lua;
//	         the spec is deterministic given a fixed seed (defaults
//	         to 1, matching the spec's own default). Ignored if
//	         --seeds is set.
//	--iters  if non-zero, downscales every per-class iteration plan
//	         (the spec divides each default by 1000 and multiplies
//	         by --iters, clamping to 1). Pass --iters=10 for a
//	         smoke run; omit (or 0) for the load-bearing CI default.
//	         CI MUST NOT pass --iters (the BYTE_FLOOR gate is waived
//	         under any --iters override; see fuzz_spec.lua).
//	--root   path to the wezsesh repo root; defaults to the working
//	         directory. The fuzz spec is expected at
//	         <root>/plugin/wezsesh/fuzz/fuzz_spec.lua.
//	--seeds  path to a regression-seed file (one integer per line,
//	         `#` comments allowed). When set, lua-fuzzer iterates
//	         every seed in the file and invokes the spec once per
//	         seed, propagating any non-zero exit. Defaults to
//	         <root>/plugin/wezsesh/fuzz/seeds.txt if that file
//	         exists; otherwise single-seed mode via --seed. Pass
//	         `--seeds=none` to force single-seed mode and bypass the
//	         default. If both --seed and --seeds are set (and
//	         --seeds is not "none"), --seeds wins.
//
// Exit status mirrors the spec: 0 on success, 1 on assertion failure,
// 2 on harness error (lua not found, spec missing, etc.).
package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run is the testable entry point. It returns the exit code rather
// than calling os.Exit directly so cmd/lua-fuzzer/main_test.go can
// invoke it as a black-box without spawning a subprocess for every
// case. Callers in main() do `os.Exit(run(...))`.
func run(argv []string, stdout, stderr *os.File) int {
	fs := flag.NewFlagSet("lua-fuzzer", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		seed  int
		iters int
		root  string
		seeds string
	)
	fs.IntVar(&seed, "seed", 1, "math.randomseed for fuzz_spec.lua")
	fs.IntVar(&iters, "iters", 0,
		"per-class iteration scale (0 = CI default; 10 = smoke)")
	fs.StringVar(&root, "root", "",
		"wezsesh repo root (default: cwd)")
	fs.StringVar(&seeds, "seeds", "",
		"regression-seed file (default: <root>/plugin/wezsesh/fuzz/seeds.txt if present)")
	if err := fs.Parse(argv); err != nil {
		return 2
	}

	if root == "" {
		cwd, err := os.Getwd()
		if err != nil {
			fmt.Fprintf(stderr, "lua-fuzzer: getwd: %v\n", err)
			return 2
		}
		root = cwd
	}

	specPath := filepath.Join(root, "plugin", "wezsesh", "fuzz",
		"fuzz_spec.lua")
	if _, err := os.Stat(specPath); err != nil {
		fmt.Fprintf(stderr,
			"lua-fuzzer: fuzz spec not found at %s: %v\n",
			specPath, err)
		return 2
	}

	// Default --seeds to the canonical regression file when present.
	// Sentinel "none" forces single-seed mode and bypasses the default
	// (used by CI for the fresh-seed coverage line that complements
	// the regression-seed run).
	if seeds == "" {
		def := filepath.Join(root, "plugin", "wezsesh", "fuzz", "seeds.txt")
		if _, err := os.Stat(def); err == nil {
			seeds = def
		}
	} else if seeds == "none" {
		seeds = ""
	}

	var seedList []int
	if seeds != "" {
		var err error
		seedList, err = readSeeds(seeds)
		if err != nil {
			fmt.Fprintf(stderr, "lua-fuzzer: %v\n", err)
			return 2
		}
		if len(seedList) == 0 {
			fmt.Fprintf(stderr,
				"lua-fuzzer: --seeds=%s has no seeds\n", seeds)
			return 2
		}
	} else {
		seedList = []int{seed}
	}

	luaPath, err := exec.LookPath("lua")
	if err != nil {
		fmt.Fprintf(stderr,
			"lua-fuzzer: `lua` not found on PATH: %v\n", err)
		return 2
	}

	for _, s := range seedList {
		args := []string{specPath, fmt.Sprintf("--seed=%d", s)}
		if iters > 0 {
			args = append(args, fmt.Sprintf("--iters=%d", iters))
		}
		cmd := exec.Command(luaPath, args...)
		cmd.Stdout = stdout
		cmd.Stderr = stderr
		cmd.Dir = root
		runErr := cmd.Run()
		if runErr == nil {
			continue
		}
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			return exitErr.ExitCode()
		}
		fmt.Fprintf(stderr, "lua-fuzzer: exec failed: %v\n", runErr)
		return 2
	}
	return 0
}

func readSeeds(path string) ([]int, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open --seeds=%s: %w", path, err)
	}
	defer f.Close()
	var out []int
	sc := bufio.NewScanner(f)
	lineno := 0
	for sc.Scan() {
		lineno++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		n, err := strconv.Atoi(line)
		if err != nil {
			return nil, fmt.Errorf(
				"%s:%d: not an integer: %q", path, lineno, line)
		}
		out = append(out, n)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return out, nil
}
