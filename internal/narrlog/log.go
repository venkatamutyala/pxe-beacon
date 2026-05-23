// Package narrlog provides narrated logging helpers for pxe-beacon.
//
// The PLAN's north star is diagnosability: every decision, every served
// asset, every benign condition gets a clear log line. This package is
// intentionally a thin wrapper around stdlib log so the call sites stay
// readable and the level filtering is centralized in one place.
package narrlog

import (
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"time"
)

// Level controls the verbosity of the narrated log.
type Level int

const (
	LevelError Level = iota
	LevelWarn
	LevelInfo
	LevelDebug
)

func ParseLevel(s string) (Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "error":
		return LevelError, nil
	case "warn", "warning":
		return LevelWarn, nil
	case "info", "":
		return LevelInfo, nil
	case "debug":
		return LevelDebug, nil
	}
	return LevelInfo, fmt.Errorf("unknown log level %q", s)
}

func (l Level) String() string {
	switch l {
	case LevelError:
		return "error"
	case LevelWarn:
		return "warn"
	case LevelInfo:
		return "info"
	case LevelDebug:
		return "debug"
	}
	return fmt.Sprintf("level(%d)", int(l))
}

// Logger is the narrated logger.
type Logger struct {
	mu    sync.Mutex
	level Level
	out   io.Writer
	comp  string // component tag, e.g. "proxydhcp", "tftp", "http"
	std   *log.Logger
}

// New returns a Logger that writes to w (defaults to os.Stderr if nil).
func New(comp string, level Level, w io.Writer) *Logger {
	if w == nil {
		w = os.Stderr
	}
	return &Logger{
		level: level,
		out:   w,
		comp:  comp,
		std:   log.New(w, "", 0),
	}
}

// With returns a new Logger that shares the underlying sink but carries
// a different component tag (e.g. for sub-systems).
func (l *Logger) With(comp string) *Logger {
	if l == nil {
		return New(comp, LevelInfo, nil)
	}
	return &Logger{
		level: l.level,
		out:   l.out,
		comp:  comp,
		std:   l.std,
	}
}

// SetLevel changes the logger level at runtime.
func (l *Logger) SetLevel(level Level) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.level = level
}

// Level returns the current level.
func (l *Logger) Level() Level {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.level
}

func (l *Logger) emit(lvl Level, format string, args ...any) {
	l.mu.Lock()
	cur := l.level
	l.mu.Unlock()
	if lvl > cur {
		return
	}
	ts := time.Now().Format("15:04:05.000")
	msg := fmt.Sprintf(format, args...)
	l.std.Printf("%s %-5s %-9s %s", ts, lvl.String(), l.comp, msg)
}

func (l *Logger) Errorf(format string, args ...any) { l.emit(LevelError, format, args...) }
func (l *Logger) Warnf(format string, args ...any)  { l.emit(LevelWarn, format, args...) }
func (l *Logger) Infof(format string, args ...any)  { l.emit(LevelInfo, format, args...) }
func (l *Logger) Debugf(format string, args ...any) { l.emit(LevelDebug, format, args...) }

// Decision logs a single decision-level line. The format is a
// deliberately stable shape so users can grep boot flows:
//
//	client <mac> arch=0x07(EFI-x64) userclass=<none> stage=firmware-TFTP -> decision: serve ipxe.efi via TFTP from 10.0.0.5
func (l *Logger) Decision(client, arch, userclass, stage, decision string) {
	if client == "" {
		client = "<unknown>"
	}
	if userclass == "" {
		userclass = "<none>"
	}
	l.Infof("client %s arch=%s userclass=%s stage=%s -> decision: %s",
		client, arch, userclass, stage, decision)
}

// Served logs a serving event with byte count.
func (l *Logger) Served(proto, what, path string, n int) {
	l.Infof("%s %s %q -> serving %s (%d bytes)", proto, "RRQ/GET", path, what, n)
}

// Benign logs a benign post-handoff condition so users learn not to
// chase it as a bug.
func (l *Logger) Benign(reason string) {
	l.Infof("(benign: %s)", reason)
}

// Hint logs a failure-path hint. Hints fire on common stuck states and
// tell the user what to actually check.
func (l *Logger) Hint(format string, args ...any) {
	l.Infof("hint: "+format, args...)
}

// HexDump renders raw bytes for -loglevel debug. Returns silently when
// the level is below debug.
func (l *Logger) HexDump(label string, b []byte) {
	if l.Level() < LevelDebug {
		return
	}
	var sb strings.Builder
	for i := 0; i < len(b); i += 16 {
		end := i + 16
		if end > len(b) {
			end = len(b)
		}
		fmt.Fprintf(&sb, "  %04x  ", i)
		for j := i; j < end; j++ {
			fmt.Fprintf(&sb, "%02x ", b[j])
		}
		sb.WriteString("\n")
	}
	l.Debugf("%s (%d bytes):\n%s", label, len(b), sb.String())
}
