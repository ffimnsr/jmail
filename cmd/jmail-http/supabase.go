package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// supabaseHookURLPath is only routed when a secret is configured. Supabase Auth
// signs with Standard Webhooks instead of the API's bearer token, so this
// handler authenticates the request itself.
const supabaseHookURLPath = "/v1/hooks/supabase/send-email"
const supabaseHookPattern = "POST " + supabaseHookURLPath

const (
	webhookSignatureVersion   = "v1"
	webhookTimestampTolerance = 5 * time.Minute
)

// supabaseHookPayload is the subset of the Send Email Hook payload this service
// uses. Supabase sends much more in the user object; unknown fields are fine
// because the body is not decoded with DisallowUnknownFields.
type supabaseHookPayload struct {
	User struct {
		Email    string `json:"email"`
		NewEmail string `json:"new_email"`
	} `json:"user"`
	EmailData struct {
		Token           string `json:"token"`
		TokenHash       string `json:"token_hash"`
		RedirectTo      string `json:"redirect_to"`
		EmailActionType string `json:"email_action_type"`
		SiteURL         string `json:"site_url"`
		TokenNew        string `json:"token_new"`
		TokenHashNew    string `json:"token_hash_new"`
	} `json:"email_data"`
}

// supabaseHook is one Supabase project: the secret its requests are signed
// with, the project API URL used for confirmation links, and the sender. The
// name is the environment prefix it came from, for logs and error messages.
type supabaseHook struct {
	name   string
	secret []byte
	apiURL string
	from   string
}

// hookMessage is one email to send. A secure email change produces two.
type hookMessage struct {
	to        string
	action    string
	token     string
	tokenHash string
	redirect  string
}

// parseWebhookSecret accepts both forms Supabase shows: the full
// `v1,whsec_<base64>` value from the dashboard, and a bare base64 secret.
func parseWebhookSecret(raw string) ([]byte, error) {
	trimmed := strings.TrimSpace(raw)
	trimmed = strings.TrimPrefix(trimmed, webhookSignatureVersion+",")
	trimmed = strings.TrimPrefix(trimmed, "whsec_")

	for _, encoding := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding} {
		decoded, err := encoding.DecodeString(trimmed)
		if err == nil && len(decoded) > 0 {
			return decoded, nil
		}
	}
	return nil, fmt.Errorf("must be base64, as in v1,whsec_<base64 secret>")
}

// verifyWebhook checks a Standard Webhooks signature: HMAC-SHA256 over
// "<id>.<timestamp>.<body>" with the decoded secret. The timestamp window is
// what bounds replay of a captured request.
func verifyWebhook(secret []byte, id, timestamp, signatures string, body []byte, now time.Time) error {
	if id == "" || timestamp == "" || signatures == "" {
		return fmt.Errorf("missing webhook-id, webhook-timestamp or webhook-signature header")
	}

	unix, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return fmt.Errorf("webhook-timestamp is not a Unix timestamp")
	}
	if skew := now.Sub(time.Unix(unix, 0)); skew > webhookTimestampTolerance || skew < -webhookTimestampTolerance {
		return fmt.Errorf("webhook-timestamp is more than %s from the server clock", webhookTimestampTolerance)
	}

	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(id))
	mac.Write([]byte("."))
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write(body)
	expected := mac.Sum(nil)

	for _, candidate := range strings.Fields(signatures) {
		version, encoded, ok := strings.Cut(candidate, ",")
		if !ok || version != webhookSignatureVersion {
			continue
		}
		got, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			continue
		}
		if hmac.Equal(got, expected) {
			return nil
		}
	}
	return fmt.Errorf("signature does not match")
}

// supabaseMessages maps the hook payload to the messages to send. The reversed
// field naming for a secure email change is handled the way Supabase documents
// it: token_hash_new belongs with the current address, token_hash with the new
// one, and the token that goes with each hash is the non-suffixed counterpart.
func (c *config) supabaseMessages(p supabaseHookPayload) ([]hookMessage, error) {
	action := strings.ToLower(strings.TrimSpace(p.EmailData.EmailActionType))
	if action == "" {
		return nil, fmt.Errorf("email_data.email_action_type is empty")
	}
	if p.User.Email == "" {
		return nil, fmt.Errorf("user.email is empty")
	}

	if action == "email_change" {
		if p.EmailData.TokenHashNew != "" && p.EmailData.TokenHash != "" && p.User.NewEmail != "" {
			return []hookMessage{
				{to: p.User.Email, action: action, token: p.EmailData.Token, tokenHash: p.EmailData.TokenHashNew, redirect: p.EmailData.RedirectTo},
				{to: p.User.NewEmail, action: action, token: p.EmailData.TokenNew, tokenHash: p.EmailData.TokenHash, redirect: p.EmailData.RedirectTo},
			}, nil
		}

		to := p.User.NewEmail
		if to == "" {
			to = p.User.Email
		}
		token, hash := p.EmailData.Token, p.EmailData.TokenHash
		if hash == "" {
			token, hash = p.EmailData.TokenNew, p.EmailData.TokenHashNew
		}
		if hash == "" {
			return nil, fmt.Errorf("email_data has no token hash for the email change")
		}
		return []hookMessage{{to: to, action: action, token: token, tokenHash: hash, redirect: p.EmailData.RedirectTo}}, nil
	}

	if p.EmailData.TokenHash == "" {
		return nil, fmt.Errorf("email_data.token_hash is empty")
	}
	return []hookMessage{{
		to:        p.User.Email,
		action:    action,
		token:     p.EmailData.Token,
		tokenHash: p.EmailData.TokenHash,
		redirect:  p.EmailData.RedirectTo,
	}}, nil
}

func supabaseSubject(action string) (subject, intro string) {
	switch action {
	case "signup":
		return "Confirm your email address", "Confirm your email address to finish signing up."
	case "recovery":
		return "Reset your password", "Follow the link to choose a new password."
	case "invite":
		return "You have been invited", "Accept the invitation to create your account."
	case "magiclink":
		return "Your magic link", "Follow the link to sign in."
	case "email_change":
		return "Confirm your email change", "Confirm this change of email address."
	case "reauthentication":
		return "Confirm it's you", "Confirm your identity to continue."
	default:
		return "Confirm your email", "Confirm your email address to continue."
	}
}

// verifyURL builds the link Supabase Auth documents for the hook receiver. It
// needs the project's API URL, which the payload does not carry.
func (h supabaseHook) verifyURL(action, tokenHash, redirectTo string) string {
	if h.apiURL == "" || tokenHash == "" {
		return ""
	}
	query := url.Values{}
	query.Set("token", tokenHash)
	query.Set("type", action)
	if redirectTo != "" {
		query.Set("redirect_to", redirectTo)
	}
	return h.apiURL + "/auth/v1/verify?" + query.Encode()
}

func composeSupabaseBody(intro, link, token string) (text, bodyHTML string) {
	var plain strings.Builder
	plain.WriteString("Hi,\n\n")
	plain.WriteString(intro)
	plain.WriteString("\n\n")
	if link != "" {
		plain.WriteString(link)
		plain.WriteString("\n\n")
	}
	if token != "" {
		plain.WriteString("Or enter this code: ")
		plain.WriteString(token)
		plain.WriteString("\n\n")
	}
	plain.WriteString("If you did not request this, you can ignore this email.\n")

	var markup strings.Builder
	markup.WriteString("<p>Hi,</p>\n")
	markup.WriteString("<p>" + html.EscapeString(intro) + "</p>\n")
	if link != "" {
		escaped := html.EscapeString(link)
		markup.WriteString(`<p><a href="` + escaped + `">` + escaped + "</a></p>\n")
	}
	if token != "" {
		markup.WriteString("<p>Or enter this code: <strong>" + html.EscapeString(token) + "</strong></p>\n")
	}
	markup.WriteString("<p>If you did not request this, you can ignore this email.</p>\n")

	return plain.String(), markup.String()
}

func (h supabaseHook) request(m hookMessage) (*sendRequest, error) {
	link := h.verifyURL(m.action, m.tokenHash, m.redirect)
	if link == "" && m.token == "" {
		return nil, fmt.Errorf("no confirmation link and no token to send")
	}
	subject, intro := supabaseSubject(m.action)
	text, bodyHTML := composeSupabaseBody(intro, link, m.token)

	return &sendRequest{
		From:    h.from,
		To:      []string{m.to},
		Subject: subject,
		Text:    text,
		HTML:    bodyHTML,
	}, nil
}

// queueMessage renders one request and hands it to Postfix. It is the same path
// POST /v1/send uses, minus the HTTP response and idempotency bookkeeping.
func (s *server) queueMessage(ctx context.Context, req *sendRequest) (string, error) {
	envelope, err := buildEnvelope(req, s.cfg)
	if err != nil {
		return "", err
	}
	message, err := envelope.bytes(s.cfg, s.now())
	if err != nil {
		return "", err
	}
	return s.submitter.submit(ctx, envelope.from.Address, envelope.recipients(), message)
}

// matchSupabaseHook returns the project whose secret verifies the signature.
// Each Supabase project signs with its own secret, which is also what selects
// the project URL and sender for the reply.
func (c *config) matchSupabaseHook(id, timestamp, signatures string, body []byte, now time.Time) (supabaseHook, error) {
	var lastErr error
	for _, hook := range c.supabaseHooks {
		err := verifyWebhook(hook.secret, id, timestamp, signatures, body, now)
		if err == nil {
			return hook, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no Supabase hook is configured")
	}
	return supabaseHook{}, lastErr
}

func (s *server) handleSupabaseSendEmail(w http.ResponseWriter, r *http.Request) {
	id := requestID(r.Context())

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.cfg.maxBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, id, http.StatusRequestEntityTooLarge, "payload_too_large",
				fmt.Sprintf("request body exceeds %d bytes", s.cfg.maxBytes))
			return
		}
		writeError(w, id, http.StatusBadRequest, "invalid_request", "request body could not be read")
		return
	}

	hook, err := s.cfg.matchSupabaseHook(
		r.Header.Get("webhook-id"),
		r.Header.Get("webhook-timestamp"),
		r.Header.Get("webhook-signature"),
		body, s.now(),
	)
	if err != nil {
		writeError(w, id, http.StatusUnauthorized, "invalid_signature", err.Error())
		return
	}

	var payload supabaseHookPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		writeError(w, id, http.StatusBadRequest, "invalid_json", "request body is not valid JSON")
		return
	}

	messages, err := s.cfg.supabaseMessages(payload)
	if err != nil {
		writeError(w, id, http.StatusUnprocessableEntity, "invalid_request", err.Error())
		return
	}

	if !s.limiter.allow(s.now()) {
		w.Header().Set("Retry-After", "1")
		writeError(w, id, http.StatusTooManyRequests, "rate_limited", "too many messages queued recently, retry shortly")
		return
	}

	for _, m := range messages {
		req, err := hook.request(m)
		if err != nil {
			writeError(w, id, http.StatusUnprocessableEntity, "invalid_request", err.Error())
			return
		}
		if _, err := s.queueMessage(r.Context(), req); err != nil {
			var apiErr *apiError
			if errors.As(err, &apiErr) {
				writeError(w, id, apiErr.Status, apiErr.Code, apiErr.Message)
				return
			}
			s.logSubmitFailure(id, err)
			writeSubmitFailure(w, id, r.Context(), err)
			return
		}
	}

	slog.Info("supabase hook accepted",
		"id", id,
		"project", hook.name,
		"action", payload.EmailData.EmailActionType,
		"messages", len(messages),
	)
	// Supabase only requires a 2xx; the documented output is empty.
	writeJSON(w, http.StatusOK, struct{}{})
}
