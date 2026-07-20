package logger

import (
	"context"
	"fmt"
	"io"
	"log"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

const (
	ansiReset     = "\033[0m"
	ansiDim       = "\033[2m"       // DEBUG (gray/dim)
	ansiGreen     = "\033[32m"      // INFO
	ansiYellow    = "\033[33m"      // WARN
	ansiBoldRed   = "\033[1;31m"    // ERROR
	ansiCyan      = "\033[36m"      // key names in dev mode
	ansiWhite     = "\033[97m"      // message text
	ansiBoldWhite = "\033[1;97m"
)

// levelColor maps slog.Level → ANSI prefix + abbreviated label.
func levelColor(l slog.Level) (color, label string) {
	switch {
	case l >= slog.LevelError:
		return ansiBoldRed, "ERR"
	case l >= slog.LevelWarn:
		return ansiYellow, "WRN"
	case l >= slog.LevelInfo:
		return ansiGreen, "INF"
	default:
		return ansiDim, "DBG"
	}
}

// ── Context key ───────────────────────────────────────────────────────────────

type ctxKey struct{}

// ── Package-level state ───────────────────────────────────────────────────────

var (
	root    *slog.Logger     // the root structured logger (set by Init)
	App     *log.Logger      // backward-compat shim for existing logger.App.Printf calls
	initMu  sync.Mutex
)

// ── Init ──────────────────────────────────────────────────────────────────────

// Init constructs the logger based on the APP_ENV environment variable.
// Must be called once from main before any other package initialises.
// Returns a close function that flushes/closes the log file.
func Init() (closeFunc func(), err error) {
	initMu.Lock()
	defer initMu.Unlock()

	if err := os.MkdirAll("logs", 0o755); err != nil {
		return nil, fmt.Errorf("logger.Init: mkdir logs: %w", err)
	}

	logFile, err := os.OpenFile("logs/app.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("logger.Init: open log file: %w", err)
	}

	env := strings.ToLower(os.Getenv("APP_ENV"))
	isProd := env == "production" || env == "prod"

	var handler slog.Handler

	if isProd {
		// JSON to stdout + file — consumed by log aggregators.
		w := io.MultiWriter(os.Stdout, logFile)
		handler = slog.NewJSONHandler(w, &slog.HandlerOptions{
			Level:     slog.LevelDebug,
			AddSource: true,
		})
	} else {
		// Dev: color-coded console to stdout, plain text to file.
		handler = &multiHandler{
			console: &devHandler{w: os.Stdout, color: true},
			file:    &devHandler{w: logFile, color: false},
		}
	}

	root = slog.New(handler)
	slog.SetDefault(root)

	// Backward-compat shim: routes Printf through slog at INFO level.
	// The log.Writer() output goes through a bridging writer so the
	// caller's formatted string is forwarded as the slog "msg" field.
	App = log.New(&slogBridge{logger: root}, "", 0)

	return func() { _ = logFile.Close() }, nil
}

// ── Public structured API ─────────────────────────────────────────────────────

// Debug logs at DEBUG level (gray in dev).
func Debug(msg string, args ...any) { logAt(slog.LevelDebug, msg, args...) }

// Info logs at INFO level (green in dev).
func Info(msg string, args ...any) { logAt(slog.LevelInfo, msg, args...) }

// Warn logs at WARN level (yellow in dev).
func Warn(msg string, args ...any) { logAt(slog.LevelWarn, msg, args...) }

// Error logs at ERROR level (bold red in dev).
func Error(msg string, args ...any) { logAt(slog.LevelError, msg, args...) }

func logAt(level slog.Level, msg string, args ...any) {
	if root == nil {
		return
	}
	// Adjust frame skip: logAt → Debug/Info/Warn/Error → caller.
	root.Log(context.Background(), level, msg, args...)
}

// ── Context propagation ───────────────────────────────────────────────────────

// With stores a child logger enriched with extra key-value pairs into ctx.
// Subsequent calls to FromContext(ctx) return this logger, automatically
// including all stored attrs (e.g. request_id) in every log line.
//
//	ctx = logger.With(ctx, "request_id", reqID, "user_id", uid)
//	logger.FromContext(ctx).Info("room created", "room_id", id)
func With(ctx context.Context, args ...any) context.Context {
	l := FromContext(ctx).With(args...)
	return context.WithValue(ctx, ctxKey{}, l)
}

// FromContext returns the logger stored in ctx by With.
// Falls back to the root logger if none was set — safe to call anywhere.
//
//	log := logger.FromContext(ctx)
//	log.Info("handler invoked", "method", r.Method)
func FromContext(ctx context.Context) *slog.Logger {
	if ctx != nil {
		if l, ok := ctx.Value(ctxKey{}).(*slog.Logger); ok && l != nil {
			return l
		}
	}
	if root != nil {
		return root
	}
	return slog.Default()
}

// ── devHandler (color-coded text) ─────────────────────────────────────────────

// devHandler is a slog.Handler that writes human-readable, optionally
// ANSI-colored lines. Format:
//
//	15:04:05.000 INF [source.go:42] message  key=val key2=val2
type devHandler struct {
	w     io.Writer
	color bool
	attrs []slog.Attr
	group string

	mu sync.Mutex
}

func (h *devHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }

func (h *devHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	cp := *h
	cp.attrs = append(append([]slog.Attr{}, h.attrs...), attrs...)
	return &cp
}

func (h *devHandler) WithGroup(name string) slog.Handler {
	cp := *h
	cp.group = name
	return &cp
}

func (h *devHandler) Handle(_ context.Context, r slog.Record) error {
	color, label := levelColor(r.Level)

	var b strings.Builder

	// Timestamp
	if h.color {
		b.WriteString(ansiDim)
	}
	b.WriteString(r.Time.Format("15:04:05.000"))
	if h.color {
		b.WriteString(ansiReset)
	}
	b.WriteByte(' ')

	// Level label
	if h.color {
		b.WriteString(color)
	}
	b.WriteString(label)
	if h.color {
		b.WriteString(ansiReset)
	}
	b.WriteByte(' ')

	// Source (file:line) — only in dev for quick jump-to-line
	if r.PC != 0 {
		frames := runtime.CallersFrames([]uintptr{r.PC})
		f, _ := frames.Next()
		file := filepath.Base(f.File)
		if h.color {
			b.WriteString(ansiDim)
		}
		fmt.Fprintf(&b, "%-20s", fmt.Sprintf("%s:%d", file, f.Line))
		if h.color {
			b.WriteString(ansiReset)
		}
		b.WriteByte(' ')
	}

	// Message
	if h.color {
		b.WriteString(ansiWhite)
	}
	b.WriteString(r.Message)
	if h.color {
		b.WriteString(ansiReset)
	}

	// Pre-stored attrs (from WithAttrs)
	writeAttrs(&b, h.attrs, h.color)

	// Record attrs (from the log call itself)
	r.Attrs(func(a slog.Attr) bool {
		writeAttr(&b, a, h.color)
		return true
	})

	b.WriteByte('\n')

	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := fmt.Fprint(h.w, b.String())
	return err
}

func writeAttrs(b *strings.Builder, attrs []slog.Attr, color bool) {
	for _, a := range attrs {
		writeAttr(b, a, color)
	}
}

func writeAttr(b *strings.Builder, a slog.Attr, color bool) {
	if a.Key == "" {
		return
	}
	b.WriteByte(' ')
	if color {
		b.WriteString(ansiCyan)
	}
	b.WriteString(a.Key)
	b.WriteByte('=')
	if color {
		b.WriteString(ansiReset)
	}
	v := a.Value.Resolve()
	if v.Kind() == slog.KindString {
		s := v.String()
		// Quote strings containing spaces or special chars.
		if strings.ContainsAny(s, " \t\n\"") {
			fmt.Fprintf(b, "%q", s)
		} else {
			b.WriteString(s)
		}
	} else {
		fmt.Fprintf(b, "%v", v.Any())
	}
}

// ── multiHandler (fan-out: console + file) ────────────────────────────────────

type multiHandler struct {
	console slog.Handler
	file    slog.Handler
}

func (m *multiHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return m.console.Enabled(ctx, l) || m.file.Enabled(ctx, l)
}

func (m *multiHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &multiHandler{
		console: m.console.WithAttrs(attrs),
		file:    m.file.WithAttrs(attrs),
	}
}

func (m *multiHandler) WithGroup(name string) slog.Handler {
	return &multiHandler{
		console: m.console.WithGroup(name),
		file:    m.file.WithGroup(name),
	}
}

func (m *multiHandler) Handle(ctx context.Context, r slog.Record) error {
	// Clone the record for the file handler because Handle may mutate it.
	r2 := r.Clone()
	_ = m.console.Handle(ctx, r)
	return m.file.Handle(ctx, r2)
}

// ── slogBridge: routes log.Logger output through slog ─────────────────────────

// slogBridge implements io.Writer. log.Logger writes its formatted string
// here; we strip the trailing newline and forward as a slog INFO record.
type slogBridge struct {
	logger *slog.Logger
}

func (b *slogBridge) Write(p []byte) (int, error) {
	msg := strings.TrimRight(string(p), "\n")
	// Emit at the caller's frame. Skip: Write → log.Logger.Output → Printf → caller.
	// We use slog's internal path — emit a record directly so we control the PC.
	b.logger.Info(msg)
	return len(p), nil
}

// ── Ensure time is always present even before Init (test safety) ──────────────

func init() {
	// Provide a no-op root so any package-level logger call before Init
	// doesn't panic. init() runs after all package-level vars are set.
	if root == nil {
		root = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
		App = log.New(os.Stderr, "", log.Ltime|log.Lmicroseconds)
	}
	// Silence unused import warning on time if needed.
	_ = time.Now
}
