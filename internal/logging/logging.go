// Package logging configures the project's slog logger.
//
// Honors init-plan.md §6 invariant 3 — slog to stderr only, so stdout
// stays reserved for renderer output when `--output-file` is unset. The
// returned logger is also installed as the slog default so packages that
// don't accept an injected logger (providers' IMDS probe, embedded SDKs
// that already use slog) still observe the project's level + format.
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
)

// Options drives logger construction. Zero values are valid: text format,
// INFO level, stderr.
type Options struct {
	Level     string    // "debug" | "info" | "warn" | "error" — case-insensitive
	Format    string    // "text" (default) | "json"
	AddSource bool      // include source file:line in records
	Output    io.Writer // defaults to os.Stderr
}

// New builds a *slog.Logger from opts. Returns an error only when the
// level string is non-empty and unrecognized (silently passing a typo
// would mask production misconfiguration). Format defaults to text on
// unknown values, intentionally — operators frequently typo to "JSON"
// (capitalized) and we don't want that to crash the binary.
func New(opts Options) (*slog.Logger, error) {
	level, err := parseLevel(opts.Level)
	if err != nil {
		return nil, err
	}

	out := opts.Output
	if out == nil {
		out = os.Stderr
	}

	handlerOpts := &slog.HandlerOptions{
		Level:     level,
		AddSource: opts.AddSource,
	}

	var h slog.Handler
	switch strings.ToLower(opts.Format) {
	case "json":
		h = slog.NewJSONHandler(out, handlerOpts)
	default:
		// "text" and unrecognized both fall here so a typo doesn't crash
		// the process. The default is friendlier for terminals.
		h = slog.NewTextHandler(out, handlerOpts)
	}

	return slog.New(h), nil
}

// SetDefault installs l as the slog package default. Call once at startup
// so packages that use slog.Info / slog.Warn without injection (the
// providers' helpers, future SDKs that adopt slog) see the same level
// and handler as the rest of the project.
func SetDefault(l *slog.Logger) {
	slog.SetDefault(l)
}

func parseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error", "err":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("unknown log level %q (want debug|info|warn|error)", s)
	}
}
