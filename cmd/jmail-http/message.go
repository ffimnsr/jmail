package main

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/http"
	"net/mail"
	"net/textproto"
	"path"
	"sort"
	"strings"
	"time"
)

type envelope struct {
	from    *mail.Address
	to      []*mail.Address
	cc      []*mail.Address
	bcc     []*mail.Address
	replyTo []*mail.Address

	subject string
	extra   map[string]string

	text string
	html string

	attachments []resolvedAttachment
}

type resolvedAttachment struct {
	filename    string
	contentType string
	content     []byte
}

// reservedHeaders are rejected in the caller-supplied map: either they are
// derived from the request fields, or allowing them would let a caller spoof
// authentication results or invalidate the DKIM signature Postfix is about to add.
func reservedHeaders() map[string]string {
	return map[string]string{
		"from":                      "it is set from the from field",
		"to":                        "it is set from the to field",
		"cc":                        "it is set from the cc field",
		"bcc":                       "bcc is envelope-only and must not appear in headers",
		"reply-to":                  "it is set from the reply_to field",
		"subject":                   "it is set from the subject field",
		"date":                      "it is generated when the message is queued",
		"message-id":                "it is generated when the message is queued",
		"mime-version":              "it is generated with the body",
		"content-type":              "it is derived from text, html and attachments",
		"content-transfer-encoding": "it is derived from the body encoding",
		"content-disposition":       "it is derived from attachments",
		"return-path":               "it is set by Postfix from the envelope sender",
		"received":                  "it is added by relays",
		"sender":                    "it must not diverge from from",
		"dkim-signature":            "OpenDKIM signs the message",
		"domainkey-signature":       "OpenDKIM signs the message",
		"authentication-results":    "it is added by receivers",
	}
}

var reservedHeaderPrefixes = []string{"content-", "dkim-", "domainkey-", "arc-", "resent-"}

func buildEnvelope(req *sendRequest, cfg *config) (*envelope, error) {
	env := &envelope{}

	from, err := mail.ParseAddress(strings.TrimSpace(req.From))
	if err != nil {
		return nil, newAPIError(http.StatusUnprocessableEntity, "invalid_request", "from is not a valid address: %v", err)
	}
	if err := validateEnvelopeAddress(from.Address); err != nil {
		return nil, newAPIError(http.StatusUnprocessableEntity, "invalid_request", "from: %v", err)
	}
	if !cfg.fromAllowed(from.Address) {
		return nil, newAPIError(http.StatusUnprocessableEntity, "sender_not_allowed",
			"from %q is not in an allowed sending domain (%s); other domains cannot be DKIM-signed",
			from.Address, strings.Join(cfg.allowedDomains(), ", "))
	}
	env.from = from

	if env.to, err = parseAddressList("to", req.To); err != nil {
		return nil, err
	}
	if env.cc, err = parseAddressList("cc", req.Cc); err != nil {
		return nil, err
	}
	if env.bcc, err = parseAddressList("bcc", req.Bcc); err != nil {
		return nil, err
	}
	if env.replyTo, err = parseAddressList("reply_to", req.ReplyTo); err != nil {
		return nil, err
	}
	if len(env.to) == 0 {
		return nil, newAPIError(http.StatusUnprocessableEntity, "invalid_request", "to must contain at least one recipient")
	}
	if total := len(env.to) + len(env.cc) + len(env.bcc); total > cfg.maxRecipients {
		return nil, newAPIError(http.StatusUnprocessableEntity, "invalid_request",
			"%d recipients exceed the limit of %d", total, cfg.maxRecipients)
	}

	subject := strings.TrimSpace(req.Subject)
	if err := validateHeaderValue(subject); err != nil {
		return nil, newAPIError(http.StatusUnprocessableEntity, "invalid_request", "subject: %v", err)
	}
	env.subject = subject

	if env.extra, err = normalizeExtraHeaders(req.Headers); err != nil {
		return nil, err
	}

	// An empty body is almost always a bug in the caller, and a message with no
	// content is more likely to be filtered than delivered.
	env.text, env.html = req.Text, req.HTML
	if env.text == "" && env.html == "" {
		return nil, newAPIError(http.StatusUnprocessableEntity, "invalid_request", "one of text or html must be set")
	}

	if env.attachments, err = resolveAttachments(req.Attachments, cfg); err != nil {
		return nil, err
	}
	return env, nil
}

// recipients is the SMTP envelope: to, cc and bcc together, deduplicated. bcc
// exists only here, never in the headers.
func (e *envelope) recipients() []string {
	seen := make(map[string]struct{}, len(e.to)+len(e.cc)+len(e.bcc))
	out := make([]string, 0, len(e.to)+len(e.cc)+len(e.bcc))
	for _, group := range [][]*mail.Address{e.to, e.cc, e.bcc} {
		for _, addr := range group {
			key := strings.ToLower(addr.Address)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, addr.Address)
		}
	}
	return out
}

func parseAddressList(field string, values []string) ([]*mail.Address, error) {
	var out []*mail.Address
	for _, raw := range values {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		if err := validateHeaderValue(raw); err != nil {
			return nil, newAPIError(http.StatusUnprocessableEntity, "invalid_request", "%s: %q: %v", field, raw, err)
		}
		// ParseAddressList accepts "a@b, c@d" in one entry, so a client that
		// collapses the array still works.
		addrs, err := mail.ParseAddressList(raw)
		if err != nil {
			return nil, newAPIError(http.StatusUnprocessableEntity, "invalid_request", "%s: %q is not a valid address: %v", field, raw, err)
		}
		for _, addr := range addrs {
			if err := validateEnvelopeAddress(addr.Address); err != nil {
				return nil, newAPIError(http.StatusUnprocessableEntity, "invalid_request", "%s: %v", field, err)
			}
			out = append(out, addr)
		}
	}
	return out, nil
}

// validateEnvelopeAddress runs after parsing, because the bare address is
// written straight into MAIL FROM:<...> and RCPT TO:<...>.
func validateEnvelopeAddress(addr string) error {
	if addr == "" {
		return fmt.Errorf("empty address")
	}
	if err := validateHeaderValue(addr); err != nil {
		return err
	}
	if strings.ContainsAny(addr, "<> \t,;\"") {
		return fmt.Errorf("%q must be a bare address without brackets, spaces or quotes", addr)
	}
	if !strings.Contains(addr, "@") {
		return fmt.Errorf("%q is missing a domain", addr)
	}
	return nil
}

func validateHeaderValue(value string) error {
	for i := 0; i < len(value); i++ {
		switch c := value[i]; {
		case c == '\r' || c == '\n':
			return fmt.Errorf("must not contain a line break")
		case c == 0:
			return fmt.Errorf("must not contain a NUL byte")
		case c < 0x20 && c != '\t':
			return fmt.Errorf("must not contain the control character 0x%02x", c)
		}
	}
	return nil
}

func validHeaderName(name string) bool {
	if name == "" || len(name) > 78 {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c < 33 || c > 126 || c == ':' {
			return false
		}
	}
	return true
}

func normalizeExtraHeaders(in map[string]string) (map[string]string, error) {
	if len(in) == 0 {
		return nil, nil
	}

	reserved := reservedHeaders()
	out := make(map[string]string, len(in))
	for name, value := range in {
		if !validHeaderName(name) {
			return nil, newAPIError(http.StatusUnprocessableEntity, "invalid_request", "headers: %q is not a valid header name", name)
		}
		canonical := textproto.CanonicalMIMEHeaderKey(name)
		lower := strings.ToLower(canonical)

		if reason, ok := reserved[lower]; ok {
			return nil, newAPIError(http.StatusUnprocessableEntity, "invalid_request", "headers: %s cannot be set here; %s", canonical, reason)
		}
		for _, prefix := range reservedHeaderPrefixes {
			if strings.HasPrefix(lower, prefix) {
				return nil, newAPIError(http.StatusUnprocessableEntity, "invalid_request", "headers: %s uses the reserved prefix %q", canonical, prefix)
			}
		}
		if err := validateHeaderValue(value); err != nil {
			return nil, newAPIError(http.StatusUnprocessableEntity, "invalid_request", "headers: %s: %v", canonical, err)
		}
		out[canonical] = strings.TrimSpace(value)
	}
	return out, nil
}

func resolveAttachments(in []attachment, cfg *config) ([]resolvedAttachment, error) {
	if len(in) == 0 {
		return nil, nil
	}
	if len(in) > cfg.maxAttachments {
		return nil, newAPIError(http.StatusUnprocessableEntity, "invalid_request",
			"%d attachments exceed the limit of %d", len(in), cfg.maxAttachments)
	}

	out := make([]resolvedAttachment, 0, len(in))
	var total int64
	for i, item := range in {
		filename := sanitizeFilename(item.Filename)
		if filename == "" {
			return nil, newAPIError(http.StatusUnprocessableEntity, "invalid_request", "attachments[%d]: filename is required", i)
		}

		// Accept wrapped base64, which is what most encoders produce.
		content, err := base64.StdEncoding.DecodeString(stripWhitespace(item.Content))
		if err != nil {
			return nil, newAPIError(http.StatusUnprocessableEntity, "invalid_request",
				"attachments[%d]: content_base64 is not valid base64: %v", i, err)
		}
		total += int64(len(content))
		if total > cfg.maxBytes {
			return nil, newAPIError(http.StatusRequestEntityTooLarge, "payload_too_large",
				"decoded attachments exceed %d bytes", cfg.maxBytes)
		}

		contentType, err := resolveContentType(item.ContentType, filename)
		if err != nil {
			return nil, newAPIError(http.StatusUnprocessableEntity, "invalid_request", "attachments[%d]: %v", i, err)
		}

		out = append(out, resolvedAttachment{filename: filename, contentType: contentType, content: content})
	}
	return out, nil
}

// resolveContentType round-trips through ParseMediaType/FormatMediaType so a
// caller-supplied value cannot inject a header and always comes out canonical.
func resolveContentType(raw, filename string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		if guessed := mime.TypeByExtension(path.Ext(filename)); guessed != "" {
			return guessed, nil
		}
		return "application/octet-stream", nil
	}

	mediaType, params, err := mime.ParseMediaType(raw)
	if err != nil {
		return "", fmt.Errorf("content_type %q is invalid: %v", raw, err)
	}
	formatted := mime.FormatMediaType(mediaType, params)
	if formatted == "" {
		return "", fmt.Errorf("content_type %q could not be normalised", raw)
	}
	return formatted, nil
}

func sanitizeFilename(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	name = path.Base(name)
	if name == "." || name == "/" {
		return ""
	}
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	if len(name) > 200 {
		name = name[:200]
	}
	return name
}

func stripWhitespace(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\t', '\r', '\n':
			return -1
		}
		return r
	}, s)
}

type bodyEncoding struct {
	contentType      string
	transferEncoding string
}

// bytes renders the full RFC 5322 message: headers, then a body that is
// text/plain, text/html, multipart/alternative, or multipart/mixed when there
// are attachments.
func (e *envelope) bytes(cfg *config, now time.Time) ([]byte, error) {
	var body bytes.Buffer
	encoding, err := e.render(&body)
	if err != nil {
		return nil, err
	}

	messageID, err := newMessageID(cfg.hostname)
	if err != nil {
		return nil, err
	}

	var out bytes.Buffer
	writeHeader(&out, "Date", now.UTC().Format(time.RFC1123Z))
	writeHeader(&out, "From", e.from.String())
	if len(e.to) > 0 {
		writeHeader(&out, "To", formatAddresses(e.to))
	}
	if len(e.cc) > 0 {
		writeHeader(&out, "Cc", formatAddresses(e.cc))
	}
	if len(e.replyTo) > 0 {
		writeHeader(&out, "Reply-To", formatAddresses(e.replyTo))
	}
	writeHeader(&out, "Subject", mime.QEncoding.Encode("utf-8", e.subject))
	writeHeader(&out, "Message-ID", messageID)
	writeHeader(&out, "MIME-Version", "1.0")
	writeHeader(&out, "Content-Type", encoding.contentType)
	if encoding.transferEncoding != "" {
		writeHeader(&out, "Content-Transfer-Encoding", encoding.transferEncoding)
	}
	for _, name := range sortedKeys(e.extra) {
		writeHeader(&out, name, e.extra[name])
	}
	out.WriteString("\r\n")
	out.Write(body.Bytes())
	return out.Bytes(), nil
}

func (e *envelope) render(w io.Writer) (bodyEncoding, error) {
	var inner bytes.Buffer
	innerEncoding, err := e.renderContent(&inner)
	if err != nil {
		return bodyEncoding{}, err
	}

	if len(e.attachments) == 0 {
		if _, err := w.Write(inner.Bytes()); err != nil {
			return bodyEncoding{}, err
		}
		return innerEncoding, nil
	}

	// The inner part is rendered first because its Content-Type is only known
	// after the boundaries are chosen, and the part header has to be written
	// before the content.
	mixed := multipart.NewWriter(w)
	part, err := createPart(mixed, innerEncoding.contentType, innerEncoding.transferEncoding)
	if err != nil {
		return bodyEncoding{}, err
	}
	if _, err := part.Write(inner.Bytes()); err != nil {
		return bodyEncoding{}, err
	}
	for _, item := range e.attachments {
		if err := writeAttachment(mixed, item); err != nil {
			return bodyEncoding{}, err
		}
	}
	if err := mixed.Close(); err != nil {
		return bodyEncoding{}, err
	}
	return bodyEncoding{contentType: "multipart/mixed; boundary=" + mixed.Boundary()}, nil
}

func (e *envelope) renderContent(w io.Writer) (bodyEncoding, error) {
	hasText, hasHTML := e.text != "", e.html != ""

	if hasText && hasHTML {
		alternative := multipart.NewWriter(w)
		if err := writeQuotedPrintablePart(alternative, "text/plain; charset=utf-8", e.text); err != nil {
			return bodyEncoding{}, err
		}
		if err := writeQuotedPrintablePart(alternative, "text/html; charset=utf-8", e.html); err != nil {
			return bodyEncoding{}, err
		}
		if err := alternative.Close(); err != nil {
			return bodyEncoding{}, err
		}
		return bodyEncoding{contentType: "multipart/alternative; boundary=" + alternative.Boundary()}, nil
	}

	contentType, content := "text/plain; charset=utf-8", e.text
	if hasHTML {
		contentType, content = "text/html; charset=utf-8", e.html
	}

	writer := quotedprintable.NewWriter(w)
	if _, err := io.WriteString(writer, content); err != nil {
		return bodyEncoding{}, err
	}
	if err := writer.Close(); err != nil {
		return bodyEncoding{}, err
	}
	return bodyEncoding{contentType: contentType, transferEncoding: "quoted-printable"}, nil
}

func createPart(w *multipart.Writer, contentType, transferEncoding string) (io.Writer, error) {
	header := textproto.MIMEHeader{}
	header.Set("Content-Type", contentType)
	if transferEncoding != "" {
		header.Set("Content-Transfer-Encoding", transferEncoding)
	}
	return w.CreatePart(header)
}

func writeQuotedPrintablePart(w *multipart.Writer, contentType, content string) error {
	part, err := createPart(w, contentType, "quoted-printable")
	if err != nil {
		return err
	}
	writer := quotedprintable.NewWriter(part)
	if _, err := io.WriteString(writer, content); err != nil {
		return err
	}
	return writer.Close()
}

func writeAttachment(w *multipart.Writer, item resolvedAttachment) error {
	header := textproto.MIMEHeader{}
	header.Set("Content-Type", item.contentType)
	header.Set("Content-Transfer-Encoding", "base64")
	if disposition := mime.FormatMediaType("attachment", map[string]string{"filename": item.filename}); disposition != "" {
		header.Set("Content-Disposition", disposition)
	}

	part, err := w.CreatePart(header)
	if err != nil {
		return err
	}
	encoder := base64.NewEncoder(base64.StdEncoding, &lineWrapper{w: part, width: 76})
	if _, err := encoder.Write(item.content); err != nil {
		return err
	}
	return encoder.Close()
}

// lineWrapper breaks base64 into 76 character lines, which encoding/base64 does
// not do on its own and MIME requires.
type lineWrapper struct {
	w     io.Writer
	width int
	used  int
}

func (l *lineWrapper) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		if l.used == l.width {
			if _, err := io.WriteString(l.w, "\r\n"); err != nil {
				return written, err
			}
			l.used = 0
		}
		chunk := p
		if remaining := l.width - l.used; len(chunk) > remaining {
			chunk = chunk[:remaining]
		}
		if _, err := l.w.Write(chunk); err != nil {
			return written, err
		}
		written += len(chunk)
		l.used += len(chunk)
		p = p[len(chunk):]
	}
	return written, nil
}

func writeHeader(w *bytes.Buffer, name, value string) {
	w.WriteString(name)
	w.WriteString(": ")
	w.WriteString(value)
	w.WriteString("\r\n")
}

func formatAddresses(addrs []*mail.Address) string {
	parts := make([]string, 0, len(addrs))
	for _, addr := range addrs {
		parts = append(parts, addr.String())
	}
	return strings.Join(parts, ", ")
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func newMessageID(hostname string) (string, error) {
	id, err := randomBytes(16)
	if err != nil {
		return "", err
	}
	host := strings.Trim(hostname, ".")
	if host == "" {
		host = "localhost"
	}
	return fmt.Sprintf("<%s@%s>", hex.EncodeToString(id), host), nil
}

func randomBytes(n int) ([]byte, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return nil, fmt.Errorf("read random bytes: %w", err)
	}
	return buf, nil
}

func randomHex(n int) string {
	buf, err := randomBytes(n)
	if err != nil {
		// Only used for log correlation ids; a fixed value is better than a panic.
		return strings.Repeat("0", n*2)
	}
	return hex.EncodeToString(buf)
}
