package dirproviders

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ───────────────────────────────────────────────────────────────────
// Config.Validate
// ───────────────────────────────────────────────────────────────────

func TestValidate_Command_RequiresArgv(t *testing.T) {
	c := Config{Type: TypeCommand}
	if err := c.Validate(); err == nil {
		t.Fatal("want error for empty argv")
	}
}

func TestValidate_Command_DefaultsLimitAndTimeout(t *testing.T) {
	c := Config{Type: TypeCommand, Argv: []string{"true"}}
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if c.Limit != 200 {
		t.Errorf("Limit=%d, want default 200", c.Limit)
	}
	if c.TimeoutMs != 5000 {
		t.Errorf("TimeoutMs=%d, want default 5000", c.TimeoutMs)
	}
}

func TestValidate_Command_RejectsTimeoutOutOfRange(t *testing.T) {
	for _, ms := range []int{50, 99, 60001, 100000} {
		c := Config{Type: TypeCommand, Argv: []string{"true"}, TimeoutMs: ms}
		if err := c.Validate(); err == nil {
			t.Errorf("ms=%d: want error, got nil", ms)
		}
	}
}

func TestValidate_Command_RejectsNULInArgv(t *testing.T) {
	c := Config{Type: TypeCommand, Argv: []string{"zoxide", "query\x00-l"}}
	if err := c.Validate(); err == nil {
		t.Fatal("want error for NUL in argv")
	}
}

func TestValidate_Directory_RequiresPath(t *testing.T) {
	c := Config{Type: TypeDirectory}
	if err := c.Validate(); err == nil {
		t.Fatal("want error for empty path")
	}
}

func TestValidate_Directory_DefaultsDepthAndLimit(t *testing.T) {
	c := Config{Type: TypeDirectory, Path: "/tmp"}
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if c.Depth != 2 {
		t.Errorf("Depth=%d, want default 2", c.Depth)
	}
	if c.Limit != 200 {
		t.Errorf("Limit=%d, want default 200", c.Limit)
	}
}

func TestValidate_Directory_RejectsDepthOutOfRange(t *testing.T) {
	for _, d := range []int{-1, 11, 100} {
		c := Config{Type: TypeDirectory, Path: "/tmp", Depth: d}
		if err := c.Validate(); err == nil {
			t.Errorf("depth=%d: want error, got nil", d)
		}
	}
}

func TestValidate_Static_RequiresPaths(t *testing.T) {
	c := Config{Type: TypeStatic}
	if err := c.Validate(); err == nil {
		t.Fatal("want error for empty paths")
	}
}

func TestValidate_UnknownType(t *testing.T) {
	c := Config{Type: "weird"}
	err := c.Validate()
	if !errors.Is(err, ErrUnknownType) {
		t.Fatalf("got %v, want ErrUnknownType", err)
	}
}

// ───────────────────────────────────────────────────────────────────
// runStatic
// ───────────────────────────────────────────────────────────────────

func TestRunStatic_AcceptsExistingDirsRejectsRest(t *testing.T) {
	tmp := t.TempDir()
	good := filepath.Join(tmp, "good")
	if err := os.MkdirAll(good, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	notADir := filepath.Join(tmp, "file")
	if err := os.WriteFile(notADir, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg := Config{
		Type:  TypeStatic,
		Paths: []string{good, notADir, "/nonexistent/abs/path", "relative"},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	rows, err := runStatic(&cfg, nil)
	if err != nil {
		t.Fatalf("runStatic: %v", err)
	}
	if len(rows) != 1 || rows[0].Path != good {
		t.Fatalf("got %+v, want exactly one row at %q", rows, good)
	}
	if rows[0].Name != "good" {
		t.Errorf("Name=%q, want \"good\"", rows[0].Name)
	}
}

// ───────────────────────────────────────────────────────────────────
// runDirectory
// ───────────────────────────────────────────────────────────────────

func TestRunDirectory_DepthOne_RootOnly(t *testing.T) {
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, "a"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cfg := Config{Type: TypeDirectory, Path: tmp, Depth: 1}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	rows, err := runDirectory(&cfg, nil)
	if err != nil {
		t.Fatalf("runDirectory: %v", err)
	}
	if len(rows) != 1 || rows[0].Path != tmp {
		t.Fatalf("got %+v, want exactly the root", rows)
	}
}

func TestRunDirectory_DepthTwo_RootAndImmediateChildren(t *testing.T) {
	tmp := t.TempDir()
	for _, child := range []string{"a", "b", "c"} {
		if err := os.MkdirAll(filepath.Join(tmp, child, "deep"), 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	cfg := Config{Type: TypeDirectory, Path: tmp, Depth: 2}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	rows, err := runDirectory(&cfg, nil)
	if err != nil {
		t.Fatalf("runDirectory: %v", err)
	}
	// depth=2: root + 3 immediate children = 4 rows.
	if len(rows) != 4 {
		t.Fatalf("got %d rows, want 4 (root + 3 children); rows=%+v", len(rows), rows)
	}
}

func TestRunDirectory_SkipsHiddenWithoutFlag(t *testing.T) {
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, ".hidden"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(tmp, "visible"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cfg := Config{Type: TypeDirectory, Path: tmp, Depth: 2}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	rows, err := runDirectory(&cfg, nil)
	if err != nil {
		t.Fatalf("runDirectory: %v", err)
	}
	for _, r := range rows {
		if strings.Contains(r.Path, "/.hidden") {
			t.Errorf("hidden dir leaked into rows: %q", r.Path)
		}
	}
}

func TestRunDirectory_HonoursLimit(t *testing.T) {
	tmp := t.TempDir()
	for i := 0; i < 10; i++ {
		if err := os.MkdirAll(filepath.Join(tmp, string(rune('a'+i))), 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	cfg := Config{Type: TypeDirectory, Path: tmp, Depth: 2, Limit: 3}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	rows, err := runDirectory(&cfg, nil)
	if err != nil {
		t.Fatalf("runDirectory: %v", err)
	}
	if len(rows) > 3 {
		t.Fatalf("got %d rows, want <= 3 (limit)", len(rows))
	}
}

// ───────────────────────────────────────────────────────────────────
// runCommand
// ───────────────────────────────────────────────────────────────────

func TestRunCommand_CatPathsRoundTrip(t *testing.T) {
	tmp := t.TempDir()
	a := filepath.Join(tmp, "a")
	b := filepath.Join(tmp, "b")
	for _, p := range []string{a, b} {
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	// Pipe the paths through `cat <file>` to dodge shell-escape
	// concerns around `printf %s\n` (the shell consumes the
	// backslash before printf sees it).
	list := filepath.Join(tmp, "list.txt")
	if err := os.WriteFile(list, []byte(a+"\n"+b+"\n"), 0o600); err != nil {
		t.Fatalf("write list: %v", err)
	}
	cfg := Config{Type: TypeCommand, Argv: []string{"cat", list}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rows, err := runCommand(ctx, &cfg, nil)
	if err != nil {
		t.Fatalf("runCommand: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2; rows=%+v", len(rows), rows)
	}
}

func TestRunCommand_NonExistentPathsAreFilteredOut(t *testing.T) {
	tmp := t.TempDir()
	list := filepath.Join(tmp, "list.txt")
	if err := os.WriteFile(list, []byte("/nonexistent/abs/path/here\n"), 0o600); err != nil {
		t.Fatalf("write list: %v", err)
	}
	cfg := Config{Type: TypeCommand, Argv: []string{"cat", list}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rows, err := runCommand(ctx, &cfg, nil)
	if err != nil {
		t.Fatalf("runCommand: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("got %d rows, want 0 (path doesn't exist)", len(rows))
	}
}

func TestRunCommand_HonoursLimit(t *testing.T) {
	tmp := t.TempDir()
	dirs := []string{}
	for _, name := range []string{"a", "b", "c", "d", "e"} {
		p := filepath.Join(tmp, name)
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		dirs = append(dirs, p)
	}
	list := filepath.Join(tmp, "list.txt")
	if err := os.WriteFile(list, []byte(strings.Join(dirs, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write list: %v", err)
	}
	cfg := Config{Type: TypeCommand, Limit: 2, Argv: []string{"cat", list}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rows, err := runCommand(ctx, &cfg, nil)
	if err != nil {
		t.Fatalf("runCommand: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2 (limit)", len(rows))
	}
}

// ───────────────────────────────────────────────────────────────────
// RunAll
// ───────────────────────────────────────────────────────────────────

func TestRunAll_UnionsValidatedRowsAcrossProviders(t *testing.T) {
	tmp := t.TempDir()
	a := filepath.Join(tmp, "from-static")
	b := filepath.Join(tmp, "from-dir")
	if err := os.MkdirAll(a, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.MkdirAll(b, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	configs := []Config{
		{Type: TypeStatic, Paths: []string{a}},
		{Type: TypeDirectory, Path: tmp, Depth: 1},
	}
	rows := RunAll(context.Background(), configs, nil)
	if len(rows) < 2 {
		t.Fatalf("got %d rows, want at least 2; rows=%+v", len(rows), rows)
	}
}

func TestRunAll_InvalidConfigContributesZero(t *testing.T) {
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, "x"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	configs := []Config{
		{Type: "weird"},                             // invalid
		{Type: TypeStatic, Paths: []string{tmp}},    // valid
	}
	rows := RunAll(context.Background(), configs, nil)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1 (only the valid provider)", len(rows))
	}
}
