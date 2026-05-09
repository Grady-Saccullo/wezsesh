package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Grady-Saccullo/wezsesh/internal/config"
)

// ──────────────────────────────────────────────────────────────────────
// `wezsesh tail` — interleaved follower for the two structured log files
// produced by the binary and the plugin.
//
// Sources:
//   - <stateDir>/wezsesh.log   — Go-side slog JSON. Schema: time (RFC3339),
//                                 level, msg, binary_session_id, optional
//                                 trace_id / plugin_session_id, plus
//                                 caller-supplied kv attrs.
//   - <stateDir>/plugin.log    — Lua-side JSON. Schema: level, ts (unix
//                                 seconds int), msg, plugin_session_id,
//                                 optional trace_id / binary_session_id /
//                                 pane_id, plus caller-supplied fields.
//                                 Lives next to wezsesh.log so a `wezsesh
//                                 tail` from a fresh shell finds both
//                                 streams under the same auto-detected
//                                 path; the plugin's spawn-env runtime_dir
//                                 isn't visible to that shell.
//
// Behaviour: spin a reader goroutine per source, parse each line as JSON,
// normalise the timestamp into a single ordering key, and emit through a
// short buffered window so out-of-order arrivals from the two sources can
// be sorted by time before printing. In follow mode each reader polls for
// new data with a 50 ms sleep and detects rotation by comparing the open
// file's dev+inode against the path's current dev+inode; on mismatch the
// reader closes and re-opens the path.
//
// The subcommand is read-only over the log files; it does NOT touch the
// IPC dispatcher, the wire envelope, the canonical encoder, or the
// logger. CLAUDE.md "platform-path-first" is N/A here — there's nothing
// for wezterm to do for this surface.
// ──────────────────────────────────────────────────────────────────────

// tailLevelOrder maps the four canonical level names to their numeric
// rank so the --level filter ("only show records at least this severe")
// is a single integer comparison.
var tailLevelOrder = map[string]int{
	"debug": 0,
	"info":  1,
	"warn":  2,
	"error": 3,
}

// tailFlags holds the parsed command-line flags. Defaults pin the
// no-filter path: every record from both sources, follow forever, with
// colour when stdout is a TTY.
type tailFlags struct {
	Trace    string
	Session  string
	Level    string // canonical name: "debug" / "info" / "warn" / "error"
	Since    time.Duration
	NoFollow bool
	JSON     bool
	NoBinary bool
	NoPlugin bool
	// WezTerm enables the wezterm GUI log as a third source, filtered to
	// lines containing WezTermFilter (default "wezsesh"). Off by default
	// because the GUI log is shared with every other wezterm plugin and
	// would drown out the binary + plugin streams without a filter.
	WezTerm       bool
	WezTermPath   string // explicit path; empty → auto-detect
	WezTermFilter string // case-insensitive substring; empty → "wezsesh"
}

// tailRecord is the normalised in-memory shape every reader emits. Time
// is the unified ordering key; Raw carries every field from the source
// JSON so --json output preserves the original schema (with a `_source`
// tag injected). Wezterm-source records synthesise Raw from the parsed
// time/level/module/msg quartet since wezterm's GUI log is plain text.
type tailRecord struct {
	Source          string         // "binary" / "plugin" / "wezterm"
	Time            time.Time      // unified ordering key
	Level           string         // canonical name
	Msg             string
	TraceID         string
	BinarySessionID string
	PluginSessionID string
	PaneID          string         // string for symmetry; plugin emits int
	Raw             map[string]any // every field; for --json output
}

// tailNow is a clock seam. Production keeps it pointed at time.Now;
// tests inject a fixed clock to drive --since deterministically.
var tailNow = time.Now

// tailColorize is a TTY seam. Production probes os.Stdout for
// ModeCharDevice; tests force colour off (or on) to assert format
// branches without poking at the FD.
var tailColorize = func() bool {
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// subcmdTail implements `wezsesh tail [flags]`. Per the existing
// subcommand pattern: top-level recover, parse flags, load config from
// the env, kick the readers, drain to stdout, return an exit code.
//
// Exit codes mirror doctor / list / find / reset (§8.20):
//   - 0  on a clean drain (--no-follow finished) or on SIGINT during
//     follow mode after a clean shutdown.
//   - 2  on flag / config / source-open failures.
func subcmdTail(rest []string, stdout, stderr io.Writer) (rc int) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(stderr, "wezsesh tail: panic: %v\n", r)
			rc = exitDoctorOrSubcmd
		}
	}()

	flags, err := parseTailFlags(rest)
	if err != nil {
		fmt.Fprintf(stderr, "wezsesh tail: %v\n", err)
		return exitDoctorOrSubcmd
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// SIGINT / SIGTERM: clean shutdown of the readers + emit goroutine.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				fmt.Fprintf(stderr, "wezsesh tail: signal goroutine panic: %v\n", r)
				cancel()
			}
		}()
		select {
		case <-sigCh:
			cancel()
		case <-ctx.Done():
		}
	}()

	cfg, err := config.LoadFromEnv(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "wezsesh tail: config: %v\n", err)
		return exitDoctorOrSubcmd
	}

	sources, err := buildTailSources(cfg, flags)
	if err != nil {
		fmt.Fprintf(stderr, "wezsesh tail: %v\n", err)
		return exitDoctorOrSubcmd
	}
	if len(sources) == 0 {
		fmt.Fprintf(stderr, "wezsesh tail: no sources enabled (--no-binary + --no-plugin?)\n")
		return exitDoctorOrSubcmd
	}

	return runTail(ctx, flags, sources, stdout, stderr)
}

// parseTailFlags parses the documented flag set and returns a tailFlags.
// Unknown levels / negative --since are rejected with a clear error so
// the user sees the typo instead of silently filtering everything out.
func parseTailFlags(rest []string) (tailFlags, error) {
	fs := flag.NewFlagSet("tail", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var p tailFlags
	var levelStr, sinceStr string
	fs.StringVar(&p.Trace, "trace", "", "filter by substring match on trace_id")
	fs.StringVar(&p.Session, "session", "", "filter by substring match on either session id")
	fs.StringVar(&levelStr, "level", "", "minimum level: debug|info|warn|error")
	fs.StringVar(&sinceStr, "since", "", "skip records older than duration (e.g. 5m, 1h)")
	fs.BoolVar(&p.NoFollow, "no-follow", false, "print existing matches and exit (default: follow)")
	fs.BoolVar(&p.JSON, "json", false, "emit raw JSON, one record per line, with _source injected")
	fs.BoolVar(&p.NoBinary, "no-binary", false, "skip <stateDir>/wezsesh.log")
	fs.BoolVar(&p.NoPlugin, "no-plugin", false, "skip <stateDir>/plugin.log")
	fs.BoolVar(&p.WezTerm, "wezterm", false,
		"include the wezterm GUI log filtered by --wezterm-filter (off by default)")
	fs.StringVar(&p.WezTermPath, "wezterm-log", "",
		"explicit path to a wezterm GUI log file (default: auto-detect newest under XDG locations)")
	fs.StringVar(&p.WezTermFilter, "wezterm-filter", "",
		"case-insensitive substring filter for --wezterm lines (default: 'wezsesh')")
	if err := fs.Parse(rest); err != nil {
		return tailFlags{}, err
	}
	if fs.NArg() != 0 {
		return tailFlags{}, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	if levelStr != "" {
		key := strings.ToLower(strings.TrimSpace(levelStr))
		if _, ok := tailLevelOrder[key]; !ok {
			return tailFlags{}, fmt.Errorf("unknown --level %q (want debug|info|warn|error)", levelStr)
		}
		p.Level = key
	}
	if sinceStr != "" {
		d, err := time.ParseDuration(sinceStr)
		if err != nil {
			return tailFlags{}, fmt.Errorf("--since %q: %w", sinceStr, err)
		}
		if d < 0 {
			return tailFlags{}, fmt.Errorf("--since %q: must be non-negative", sinceStr)
		}
		p.Since = d
	}
	return p, nil
}

// tailSource describes one open log file the runtime is following. The
// path is captured up front so rotation detection (open dev+inode vs.
// path dev+inode) has a stable referent. Wezterm-source records carry a
// substring `Filter` and the wezterm log's plain-text format means the
// reader synthesises a date anchor from the file's mtime (the GUI log
// only stamps `HH:MM:SS.mmm`, not the date).
type tailSource struct {
	Name   string // "binary" / "plugin" / "wezterm"
	Path   string
	Filter string // case-insensitive substring (wezterm only)
}

// buildTailSources resolves the two log file paths from the loaded
// config, honouring --no-binary / --no-plugin. A missing file is NOT a
// fatal error — we'll return the source and the reader will retry on
// the polling cadence (in follow mode) or simply emit nothing (in
// no-follow mode).
func buildTailSources(cfg *config.Config, flags tailFlags) ([]tailSource, error) {
	var out []tailSource
	if !flags.NoBinary {
		if cfg.StateDir == "" {
			return nil, errors.New("config: state_dir is empty")
		}
		out = append(out, tailSource{Name: "binary", Path: filepath.Join(cfg.StateDir, "wezsesh.log")})
	}
	if !flags.NoPlugin {
		if cfg.StateDir == "" {
			return nil, errors.New("config: state_dir is empty")
		}
		out = append(out, tailSource{Name: "plugin", Path: filepath.Join(cfg.StateDir, "plugin.log")})
	}
	if flags.WezTerm {
		path := flags.WezTermPath
		if path == "" {
			path = autoDetectWezTermLog()
		}
		if path == "" {
			return nil, errors.New("--wezterm: could not find a wezterm GUI log; pass --wezterm-log <path>")
		}
		filter := flags.WezTermFilter
		if filter == "" {
			filter = "wezsesh"
		}
		out = append(out, tailSource{
			Name: "wezterm", Path: path, Filter: strings.ToLower(filter),
		})
	}
	return out, nil
}

// wezTermLogCandidates is the list of directories `autoDetectWezTermLog`
// scans. Wezterm writes one `wezterm-gui-log-<pid>.txt` per GUI process
// invocation; we pick the most-recently-modified one, matching the
// running wezterm. Order is irrelevant — the mtime comparison decides.
//
// Tests substitute this slice via the package var so a scratch tmpdir
// drives auto-detection without touching the user's real `$HOME`.
var wezTermLogCandidates = func() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	return []string{
		// Linux + macOS — wezterm uses XDG_DATA_HOME on both.
		filepath.Join(home, ".local", "share", "wezterm"),
		// macOS Application Support — observed on some installs.
		filepath.Join(home, "Library", "Application Support", "wezterm"),
		// Cache directory fallback.
		filepath.Join(home, ".cache", "wezterm"),
	}
}

// autoDetectWezTermLog returns the absolute path of the newest
// `wezterm-gui-log-*.txt` file under any of the platform candidate
// directories, or "" if nothing is found. Caller maps "" to a clear
// error so the user knows to pass --wezterm-log explicitly.
func autoDetectWezTermLog() string {
	var best string
	var bestMtime time.Time
	for _, dir := range wezTermLogCandidates() {
		matches, err := filepath.Glob(filepath.Join(dir, "wezterm-gui-log-*.txt"))
		if err != nil {
			continue
		}
		for _, m := range matches {
			fi, err := os.Stat(m)
			if err != nil {
				continue
			}
			if fi.ModTime().After(bestMtime) {
				best = m
				bestMtime = fi.ModTime()
			}
		}
	}
	return best
}

// runTail wires the reader goroutines + the buffered emit loop. In
// follow mode the function blocks until the context is cancelled (e.g.
// SIGINT). In no-follow mode the readers drain to EOF, the emit loop
// sorts the entire buffered window, and the function returns.
func runTail(ctx context.Context, flags tailFlags, sources []tailSource, stdout, stderr io.Writer) int {
	ch := make(chan tailRecord, 256)
	var wg sync.WaitGroup
	for _, src := range sources {
		src := src
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					fmt.Fprintf(stderr, "wezsesh tail: reader %q panic: %v\n", src.Name, r)
				}
			}()
			tailReader(ctx, src, flags.NoFollow, ch, stderr)
		}()
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				fmt.Fprintf(stderr, "wezsesh tail: closer panic: %v\n", r)
			}
		}()
		wg.Wait()
		close(ch)
	}()

	useColor := !flags.JSON && tailColorize()

	if flags.NoFollow {
		// Drain everything, then sort, then emit. The window is the entire
		// drained set — bounded by the file sizes (1 MiB caps), so the
		// O(n log n) sort is cheap.
		var all []tailRecord
		for rec := range ch {
			if !tailRecordMatches(rec, flags) {
				continue
			}
			all = append(all, rec)
		}
		sort.SliceStable(all, func(i, j int) bool { return all[i].Time.Before(all[j].Time) })
		for _, rec := range all {
			emitTailRecord(stdout, rec, flags.JSON, useColor)
		}
		return exitOK
	}

	// Follow mode: small ordered window. Buffer up to maxBufferedRecords
	// or maxBufferedDuration (whichever fires first), then flush the
	// oldest record. The window absorbs cross-source clock skew without
	// holding the stream up indefinitely.
	const (
		maxBufferedRecords  = 100
		maxBufferedDuration = 200 * time.Millisecond
	)
	var buf []tailRecord
	flushOldest := func() {
		if len(buf) == 0 {
			return
		}
		sort.SliceStable(buf, func(i, j int) bool { return buf[i].Time.Before(buf[j].Time) })
		emitTailRecord(stdout, buf[0], flags.JSON, useColor)
		buf = buf[1:]
	}
	flushAll := func() {
		sort.SliceStable(buf, func(i, j int) bool { return buf[i].Time.Before(buf[j].Time) })
		for _, rec := range buf {
			emitTailRecord(stdout, rec, flags.JSON, useColor)
		}
		buf = buf[:0]
	}
	timer := time.NewTimer(maxBufferedDuration)
	defer timer.Stop()
	resetTimer := func() {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(maxBufferedDuration)
	}
	for {
		select {
		case <-ctx.Done():
			flushAll()
			return exitOK
		case rec, ok := <-ch:
			if !ok {
				flushAll()
				return exitOK
			}
			if !tailRecordMatches(rec, flags) {
				continue
			}
			buf = append(buf, rec)
			if len(buf) >= maxBufferedRecords {
				flushOldest()
			}
			resetTimer()
		case <-timer.C:
			flushOldest()
			resetTimer()
		}
	}
}

// tailRecordMatches applies the four filter dimensions (trace, session,
// level, since) to a single normalised record. Empty filter dimensions
// are no-ops, so the default invocation of `wezsesh tail` matches every
// record on both sources.
func tailRecordMatches(rec tailRecord, flags tailFlags) bool {
	if flags.Trace != "" && !strings.Contains(rec.TraceID, flags.Trace) {
		return false
	}
	if flags.Session != "" {
		if !strings.Contains(rec.BinarySessionID, flags.Session) &&
			!strings.Contains(rec.PluginSessionID, flags.Session) {
			return false
		}
	}
	if flags.Level != "" {
		min, ok := tailLevelOrder[flags.Level]
		if !ok {
			min = 0
		}
		got, gotOK := tailLevelOrder[strings.ToLower(rec.Level)]
		if !gotOK {
			// Unknown level on the record side — treat as the most
			// permissive bucket (info) so the record is still surfaced
			// when the user asked for warn / error and didn't, etc. This
			// is a defensive default; real records always carry one of
			// the four canonical names.
			got = 1
		}
		if got < min {
			return false
		}
	}
	if flags.Since > 0 {
		cutoff := tailNow().Add(-flags.Since)
		if rec.Time.Before(cutoff) {
			return false
		}
	}
	return true
}

// ──────────────────────────────────────────────────────────────────────
// Reader
// ──────────────────────────────────────────────────────────────────────

// tailReader follows one source. Strategy: open the path, read lines
// until EOF, parse each into a tailRecord, and emit on `out`. In follow
// mode after EOF, sleep briefly, check for rotation (dev+inode change),
// then re-read. Closes its own file and returns when ctx is cancelled.
//
// Errors are logged to stderr (best-effort) and do not propagate; the
// reader keeps trying on the next poll tick. A missing file at startup
// is not a hard error in follow mode (we'll keep watching the path);
// in no-follow mode it returns immediately with a stderr note.
func tailReader(ctx context.Context, src tailSource, noFollow bool, out chan<- tailRecord, stderr io.Writer) {
	const pollInterval = 50 * time.Millisecond

	var (
		f       *os.File
		reader  *bufio.Reader
		curDev  uint64
		curIno  uint64
		hasFile bool
		// dateAnchor is the calendar date at which time-of-day-only
		// timestamps (wezterm's `HH:MM:SS.mmm` prefix) are anchored.
		// Captured from the file's mtime each time openFile succeeds so
		// the wezterm source orders sensibly against the JSON-timestamped
		// binary + plugin streams. Zero for sources that don't need it.
		dateAnchor time.Time
		// Carry-over for partial trailing line across reads. bufio.Reader
		// returns io.EOF mid-line if the writer hasn't flushed the
		// newline yet; we stash the partial fragment and prepend on the
		// next attempt rather than parsing a half-formed JSON object.
		partial []byte
	)
	defer func() {
		if f != nil {
			_ = f.Close()
		}
	}()

	openFile := func() bool {
		fh, err := os.Open(src.Path)
		if err != nil {
			if noFollow && errors.Is(err, os.ErrNotExist) {
				// Quietly skip: the file may simply not exist yet on a
				// fresh install. Follow-mode handles this by retrying.
				return false
			}
			if !errors.Is(err, os.ErrNotExist) {
				fmt.Fprintf(stderr, "wezsesh tail: open %s: %v\n", src.Path, err)
			}
			return false
		}
		dev, ino, ok := statDevIno(fh)
		if !ok {
			_ = fh.Close()
			return false
		}
		// File mtime → date anchor. Best-effort; on Stat failure fall
		// back to "now" so the wezterm source still produces orderable
		// records (just less correct relative to the binary/plugin
		// streams when the user is tailing an old wezterm log).
		if fi, err := fh.Stat(); err == nil {
			dateAnchor = fi.ModTime()
		} else {
			dateAnchor = tailNow()
		}
		f = fh
		reader = bufio.NewReader(fh)
		curDev = dev
		curIno = ino
		hasFile = true
		partial = partial[:0]
		return true
	}

	closeFile := func() {
		if f != nil {
			_ = f.Close()
			f = nil
			reader = nil
		}
		hasFile = false
	}

	rotated := func() bool {
		if !hasFile {
			return false
		}
		dev, ino, ok := pathDevIno(src.Path)
		if !ok {
			// Path gone → treat as rotation; the next openFile will
			// either succeed (fresh file) or fail (still gone).
			return true
		}
		return dev != curDev || ino != curIno
	}

	openFile()

	for {
		// ctx-cancelled exits cleanly even mid-poll.
		select {
		case <-ctx.Done():
			return
		default:
		}

		if !hasFile {
			if !openFile() {
				if noFollow {
					return
				}
				if !sleepWithCtx(ctx, pollInterval) {
					return
				}
				continue
			}
		}

		// Read up to EOF. Each ReadBytes('\n') returns either a full line
		// (no error) or a partial trailing fragment + io.EOF.
		for {
			line, err := reader.ReadBytes('\n')
			if len(line) > 0 {
				if len(partial) > 0 {
					line = append(append([]byte{}, partial...), line...)
					partial = partial[:0]
				}
				if err == io.EOF {
					// Stash the no-newline trailing fragment and break;
					// we'll prepend on the next read.
					partial = append(partial[:0], line...)
					break
				}
				// Strip trailing newline before parsing.
				trim := line
				if n := len(trim); n > 0 && trim[n-1] == '\n' {
					trim = trim[:n-1]
				}
				if len(trim) == 0 {
					continue
				}
				rec, ok := parseTailLine(src, trim, dateAnchor)
				if !ok {
					// Malformed line — skip silently. The Lua side could
					// in principle emit a partial write under crash; we
					// defensively drop rather than panic. For the wezterm
					// source this is also where the substring filter
					// rejects unrelated lines.
					continue
				}
				select {
				case <-ctx.Done():
					return
				case out <- rec:
				}
				continue
			}
			if err != nil {
				if err != io.EOF {
					fmt.Fprintf(stderr, "wezsesh tail: read %s: %v\n", src.Path, err)
				}
				break
			}
		}

		if noFollow {
			return
		}

		if !sleepWithCtx(ctx, pollInterval) {
			return
		}

		if rotated() {
			closeFile()
			// Loop back; openFile fires next iteration. The new file may
			// not exist yet (rename-then-create race); retry on the
			// poll cadence.
		}
	}
}

// sleepWithCtx is a context-cancellable sleep. Returns false iff the
// context fired during the sleep, signalling the caller to return.
func sleepWithCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// statDevIno extracts (dev, ino) from an open *os.File. Returns ok=false
// on a non-Unix Stat shape (which we don't expect to see — wezsesh is
// linux/darwin only — but the type assertion guards a panic on a
// surprise host).
func statDevIno(f *os.File) (dev, ino uint64, ok bool) {
	fi, err := f.Stat()
	if err != nil {
		return 0, 0, false
	}
	st, ok2 := fi.Sys().(*syscall.Stat_t)
	if !ok2 || st == nil {
		return 0, 0, false
	}
	return uint64(st.Dev), uint64(st.Ino), true
}

// pathDevIno is the path-based variant. Used for rotation detection:
// the open file's (dev, ino) is compared against the path's (dev, ino)
// every poll tick. A mismatch means the path now points to a different
// inode (rename + create, or unlink + recreate).
func pathDevIno(path string) (dev, ino uint64, ok bool) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, 0, false
	}
	st, ok2 := fi.Sys().(*syscall.Stat_t)
	if !ok2 || st == nil {
		return 0, 0, false
	}
	return uint64(st.Dev), uint64(st.Ino), true
}

// ──────────────────────────────────────────────────────────────────────
// Parsing
// ──────────────────────────────────────────────────────────────────────

// parseTailLine decodes one JSON line into a tailRecord. The two source
// schemas differ on timestamp and on which fields are guaranteed
// present, so the source name selects the timestamp extraction strategy.
//
// Returns ok=false on malformed JSON / an unparseable timestamp / a
// wezterm line that fails the substring filter. The caller (tailReader)
// silently drops on ok=false.
//
// dateAnchor is consulted only by the "wezterm" source (its log lines
// stamp `HH:MM:SS.mmm` without a calendar date); other sources ignore it.
func parseTailLine(src tailSource, line []byte, dateAnchor time.Time) (tailRecord, bool) {
	if src.Name == "wezterm" {
		return parseWezTermLine(line, src.Filter, dateAnchor)
	}
	var raw map[string]any
	if err := json.Unmarshal(line, &raw); err != nil {
		return tailRecord{}, false
	}
	rec := tailRecord{
		Source: src.Name,
		Raw:    raw,
	}
	switch src.Name {
	case "binary":
		// Go-side slog JSON. `time` is RFC3339 with sub-second precision.
		if ts, ok := raw["time"].(string); ok {
			t, err := time.Parse(time.RFC3339Nano, ts)
			if err != nil {
				// slog's default format is RFC3339Nano, but tolerate
				// RFC3339 (no nanos) as a fallback.
				t, err = time.Parse(time.RFC3339, ts)
				if err != nil {
					return tailRecord{}, false
				}
			}
			rec.Time = t
		} else {
			return tailRecord{}, false
		}
	case "plugin":
		// Lua-side JSON. `ts` is unix seconds (integer). Tolerate either
		// a JSON number (float64 after Unmarshal) or a string of digits.
		if v, ok := raw["ts"]; ok {
			switch t := v.(type) {
			case float64:
				rec.Time = time.Unix(int64(t), 0)
			case string:
				n, err := strconv.ParseInt(strings.TrimSpace(t), 10, 64)
				if err != nil {
					return tailRecord{}, false
				}
				rec.Time = time.Unix(n, 0)
			default:
				return tailRecord{}, false
			}
		} else {
			return tailRecord{}, false
		}
	default:
		return tailRecord{}, false
	}

	rec.Level = stringField(raw, "level")
	rec.Msg = stringField(raw, "msg")
	rec.TraceID = stringField(raw, "trace_id")
	rec.BinarySessionID = stringField(raw, "binary_session_id")
	rec.PluginSessionID = stringField(raw, "plugin_session_id")
	// pane_id is plugin-side and emitted as int; stringify for symmetry
	// with the binary surface (which doesn't have a pane id field).
	if v, ok := raw["pane_id"]; ok {
		switch t := v.(type) {
		case float64:
			rec.PaneID = strconv.FormatInt(int64(t), 10)
		case string:
			rec.PaneID = t
		}
	}
	return rec, true
}

// wezTermLineRE matches one line of wezterm's GUI log:
//
//	HH:MM:SS.mmm  LEVEL  module > body
//
// The body capture is greedy and may itself contain `>` characters. The
// timestamp lacks a calendar date (wezterm anchors to "today" in the
// process's local TZ); the caller combines it with the file's mtime
// date so the record orders sensibly against the JSON-timestamped
// binary + plugin streams.
var wezTermLineRE = regexp.MustCompile(
	`^(\d{2}):(\d{2}):(\d{2})\.(\d+)\s+(\S+)\s+(\S+)\s*>\s*(.*)$`)

// parseWezTermLine extracts the time-of-day prefix, level, module, and
// message body from a wezterm GUI log line and applies a case-
// insensitive substring filter. Lines that don't match the regex OR
// don't contain `filter` are dropped (ok=false). `dateAnchor` provides
// the calendar date the time-of-day attaches to.
//
// `filter` MUST already be lower-cased by the caller; we lower-case
// the line once for the contains check and skip lower-casing the
// filter on every record.
func parseWezTermLine(line []byte, filter string, dateAnchor time.Time) (tailRecord, bool) {
	if filter != "" && !bytes.Contains(bytes.ToLower(line), []byte(filter)) {
		return tailRecord{}, false
	}
	m := wezTermLineRE.FindSubmatch(line)
	if m == nil {
		// Continuation lines / non-standard prefix. Drop — the surrounding
		// lines that DO match carry the diagnostic content.
		return tailRecord{}, false
	}
	hh, _ := strconv.Atoi(string(m[1]))
	mm, _ := strconv.Atoi(string(m[2]))
	ss, _ := strconv.Atoi(string(m[3]))
	frac := string(m[4])
	// Pad/truncate the fractional part to 9 digits for time.Date's nanos.
	for len(frac) < 9 {
		frac += "0"
	}
	if len(frac) > 9 {
		frac = frac[:9]
	}
	ns, _ := strconv.Atoi(frac)
	anchor := dateAnchor
	if anchor.IsZero() {
		anchor = tailNow()
	}
	t := time.Date(anchor.Year(), anchor.Month(), anchor.Day(),
		hh, mm, ss, ns, anchor.Location())
	level := strings.ToLower(string(m[5]))
	module := string(m[6])
	body := string(m[7])
	return tailRecord{
		Source: "wezterm",
		Time:   t,
		Level:  level,
		Msg:    body,
		Raw: map[string]any{
			"ts":     t.Unix(),
			"level":  level,
			"module": module,
			"msg":    body,
		},
	}, true
}

// stringField fetches a top-level string field from the parsed JSON
// map. Non-string / missing values yield "".
func stringField(raw map[string]any, key string) string {
	if v, ok := raw[key]; ok {
		if s, ok2 := v.(string); ok2 {
			return s
		}
	}
	return ""
}

// ──────────────────────────────────────────────────────────────────────
// Output
// ──────────────────────────────────────────────────────────────────────

// ANSI colour codes for the default text output. Suppressed when stdout
// is not a TTY or when --json is set.
const (
	ansiReset   = "\x1b[0m"
	ansiCyan    = "\x1b[36m"
	ansiMagenta = "\x1b[35m"
	ansiYellow  = "\x1b[33m"
)

// emitTailRecord writes one record to stdout in the requested format.
// JSON: re-encode raw + injected `_source`. Text: tag, time, level,
// trace/session chips, then msg + trailing kv pairs.
func emitTailRecord(w io.Writer, rec tailRecord, asJSON, color bool) {
	if asJSON {
		// Inject _source into the raw map. We mutate in place: the
		// record is consumed once and discarded.
		if rec.Raw == nil {
			rec.Raw = map[string]any{}
		}
		rec.Raw["_source"] = rec.Source
		buf, err := json.Marshal(rec.Raw)
		if err != nil {
			return
		}
		_, _ = w.Write(buf)
		_, _ = w.Write([]byte{'\n'})
		return
	}

	var b strings.Builder
	tag := tailSourceTag(rec.Source)
	if color {
		b.WriteString(tailSourceColor(rec.Source))
	}
	b.WriteByte('[')
	b.WriteString(tag)
	b.WriteByte(' ')
	b.WriteString(rec.Time.Local().Format("15:04:05.000"))
	b.WriteByte(' ')
	b.WriteString(tailLevelLetter(rec.Level))
	b.WriteByte(']')
	if color {
		b.WriteString(ansiReset)
	}
	b.WriteByte(' ')
	if rec.TraceID != "" {
		b.WriteString("[t:")
		b.WriteString(tailTruncID(rec.TraceID))
		b.WriteString("] ")
	}
	if rec.BinarySessionID != "" || rec.PluginSessionID != "" {
		b.WriteByte('[')
		if rec.BinarySessionID != "" {
			b.WriteString("bs:")
			b.WriteString(tailTruncID(rec.BinarySessionID))
		}
		if rec.PluginSessionID != "" {
			if rec.BinarySessionID != "" {
				b.WriteByte('|')
			}
			b.WriteString("ps:")
			b.WriteString(tailTruncID(rec.PluginSessionID))
		}
		b.WriteString("] ")
	}
	b.WriteString(rec.Msg)

	// Trailing kv: every Raw field that isn't one of the structural
	// names already shown. Sorted by key for deterministic output.
	skip := map[string]struct{}{
		"time":              {},
		"ts":                {},
		"level":             {},
		"msg":               {},
		"trace_id":          {},
		"binary_session_id": {},
		"plugin_session_id": {},
		"_source":           {},
	}
	keys := make([]string, 0, len(rec.Raw))
	for k := range rec.Raw {
		if _, drop := skip[k]; drop {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		b.WriteByte(' ')
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(formatKVValue(rec.Raw[k]))
	}
	b.WriteByte('\n')
	_, _ = io.WriteString(w, b.String())
}

// tailSourceTag is the short label printed in the leading [tag …] chip.
func tailSourceTag(source string) string {
	switch source {
	case "binary":
		return "bin"
	case "plugin":
		return "lua"
	case "wezterm":
		return "wzt"
	}
	return source
}

// tailSourceColor returns the ANSI colour escape for the source. Empty
// for unknown sources; the text path then emits without colour.
func tailSourceColor(source string) string {
	switch source {
	case "binary":
		return ansiCyan
	case "plugin":
		return ansiMagenta
	case "wezterm":
		return ansiYellow
	}
	return ""
}

// tailLevelLetter compresses the canonical level name to a single
// uppercase letter so the leading chip stays narrow when scanning
// output columns.
func tailLevelLetter(level string) string {
	switch strings.ToLower(level) {
	case "debug":
		return "D"
	case "info":
		return "I"
	case "warn", "warning":
		return "W"
	case "error":
		return "E"
	}
	return "?"
}

// tailTruncID returns the last 8 chars of the id (or the full id if
// shorter). The tail of a ULID is the more entropy-dense end, so 8
// trailing chars give a high-uniqueness preview while keeping the
// columns narrow.
func tailTruncID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[len(id)-8:]
}

// formatKVValue renders an arbitrary JSON value as a single token for
// the trailing key=value section. Strings render naked (no quotes) when
// they contain no whitespace; otherwise they're JSON-quoted. Numbers
// and booleans render via their Go default. Maps and slices fall
// through to a JSON re-encode so the user sees the structure.
func formatKVValue(v any) string {
	switch t := v.(type) {
	case string:
		if strings.ContainsAny(t, " \t\"\n") {
			b, _ := json.Marshal(t)
			return string(b)
		}
		return t
	case float64:
		// JSON numbers come back as float64. Render integers without
		// the trailing ".0" decoration so kv pairs read naturally.
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		if t {
			return "true"
		}
		return "false"
	case nil:
		return "null"
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return fmt.Sprint(t)
		}
		return string(b)
	}
}
