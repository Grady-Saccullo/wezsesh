// Package logger provides the binary-side structured logger backing every
// wezsesh subcommand. It implements §8.18: a slog JSON handler over a
// rotating writer that keeps Debug/Info line-buffered (1 s tick flush) and
// flushes Warn/Error synchronously, so a crash within the 1 s window cannot
// lose the diagnostic line that explains the crash itself (§17.3 row
// "Logger Warn/Error sync flush").
package logger

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Level mirrors slog.Level at the four wezsesh-recognised tiers. The
// numeric ordering (Debug < Info < Warn < Error) matches slog so a Level
// is interchangeable with slog.Level via levelToSlog.
type Level int

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
)

// File-system policy for the rotating log writer.
const (
	// rotateThreshold is the size (in bytes) at which the active log is
	// rotated. §8.18 specifies 1 MiB.
	rotateThreshold int64 = 1 << 20

	// flushInterval is the periodic flush cadence for the bufio writer
	// when only Debug/Info traffic is flowing. §8.18.
	flushInterval = time.Second

	// logFileMode matches §12.1 row "$XDG_STATE_HOME/wezsesh/wezsesh.log".
	logFileMode os.FileMode = 0o600

	// stateDirMode matches §12.1 "state dir parent".
	stateDirMode os.FileMode = 0o700

	// logFilename and the rotated suffix template.
	logFilename = "wezsesh.log"
)

// Logger is the structured logger handed to every subsystem. Construct via
// New; tear down via Close. Method calls are safe for concurrent use.
//
// All four log methods accept a printf-free `kv ...any` slice that is
// forwarded to slog as alternating key/value pairs (the slog convention).
//
// Caller responsibility: any user-controlled string passed as a value MUST
// be sanitised before the call. The project-wide helper for that is
// internal/nameval.SanitizeForDisplay; the logger does not reach across
// package boundaries to apply it.
type Logger struct {
	level   Level
	handler slog.Handler
	writer  *rotatingWriter
	slog    *slog.Logger

	closeOnce sync.Once
	closeErr  error
}

// New opens (or creates) <stateDir>/wezsesh.log and returns a Logger
// preconfigured with the slog JSON handler. The state directory itself is
// created with 0700 if missing. Both the directory and the log file are
// guarded against symlinks via the inline os.Lstat check (see
// "Accepted findings" — defers to safefs.Enforce when T-101 lands).
func New(stateDir string, level Level) (*Logger, error) {
	if stateDir == "" {
		return nil, errors.New("logger: stateDir is empty")
	}
	if err := ensureDir(stateDir); err != nil {
		return nil, err
	}
	w, err := newRotatingWriter(filepath.Join(stateDir, logFilename))
	if err != nil {
		return nil, err
	}
	h := slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level: levelToSlog(level),
	})
	return &Logger{
		level:   level,
		handler: h,
		writer:  w,
		slog:    slog.New(h),
	}, nil
}

// Debug emits at LevelDebug. Sanitize user-controlled strings before
// passing — internal/nameval.SanitizeForDisplay is the project-wide helper.
func (l *Logger) Debug(msg string, kv ...any) {
	if l == nil {
		return
	}
	l.slog.Debug(msg, kv...)
}

// Info emits at LevelInfo. Buffered until the 1 s tick or a Warn/Error
// forces a flush. Sanitize user-controlled strings before passing —
// internal/nameval.SanitizeForDisplay is the project-wide helper.
func (l *Logger) Info(msg string, kv ...any) {
	if l == nil {
		return
	}
	l.slog.Info(msg, kv...)
}

// Warn emits at LevelWarn and synchronously flushes to disk so the line
// survives a process crash within the next 1 s tick window
// (§17.3 row "Logger Warn/Error sync flush"). Sanitize user-controlled
// strings before passing — internal/nameval.SanitizeForDisplay is the
// project-wide helper.
func (l *Logger) Warn(msg string, kv ...any) {
	if l == nil {
		return
	}
	l.slog.Warn(msg, kv...)
	l.writer.syncFlush()
}

// Error emits at LevelError and synchronously flushes to disk
// (§17.3 row "Logger Warn/Error sync flush"). Sanitize user-controlled
// strings before passing — internal/nameval.SanitizeForDisplay is the
// project-wide helper.
func (l *Logger) Error(msg string, kv ...any) {
	if l == nil {
		return
	}
	l.slog.Error(msg, kv...)
	l.writer.syncFlush()
}

// Close stops the background flush goroutine, drains the buffer, fsyncs,
// and closes the underlying file. Idempotent.
func (l *Logger) Close() error {
	if l == nil {
		return nil
	}
	l.closeOnce.Do(func() {
		l.closeErr = l.writer.Close()
	})
	return l.closeErr
}

// ResolveLevel picks the more verbose (lower-numeric) of the two named
// levels. Unknown / empty inputs are treated as "no preference" at that
// slot; if both are unrecognised the default is LevelInfo.
//
// NOTE: §11.4 prose calls this ResolveLevel(envLevel, optsLevel), but the
// §8.18 declaration ResolveLevel(optsLevel, envLevel) is the authoritative
// API contract — argument order matches §8.18.
func ResolveLevel(optsLevel string, envLevel string) Level {
	optsParsed, optsOK := parseLevel(optsLevel)
	envParsed, envOK := parseLevel(envLevel)
	switch {
	case optsOK && envOK:
		if envParsed < optsParsed {
			return envParsed
		}
		return optsParsed
	case optsOK:
		return optsParsed
	case envOK:
		return envParsed
	default:
		return LevelInfo
	}
}

// parseLevel accepts the four canonical names case-insensitively. Any other
// input (including the empty string) returns ok=false so ResolveLevel can
// treat it as "no preference at this slot".
func parseLevel(s string) (Level, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return LevelDebug, true
	case "info":
		return LevelInfo, true
	case "warn", "warning":
		return LevelWarn, true
	case "error":
		return LevelError, true
	}
	return 0, false
}

func levelToSlog(l Level) slog.Level {
	switch l {
	case LevelDebug:
		return slog.LevelDebug
	case LevelInfo:
		return slog.LevelInfo
	case LevelWarn:
		return slog.LevelWarn
	case LevelError:
		return slog.LevelError
	}
	return slog.LevelInfo
}

// ensureDir mkdir-p's the state directory, refusing to traverse a symlink
// at the leaf. The fail-CLOSED inline check substitutes for safefs.Enforce
// until T-101 lands (see "Accepted findings").
func ensureDir(dir string) error {
	if err := os.MkdirAll(dir, stateDirMode); err != nil {
		return fmt.Errorf("logger: mkdir state dir: %w", err)
	}
	if err := refuseSymlink(dir); err != nil {
		return err
	}
	return nil
}

// refuseSymlink returns an error if `path` exists and is a symbolic link.
// Missing paths are treated as ok (the caller will create them next).
// This is the inline stand-in for safefs.Enforce(SymlinkRefuse) that this
// task uses to avoid taking a depends-on T-101.
func refuseSymlink(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("logger: lstat %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("logger: refusing to use symlink at %s", path)
	}
	return nil
}

// rotatingWriter is the io.Writer slog writes through. It owns the active
// log file, a bufio.Writer in front of it (so Info records do NOT reach
// the kernel page cache until either the 1 s tick fires or a Warn/Error
// forces a flush — that's the §8.18 buffering tradeoff that keeps a
// crash-after-Info from polluting the on-disk record while still letting
// crash-after-Warn preserve the explanatory line), the rotation counter,
// and the periodic-flush goroutine.
type rotatingWriter struct {
	path string

	mu      sync.Mutex
	file    *os.File
	buf     *bufio.Writer
	written int64
	closed  bool

	tickerStop chan struct{}
	tickerDone chan struct{}
}

func newRotatingWriter(path string) (*rotatingWriter, error) {
	if err := refuseSymlink(path); err != nil {
		return nil, err
	}
	f, err := openLog(path)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("logger: stat log: %w", err)
	}
	w := &rotatingWriter{
		path:       path,
		file:       f,
		buf:        bufio.NewWriter(f),
		written:    st.Size(),
		tickerStop: make(chan struct{}),
		tickerDone: make(chan struct{}),
	}
	go w.tickLoop()
	return w, nil
}

// openLog opens the log file in append mode, creating with 0600. The
// caller MUST have already called refuseSymlink on `path`.
func openLog(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, logFileMode)
	if err != nil {
		return nil, fmt.Errorf("logger: open log: %w", err)
	}
	return f, nil
}

// Write satisfies io.Writer. Records flow into the bufio buffer; rotation
// fires when the cumulative on-disk-or-buffered size crosses the
// threshold.
func (w *rotatingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return 0, errors.New("logger: write after close")
	}
	if w.written+int64(len(p)) > rotateThreshold && w.written > 0 {
		if err := w.rotateLocked(); err != nil {
			// WHY: a rotation failure mid-write is best-effort per §8.18
			// (rotation is silent because the only side-channel for
			// reporting it would be the very logger that failed). Continue
			// writing to the current file so callers don't lose telemetry.
			_ = err
		}
	}
	n, err := w.buf.Write(p)
	w.written += int64(n)
	return n, err
}

// syncFlush is called by Warn/Error after the slog handler has emitted the
// record AND by the 1 s tick. It flushes the userspace bufio buffer into
// the file then fsyncs so the kernel pushes dirty pages to disk; the line
// survives a crash. Errors are swallowed (best-effort, same rationale as
// Write's rotation failure).
func (w *rotatingWriter) syncFlush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed || w.file == nil {
		return
	}
	_ = w.buf.Flush()
	_ = w.file.Sync()
}

// tickLoop runs in its own goroutine; on each tick it calls fsync so
// Debug/Info records that were written more than `flushInterval` ago are
// pushed to disk. Cancellable via Close.
func (w *rotatingWriter) tickLoop() {
	defer close(w.tickerDone)
	t := time.NewTicker(flushInterval)
	defer t.Stop()
	for {
		select {
		case <-w.tickerStop:
			return
		case <-t.C:
			w.syncFlush()
		}
	}
}

// rotateLocked performs the .1→.2→.3 shift then renames the active log to
// .1, finally re-opening a fresh active log. Caller must hold w.mu.
//
// Symlink defense: each numbered target path is Lstat'd; if any is a
// symlink, rotation aborts (fail-CLOSED). This is the inline stand-in for
// safefs.Enforce(SymlinkRefuse).
func (w *rotatingWriter) rotateLocked() error {
	// Numbered targets, oldest-first, so we can drop .3 then shift .2→.3,
	// .1→.2, active→.1 without overwriting a target prematurely.
	for i := 3; i >= 1; i-- {
		dst := fmt.Sprintf("%s.%d", w.path, i)
		if err := refuseSymlink(dst); err != nil {
			return err
		}
		if i == 3 {
			// Drop the oldest if present.
			if err := os.Remove(dst); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("logger: drop oldest rotated: %w", err)
			}
			continue
		}
		src := fmt.Sprintf("%s.%d", w.path, i)
		nextDst := fmt.Sprintf("%s.%d", w.path, i+1)
		if _, err := os.Lstat(src); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return fmt.Errorf("logger: lstat %s: %w", src, err)
		}
		if err := os.Rename(src, nextDst); err != nil {
			return fmt.Errorf("logger: rotate %s -> %s: %w", src, nextDst, err)
		}
	}
	// Move active to .1.
	if err := refuseSymlink(w.path); err != nil {
		return err
	}
	if err := w.buf.Flush(); err != nil {
		return fmt.Errorf("logger: flush before rotate: %w", err)
	}
	if err := w.file.Sync(); err != nil {
		return fmt.Errorf("logger: sync before rotate: %w", err)
	}
	if err := w.file.Close(); err != nil {
		return fmt.Errorf("logger: close before rotate: %w", err)
	}
	w.file = nil
	w.buf = nil
	if err := os.Rename(w.path, w.path+".1"); err != nil {
		return fmt.Errorf("logger: rotate active: %w", err)
	}
	f, err := openLog(w.path)
	if err != nil {
		return err
	}
	w.file = f
	w.buf = bufio.NewWriter(f)
	w.written = 0
	return nil
}

// Close cancels the tick goroutine, fsyncs, and closes the file. Calling
// Close concurrently with a Write is safe: the tick goroutine exit waits
// on its own channel, and the file is closed under the mutex.
func (w *rotatingWriter) Close() error {
	close(w.tickerStop)
	<-w.tickerDone
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.closed = true
	if w.file == nil {
		return nil
	}
	flushErr := w.buf.Flush()
	syncErr := w.file.Sync()
	closeErr := w.file.Close()
	w.file = nil
	w.buf = nil
	if flushErr != nil {
		return flushErr
	}
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

// Compile-time interface check: rotatingWriter is the io.Writer slog uses.
var _ io.Writer = (*rotatingWriter)(nil)
