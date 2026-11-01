package main

import (
	"bytes"
	"encoding/base64"
	"net/mail"
	"strings"
	"testing"
	"time"
)

func testEnvelope(t *testing.T, req *sendRequest) *envelope {
	t.Helper()
	env, err := buildEnvelope(req, testConfig("127.0.0.1:1"))
	if err != nil {
		t.Fatalf("buildEnvelope: %v", err)
	}
	return env
}

func TestMessageStructure(t *testing.T) {
	env := testEnvelope(t, &sendRequest{
		From:    "app@example.com",
		To:      []string{"alice@example.net"},
		Cc:      []string{"bob@example.net"},
		Bcc:     []string{"audit@example.org"},
		Subject: "Grüße",
		Text:    "plain body",
		HTML:    "<p>html body</p>",
		Attachments: []attachment{{
			Filename: "../../etc/passwd.txt",
			Content:  base64.StdEncoding.EncodeToString([]byte("attachment content")),
		}},
	})

	rough, err := env.bytes(testConfig("127.0.0.1:1"), time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	if err != nil {
		t.Fatalf("bytes: %v", err)
	}
	parsed, err := mail.ReadMessage(bytes.NewReader(rough))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if got := parsed.Header.Get("MIME-Version"); got != "1.0" {
		t.Errorf("MIME-Version = %q", got)
	}
	contentType := parsed.Header.Get("Content-Type")
	if !strings.HasPrefix(contentType, "multipart/mixed; boundary=") {
		t.Errorf("Content-Type = %q, want multipart/mixed", contentType)
	}
	if got := parsed.Header.Get("Date"); got != "Fri, 02 Jan 2026 03:04:05 +0000" {
		t.Errorf("Date = %q", got)
	}
	if got := parsed.Header.Get("Subject"); !strings.HasPrefix(got, "=?utf-8?") {
		t.Errorf("Subject = %q, want an encoded word", got)
	}
	if got := parsed.Header.Get("Bcc"); got != "" {
		t.Errorf("Bcc = %q, want it absent from headers", got)
	}
	if got := parsed.Header.Get("Content-Transfer-Encoding"); got != "" {
		t.Errorf("top level CTE = %q, want it only on parts", got)
	}

	recipients := env.recipients()
	if len(recipients) != 3 {
		t.Fatalf("recipients = %v, want to+cc+bcc", recipients)
	}

	raw := string(rough)
	if !strings.Contains(raw, "multipart/alternative") {
		t.Error("text and html were not combined into multipart/alternative")
	}
	// The attachment filename must not escape its directory.
	if !strings.Contains(raw, `filename=passwd.txt`) {
		t.Errorf("attachment filename was not sanitised:\n%s", raw)
	}
	if strings.Contains(raw, "etc/passwd") {
		t.Error("attachment filename kept a path component")
	}
}

func TestAttachmentBase64Wrapping(t *testing.T) {
	payload := bytes.Repeat([]byte("z"), 300)
	env := testEnvelope(t, &sendRequest{
		From:    "app@example.com",
		To:      []string{"alice@example.net"},
		Subject: "with attachment",
		Text:    "see attached",
		Attachments: []attachment{{
			Filename:    "report.bin",
			ContentType: "application/octet-stream",
			Content:     base64.StdEncoding.EncodeToString(payload),
		}},
	})

	raw, err := env.bytes(testConfig("127.0.0.1:1"), time.Now())
	if err != nil {
		t.Fatalf("bytes: %v", err)
	}

	lines := strings.Split(extractBase64Block(string(raw)), "\r\n")
	if len(lines) < 2 {
		t.Fatalf("expected a wrapped base64 block, got %d lines", len(lines))
	}
	for _, line := range lines {
		if len(line) > 76 {
			t.Fatalf("base64 line is %d characters, want at most 76: %q", len(line), line)
		}
	}

	// The payload must survive decoding after unwrapping.
	decoded, err := base64.StdEncoding.DecodeString(strings.Join(lines, ""))
	if err != nil {
		t.Fatalf("decode attachment: %v", err)
	}
	if !bytes.Equal(decoded, payload) {
		t.Fatalf("decoded attachment = %d bytes, want %d", len(decoded), len(payload))
	}
}

// extractBase64Block returns the unwrapped body of the first base64 part. Part
// header order is not stable because multipart.Writer iterates a map, so this
// looks for the header line and then the following blank line.
func extractBase64Block(raw string) string {
	const (
		looking = iota
		skipping
		collecting
	)

	state := looking
	var out []string
	for _, line := range strings.Split(raw, "\r\n") {
		switch state {
		case looking:
			if strings.EqualFold(strings.TrimSpace(line), "Content-Transfer-Encoding: base64") {
				state = skipping
			}
		case skipping:
			if line == "" {
				state = collecting
			}
		case collecting:
			if strings.HasPrefix(line, "--") {
				return strings.Join(out, "\r\n")
			}
			out = append(out, line)
		}
	}
	return strings.Join(out, "\r\n")
}

func TestMessageRejectsInvalidInput(t *testing.T) {
	cases := map[string]*sendRequest{
		"too many recipients": {
			From: "a@example.com",
			To:   []string{"1@x.net", "2@x.net", "3@x.net", "4@x.net", "5@x.net", "6@x.net", "7@x.net", "8@x.net", "9@x.net", "10@x.net", "11@x.net"},
			Text: "y",
		},
		"no recipients": {
			From: "a@example.com",
			Text: "y",
		},
		"bad attachment base64": {
			From:        "a@example.com",
			To:          []string{"b@example.net"},
			Text:        "y",
			Attachments: []attachment{{Filename: "f.bin", Content: "!!!not base64!!!"}},
		},
		"attachment without filename": {
			From:        "a@example.com",
			To:          []string{"b@example.net"},
			Text:        "y",
			Attachments: []attachment{{Content: base64.StdEncoding.EncodeToString([]byte("x"))}},
		},
		"address without domain": {
			From: "a@example.com",
			To:   []string{"localonly"},
			Text: "y",
		},
	}

	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := buildEnvelope(req, testConfig("127.0.0.1:1")); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestRecipientsAreDeduplicated(t *testing.T) {
	env := testEnvelope(t, &sendRequest{
		From: "a@example.com",
		To:   []string{"Same@Example.net", "same@example.net"},
		Cc:   []string{"same@example.net"},
		Text: "y",
	})

	recipients := env.recipients()
	if len(recipients) != 1 {
		t.Fatalf("recipients = %v, want one entry", recipients)
	}
}

func TestHeaderValueValidation(t *testing.T) {
	for _, value := range []string{"ok", "with\ttab", "ümlaut"} {
		if err := validateHeaderValue(value); err != nil {
			t.Errorf("validateHeaderValue(%q) = %v, want nil", value, err)
		}
	}
	for _, value := range []string{"a\r\nBcc: x@y", "nul\x00", "bell\x07"} {
		if err := validateHeaderValue(value); err == nil {
			t.Errorf("validateHeaderValue(%q) = nil, want an error", value)
		}
	}
}
