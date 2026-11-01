package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"fmt"
	"mime"
	"net/mail"
	"strings"
	"testing"
	"time"
)

func TestHeaderFolding(t *testing.T) {
	recipients := make([]string, 0, 10)
	for i := 0; i < 10; i++ {
		recipients = append(recipients, fmt.Sprintf("Long Recipient %02d <recipient%02d@example.net>", i, i))
	}
	longValue := strings.Repeat("word ", 120)

	env := testEnvelope(t, &sendRequest{
		From:    "app@example.com",
		To:      recipients,
		Subject: "folding",
		Text:    "body",
		Headers: map[string]string{"X-Long": longValue},
	})

	raw, err := env.bytes(testConfig("127.0.0.1:1"), time.Now())
	if err != nil {
		t.Fatalf("bytes: %v", err)
	}

	headerBlock := string(raw[:bytes.Index(raw, []byte("\r\n\r\n"))])
	for _, line := range strings.Split(headerBlock, "\r\n") {
		if len(line) > headerLineLength {
			t.Errorf("header line is %d chars, want at most %d: %q", len(line), headerLineLength, line)
		}
	}

	parsed, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	addrs, err := mail.ParseAddressList(parsed.Header.Get("To"))
	if err != nil {
		t.Fatalf("parse To: %v", err)
	}
	if len(addrs) != len(recipients) {
		t.Fatalf("To has %d addresses, want %d", len(addrs), len(recipients))
	}
	if addrs[7].Name != "Long Recipient 07" {
		t.Errorf("address 7 name = %q", addrs[7].Name)
	}

	// Folding normalises runs of whitespace, which is the only mutation allowed.
	wantLong := strings.Join(strings.Fields(longValue), " ")
	if got := parsed.Header.Get("X-Long"); got != wantLong {
		t.Errorf("X-Long round trip mismatch: %d chars, want %d", len(got), len(wantLong))
	}
}

func TestLongSubjectIsSplitIntoEncodedWords(t *testing.T) {
	subject := strings.Repeat("grüße aus münchen ", 20)

	env := testEnvelope(t, &sendRequest{
		From:    "app@example.com",
		To:      []string{"alice@example.net"},
		Subject: subject,
		Text:    "body",
	})

	raw, err := env.bytes(testConfig("127.0.0.1:1"), time.Now())
	if err != nil {
		t.Fatalf("bytes: %v", err)
	}

	parsed, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	encoded := parsed.Header.Get("Subject")

	if strings.Count(encoded, "=?utf-8?") < 2 {
		t.Fatalf("subject was not split into several encoded-words: %q", encoded)
	}

	decoded, err := new(mime.WordDecoder).DecodeHeader(encoded)
	if err != nil {
		t.Fatalf("decode subject: %v", err)
	}
	if want := strings.TrimSpace(subject); decoded != want {
		t.Errorf("decoded subject = %q, want %q", decoded, want)
	}

	for _, line := range strings.Split(string(raw), "\r\n") {
		if len(line) > headerLineLength {
			t.Errorf("header line is %d chars, want at most %d", len(line), headerLineLength)
		}
	}
}

func TestHeaderCaps(t *testing.T) {
	tooManyHeaders := make(map[string]string, maxExtraHeaderCount+1)
	for i := 0; i <= maxExtraHeaderCount; i++ {
		tooManyHeaders[fmt.Sprintf("X-Extra-%d", i)] = "v"
	}

	cases := map[string]*sendRequest{
		"too many headers": {
			From:    "a@example.com",
			To:      []string{"b@example.net"},
			Text:    "y",
			Headers: tooManyHeaders,
		},
		"header value too long": {
			From:    "a@example.com",
			To:      []string{"b@example.net"},
			Text:    "y",
			Headers: map[string]string{"X-Big": strings.Repeat("a", maxHeaderValueBytes+1)},
		},
		"subject too long": {
			From:    "a@example.com",
			To:      []string{"b@example.net"},
			Text:    "y",
			Subject: strings.Repeat("a", maxSubjectBytes+1),
		},
	}

	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := buildEnvelope(req, testConfig("127.0.0.1:1"))
			if err == nil {
				t.Fatal("expected an error")
			}
			apiErr, ok := err.(*apiError)
			if !ok {
				t.Fatalf("error type = %T, want *apiError", err)
			}
			if apiErr.Status != 422 {
				t.Errorf("status = %d, want 422", apiErr.Status)
			}
		})
	}
}

func TestWriteMessageDotStuffing(t *testing.T) {
	cases := map[string]struct{ in, want string }{
		"leading dot":      {".start\r\nrest\r\n", "..start\r\nrest\r\n.\r\n"},
		"no trailing crlf": {"a\r\nb", "a\r\nb\r\n.\r\n"},
		"empty":            {"", ".\r\n"},
		"only dot line":    {".\r\n", "..\r\n.\r\n"},
		"dot mid line":     {"a.b\r\n", "a.b\r\n.\r\n"},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			writer := bufio.NewWriter(&buf)
			if err := writeMessage(writer, []byte(tc.in)); err != nil {
				t.Fatalf("writeMessage: %v", err)
			}
			if err := writer.Flush(); err != nil {
				t.Fatalf("flush: %v", err)
			}
			if got := buf.String(); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestAttachmentsStayWithinHeaderLimits(t *testing.T) {
	// A long filename must not push a header line past the limit.
	env := testEnvelope(t, &sendRequest{
		From: "a@example.com",
		To:   []string{"b@example.net"},
		Text: "y",
		Attachments: []attachment{{
			Filename: strings.Repeat("ü", 80) + ".pdf",
			Content:  base64.StdEncoding.EncodeToString([]byte("x")),
		}},
	})

	raw, err := env.bytes(testConfig("127.0.0.1:1"), time.Now())
	if err != nil {
		t.Fatalf("bytes: %v", err)
	}
	for _, line := range strings.Split(string(raw), "\r\n") {
		if len(line) > 998 {
			t.Errorf("line is %d chars, past the RFC 5322 limit: %q", len(line), line)
		}
	}
}
