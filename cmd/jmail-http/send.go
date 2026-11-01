package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strings"
)

const maxIdempotencyKeyLength = 255

type sendRequest struct {
	From        string            `json:"from"`
	To          []string          `json:"to"`
	Cc          []string          `json:"cc"`
	Bcc         []string          `json:"bcc"`
	ReplyTo     []string          `json:"reply_to"`
	Subject     string            `json:"subject"`
	Text        string            `json:"text"`
	HTML        string            `json:"html"`
	Headers     map[string]string `json:"headers"`
	Attachments []attachment      `json:"attachments"`
}

type attachment struct {
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Content     string `json:"content_base64"`
}

type sendResponse struct {
	// ID is the Postfix queue id from the "queued as ..." reply, which is what
	// `postqueue -p` and the logs use. It is empty if Postfix did not echo one.
	ID        string `json:"id,omitempty"`
	Status    string `json:"status"`
	RequestID string `json:"request_id,omitempty"`
}

func (s *server) handleSend(w http.ResponseWriter, r *http.Request) {
	id := requestID(r.Context())

	if !s.limiter.allow(s.now()) {
		w.Header().Set("Retry-After", "1")
		writeError(w, id, http.StatusTooManyRequests, "rate_limited", "too many messages queued recently, retry shortly")
		return
	}

	if contentType := r.Header.Get("Content-Type"); contentType != "" {
		mediaType, _, err := mime.ParseMediaType(contentType)
		if err != nil || mediaType != "application/json" {
			writeError(w, id, http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json")
			return
		}
	}

	req, err := decodeSendRequest(w, r, s.cfg.maxBytes)
	if err != nil {
		var apiErr *apiError
		if errors.As(err, &apiErr) {
			writeError(w, id, apiErr.Status, apiErr.Code, apiErr.Message)
			return
		}
		slog.Error("decode request", "id", id, "error", err)
		writeError(w, id, http.StatusBadRequest, "invalid_json", "request body could not be parsed")
		return
	}

	envelope, err := buildEnvelope(req, s.cfg)
	if err != nil {
		var apiErr *apiError
		if errors.As(err, &apiErr) {
			writeError(w, id, apiErr.Status, apiErr.Code, apiErr.Message)
			return
		}
		slog.Error("build message", "id", id, "error", err)
		writeError(w, id, http.StatusInternalServerError, "internal_error", "message could not be built")
		return
	}

	message, err := envelope.bytes(s.cfg, s.now())
	if err != nil {
		slog.Error("render message", "id", id, "error", err)
		writeError(w, id, http.StatusInternalServerError, "internal_error", "message could not be rendered")
		return
	}

	// A repeated Idempotency-Key must not produce a second copy of the mail, which
	// is what a client retry after a timeout would otherwise do.
	idempotencyKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if len(idempotencyKey) > maxIdempotencyKeyLength {
		writeError(w, id, http.StatusUnprocessableEntity, "invalid_request",
			fmt.Sprintf("Idempotency-Key must be at most %d characters", maxIdempotencyKeyLength))
		return
	}

	entry, owner := s.idem.begin(idempotencyKey, s.now())
	if !owner {
		if cached := entry.wait(); cached != nil {
			slog.Info("idempotent replay", "id", id, "idempotency_key", idempotencyKey)
			writeCached(w, *cached)
			return
		}
		// The original attempt failed and released the key; the client has to
		// retry so this request is not charged twice against the rate limit.
		writeError(w, id, http.StatusConflict, "idempotency_conflict",
			"a request with this Idempotency-Key failed while in flight, retry to send again")
		return
	}

	queueID, err := s.submitter.submit(r.Context(), envelope.from.Address, envelope.recipients(), message)
	if err != nil {
		s.idem.abort(entry)
		s.logSubmitFailure(id, err)
		writeSubmitFailure(w, id, r.Context(), err)
		return
	}

	body := sendResponse{ID: queueID, Status: "queued", RequestID: id}
	payload, marshalErr := json.Marshal(body)
	if marshalErr != nil {
		// Only reachable with a non-UTF-8 queue id, which Postfix does not emit.
		slog.Error("marshal response", "id", id, "error", marshalErr)
		writeError(w, id, http.StatusInternalServerError, "internal_error", "response could not be encoded")
		return
	}
	s.idem.finish(entry, cachedResponse{status: http.StatusAccepted, body: payload})

	slog.Info("queued",
		"id", id,
		"queue_id", queueID,
		"from", envelope.from.Address,
		"recipients", len(envelope.recipients()),
		"attachments", len(envelope.attachments),
		"bytes", len(message),
	)
	writeJSON(w, http.StatusAccepted, body)
}

func decodeSendRequest(w http.ResponseWriter, r *http.Request, maxBytes int64) (*sendRequest, error) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)

	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()

	var req sendRequest
	if err := decoder.Decode(&req); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return nil, newAPIError(http.StatusRequestEntityTooLarge, "payload_too_large",
				"request body exceeds %d bytes", maxBytes)
		}
		return nil, newAPIError(http.StatusBadRequest, "invalid_json", "request body is not valid JSON: %v", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, newAPIError(http.StatusBadRequest, "invalid_json", "request body must contain exactly one JSON object")
	}
	return &req, nil
}

func (s *server) logSubmitFailure(id string, err error) {
	fields := []any{"id", id, "error", err}
	var smtpErr *smtpError
	if errors.As(err, &smtpErr) {
		fields = append(fields, "stage", smtpErr.stage, "code", smtpErr.code)
	}
	slog.Error("submission failed", fields...)
}

func writeSubmitFailure(w http.ResponseWriter, id string, ctx context.Context, err error) {
	var smtpErr *smtpError
	switch {
	case errors.Is(err, context.Canceled), ctx.Err() != nil:
		// The caller went away; nothing useful can be written, but keep the shape.
		writeError(w, id, http.StatusServiceUnavailable, "submission_canceled", "the request was canceled")
	case errors.Is(err, context.DeadlineExceeded):
		writeError(w, id, http.StatusGatewayTimeout, "submission_timeout", "the mail server did not respond in time")
	case errors.As(err, &smtpErr):
		writeError(w, id, http.StatusBadGateway, "submission_failed", smtpErr.clientMessage())
	default:
		writeError(w, id, http.StatusBadGateway, "submission_failed", "the message could not be handed to the mail server")
	}
}

func writeCached(w http.ResponseWriter, cached cachedResponse) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Idempotent-Replay", "true")
	w.WriteHeader(cached.status)
	_, _ = w.Write(cached.body)
}
