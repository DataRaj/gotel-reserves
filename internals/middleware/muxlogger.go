package middleware

import (
	"context"
	"fmt"
	"math/rand"
	"net/http"
	"strings"
	"time"

	"recallo/internals/logger"
)

type reqIDKey struct{}

// RequestIDFromContext returns the request ID stored by Loggingmiddleware.
// Returns "" if not set (e.g. background workers, tests without middleware).
func RequestIDFromContext(ctx context.Context) string {
	if id, ok := ctx.Value(reqIDKey{}).(string); ok {
		return id
	}
	return ""
}

// ── Status color codes ────────────────────────────────────────────────────────

const (
	ansiReset   = "\033[0m"
	ansiGreen   = "\033[32m"   // 2xx
	ansiCyan    = "\033[36m"   // 3xx
	ansiYellow  = "\033[33m"   // 4xx
	ansiBoldRed = "\033[1;31m" // 5xx
	ansiDim     = "\033[2m"    // decorative (method, path)
	ansiWhite   = "\033[97m"
)

func statusColor(code int) string {
	switch {
	case code >= 500:
		return ansiBoldRed
	case code >= 400:
		return ansiYellow
	case code >= 300:
		return ansiCyan
	default:
		return ansiGreen
	}
}

func statusIcon(code int) string {
	switch {
	case code >= 500:
		return "✗"
	case code >= 400:
		return "!"
	case code >= 300:
		return "→"
	default:
		return "✓"
	}
}

// ── responseWriter wrapper ────────────────────────────────────────────────────

// responseWriter wraps http.ResponseWriter to capture status code and bytes written.
type responseWriter struct {
	http.ResponseWriter
	status      int
	bytesWritten int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.status = code
	rw.ResponseWriter.WriteHeader(code)
}

func (rw *responseWriter) Write(b []byte) (int, error) {
	n, err := rw.ResponseWriter.Write(b)
	rw.bytesWritten += n
	return n, err
}

// Unwrap allows middleware-stack introspection (e.g. http.ResponseController).
func (rw *responseWriter) Unwrap() http.ResponseWriter { return rw.ResponseWriter }

// ── Request ID generation ─────────────────────────────────────────────────────

const requestIDHeader = "X-Request-ID"

// generateRequestID produces a compact 8-char hex ID.
// Uses math/rand (not crypto/rand) — this is tracing, not security.
func generateRequestID() string {
	return fmt.Sprintf("%08x", rand.Uint32())
}

// ── Loggingmiddleware ─────────────────────────────────────────────────────────

// Loggingmiddleware logs every HTTP request/response with timing, status color,
// and an automatically-propagated request ID.
//
// Drop-in replacement for the previous Loggingmiddleware — same signature.
func Loggingmiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		// 1. Extract or generate request ID.
		reqID := r.Header.Get(requestIDHeader)
		if reqID == "" {
			reqID = generateRequestID()
		}

		// 2. Inject request ID as response header for client-side correlation.
		w.Header().Set(requestIDHeader, reqID)

		// 3. Store request ID in context AND attach it to the per-request logger
		//    so every downstream log call auto-includes req_id.
		ctx := context.WithValue(r.Context(), reqIDKey{}, reqID)
		ctx = logger.With(ctx,
			"req_id", reqID,
			"method", r.Method,
			"path", r.URL.Path,
		)
		r = r.WithContext(ctx)

		// 4. Wrap ResponseWriter to capture status + bytes.
		rw := &responseWriter{ResponseWriter: w, status: http.StatusOK}

		// 5. Serve.
		next.ServeHTTP(rw, r)

		// 6. Emit the access log line.
		elapsed := time.Since(start)
		logRequest(ctx, r, rw.status, rw.bytesWritten, elapsed, reqID)
	})
}

// logRequest emits the formatted access log line.
// Format:  METHOD  ✓ 200  3.12ms  127.0.0.1  /path  req=a1b2c3d4  bytes=512
func logRequest(ctx context.Context, r *http.Request, status, bytesWritten int, elapsed time.Duration, reqID string) {
	sc := statusColor(status)
	icon := statusIcon(status)

	// Latency string — adaptive unit.
	latStr := fmtLatency(elapsed)

	// Client IP — strip port.
	clientIP := r.RemoteAddr
	if i := strings.LastIndex(clientIP, ":"); i >= 0 {
		clientIP = clientIP[:i]
	}
	// Prefer X-Forwarded-For for proxy environments.
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		clientIP = strings.SplitN(fwd, ",", 2)[0]
	}

	// Build the colored access line for console.
	// logger.FromContext gives us the per-request slog.Logger with req_id baked in.
	log := logger.FromContext(ctx)

	// For the access line we use a raw formatted message so the status color
	// stands out visually, with structured attrs appended for machine parsing.
	msg := fmt.Sprintf("%-7s %s%s %3d%s  %s",
		r.Method,
		sc, icon, status, ansiReset,
		latStr,
	)

	log.Info(msg,
		"ip", clientIP,
		"bytes", bytesWritten,
	)
}

// fmtLatency returns a human-readable latency string with adaptive units.
func fmtLatency(d time.Duration) string {
	switch {
	case d < time.Millisecond:
		return fmt.Sprintf("%dµs", d.Microseconds())
	case d < time.Second:
		return fmt.Sprintf("%.2fms", float64(d.Microseconds())/1000)
	default:
		return fmt.Sprintf("%.2fs", d.Seconds())
	}
}
