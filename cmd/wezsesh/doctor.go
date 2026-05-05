package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/Grady-Saccullo/wezsesh/internal/config"
	"github.com/Grady-Saccullo/wezsesh/internal/doctor"
)

// doctorFormatText / doctorFormatJSON are the two values accepted by
// `wezsesh doctor --format`. Default is text.
const (
	doctorFormatText = "text"
	doctorFormatJSON = "json"
)

// subcmdDoctor implements `wezsesh doctor [--format text|json]` (§8.20).
// Per §8.20.1 step 3, doctor runs in the "config.LoadFromEnv (falls back
// to AutoDetect); no listener" group: load config, run every check in
// internal/doctor, render, exit. Exit-code rule (§8.20): 0 iff all
// checks pass; otherwise exitDoctorOrSubcmd (2). "All checks pass" means
// !report.Critical && !report.Warnings (§8.17).
//
// §13.14: top-level recover mirrors keygen / list / trust / reset. rc on
// panic is exitDoctorOrSubcmd (2). The doctor path holds no reply socket.
func subcmdDoctor(rest []string, stdout, stderr io.Writer) (rc int) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(stderr, "wezsesh doctor: panic: %v\n", r)
			rc = exitDoctorOrSubcmd
		}
	}()

	format, err := parseDoctorFlags(rest)
	if err != nil {
		fmt.Fprintf(stderr, "wezsesh doctor: %v\n", err)
		return exitDoctorOrSubcmd
	}

	ctx := context.Background()

	// Doctor is a diagnostic; if config can't load the user wants to
	// know. Bail with a clear stderr line + non-zero rc rather than
	// papering over with a synthetic empty Env (which would still report
	// every fs check as fail with an unhelpful "empty path" message).
	cfg, err := config.LoadFromEnv(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "wezsesh doctor: config: %v\n", err)
		return exitDoctorOrSubcmd
	}

	env := buildDoctorEnv(cfg)
	report := doctor.Run(ctx, env)

	if err := renderDoctorReport(stdout, format, report); err != nil {
		fmt.Fprintf(stderr, "wezsesh doctor: render: %v\n", err)
		return exitDoctorOrSubcmd
	}

	if report.Critical || report.Warnings {
		return exitDoctorOrSubcmd
	}
	return exitOK
}

// parseDoctorFlags accepts --format. Trailing positional args are
// rejected (§8.20 lists no positional args for `doctor`).
func parseDoctorFlags(rest []string) (string, error) {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	format := fs.String("format", doctorFormatText, "output format: text|json")
	if err := fs.Parse(rest); err != nil {
		return "", err
	}
	if fs.NArg() != 0 {
		return "", fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	switch *format {
	case doctorFormatText, doctorFormatJSON:
		return *format, nil
	default:
		return "", fmt.Errorf("unknown --format %q (want text|json)", *format)
	}
}

// buildDoctorEnv assembles the doctor.Env from a loaded config. The
// binary path is the binary's own path (os.Executable()) so the
// binary.path check has something to stat; on platforms where
// os.Executable fails (e.g., a stripped /proc/self/exe on a hardened
// host) we fall back to os.Args[0] — the doctor's binary.path check
// will fail loudly if neither resolves to a real file, which is the
// correct outcome.
func buildDoctorEnv(cfg *config.Config) doctor.Env {
	exe, err := os.Executable()
	if err != nil || exe == "" {
		exe = os.Args[0]
	}
	return doctor.Env{
		BinaryPath:    exe,
		PluginVersion: cfg.PluginVersion,
		SnapshotDir:   cfg.SnapshotDir,
		StateDir:      cfg.StateDir,
		RuntimeDir:    cfg.RuntimeDir,
		DataDir:       cfg.DataDir,
		TrustDir:      cfg.TrustDir,
		Cfg:           cfg,
	}
}

// renderDoctorReport emits the report in the requested format. JSON
// renders the full doctor.Report; text renders one line per check plus
// a trailing summary.
func renderDoctorReport(w io.Writer, format string, report doctor.Report) error {
	switch format {
	case doctorFormatJSON:
		buf, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			return err
		}
		if _, err := w.Write(buf); err != nil {
			return err
		}
		_, err = w.Write([]byte{'\n'})
		return err
	case doctorFormatText:
		var b strings.Builder
		var ok, warn, fail, skip int
		for _, c := range report.Checks {
			switch c.Status {
			case doctor.StatusOK:
				ok++
			case doctor.StatusWarn:
				warn++
			case doctor.StatusFail:
				fail++
			case doctor.StatusSkip:
				skip++
			}
			fmt.Fprintf(&b, "%-6s %s  %s\n", strings.ToUpper(string(c.Status)), c.ID, c.Message)
		}
		fmt.Fprintf(&b, "%d checks, %d ok, %d warn, %d fail, %d skip\n",
			len(report.Checks), ok, warn, fail, skip)
		_, err := io.WriteString(w, b.String())
		return err
	default:
		return fmt.Errorf("unknown format: %s", format)
	}
}
