package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

type server struct {
	cfg       *config
	submitter *submitter
	limiter   *tokenBucket
	idem      *idempotency
	logs      *logLimiter
	now       func() time.Time
}

func newServer(cfg *config) *server {
	var store *idemStore
	if cfg.idempotencyDir == "" {
		slog.Info("idempotency persistence is disabled, keys will not survive a restart")
	} else {
		opened, err := openIdemStore(cfg.idempotencyDir)
		if err != nil {
			slog.Warn("idempotency records will not survive a restart",
				"dir", cfg.idempotencyDir, "error", err)
		} else {
			store = opened
		}
	}

	return &server{
		cfg:       cfg,
		submitter: newSubmitter(cfg.smtpAddr, cfg.hostname, cfg.smtpTimeout, cfg.concurrency),
		limiter:   newTokenBucket(cfg.rate, cfg.burst),
		idem:      newIdempotency(cfg.idempotencyTTL, 4096, store),
		logs:      newLogLimiter(cfg.logRate),
		now:       time.Now,
	}
}

func (s *server) close() {
	if s.idem != nil && s.idem.store != nil {
		if err := s.idem.store.close(); err != nil {
			slog.Error("closing the idempotency store", "error", err)
		}
	}
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	// /healthz is unauthenticated so a container health check does not need the
	// send token; it reveals nothing beyond "the process is serving".
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.Handle("POST /v1/send", s.authenticate(http.HandlerFunc(s.handleSend)))
	return s.instrument(mux)
}

func (s *server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "ok\n")
}

// apiError carries the HTTP status alongside the client-facing code and message.
type apiError struct {
	Status  int    `json:"-"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *apiError) Error() string { return e.Message }

func newAPIError(status int, code, format string, args ...any) *apiError {
	return &apiError{Status: status, Code: code, Message: fmt.Sprintf(format, args...)}
}

type errorBody struct {
	RequestID string   `json:"request_id,omitempty"`
	Error     apiError `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, requestID string, status int, code, message string) {
	writeJSON(w, status, errorBody{
		RequestID: requestID,
		Error:     apiError{Code: code, Message: message},
	})
}

// authenticate compares digests rather than the raw token so the comparison is
// constant time regardless of the supplied length. A missing token is a startup
// error, so reaching this point always means a token is configured.
func (s *server) authenticate(next http.Handler) http.Handler {
	want := sha256.Sum256([]byte(s.cfg.token))
	const prefix = "bearer "

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := strings.TrimSpace(r.Header.Get("Authorization"))
		if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="jmail"`)
			writeError(w, requestID(r.Context()), http.StatusUnauthorized, "unauthorized", "missing bearer token")
			return
		}
		got := sha256.Sum256([]byte(strings.TrimSpace(header[len(prefix):])))
		if subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="jmail"`)
			writeError(w, requestID(r.Context()), http.StatusUnauthorized, "unauthorized", "invalid bearer token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

type ctxKey int

const ctxRequestID ctxKey = iota

func requestID(ctx context.Context) string {
	id, _ := ctx.Value(ctxRequestID).(string)
	return id
}

// statusRecorder records what was actually written so the access log shows the
// real status rather than an assumed 200.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	written     int
	wroteHeader bool
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.wroteHeader = true
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(p []byte) (int, error) {
	r.wroteHeader = true
	n, err := r.ResponseWriter.Write(p)
	r.written += n
	return n, err
}

// instrument assigns a request id, recovers a panicking handler and logs one
// line per request. Recovery lives here instead of in its own middleware so it
// can tell whether a response was already started, and so the log line still
// carries the real status when a handler panics.
func (s *server) instrument(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := randomHex(8)
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		rec.Header().Set("X-Request-Id", id)

		start := time.Now()
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v)
				}
				slog.Error("panic while handling request", "id", id, "panic", v)
				if !rec.wroteHeader {
					writeError(rec, id, http.StatusInternalServerError, "internal_error", "internal error")
				}
			}

			// A health check every 30 seconds is noise; its failures still log.
			if r.Method == http.MethodGet && r.URL.Path == "/healthz" && rec.status == http.StatusOK {
				return
			}

			allowed, suppressed := s.logs.allow(time.Now())
			if !allowed {
				return
			}

			attrs := []any{
				"id", id,
				"method", r.Method,
				"path", r.URL.Path,
				"status", rec.status,
				"bytes", rec.written,
				"duration", time.Since(start).Round(time.Microsecond).String(),
				"remote", r.RemoteAddr,
			}
			if suppressed > 0 {
				attrs = append(attrs, "suppressed", suppressed)
			}
			slog.Info("request", attrs...)
		}()

		next.ServeHTTP(rec, r.WithContext(context.WithValue(r.Context(), ctxRequestID, id)))
	})
}
