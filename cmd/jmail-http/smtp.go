package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Postfix answers DATA with "250 2.0.0 Ok: queued as 4F3A1B2C".
var queueIDPattern = regexp.MustCompile(`(?i)queued as (\S+)`)

type smtpError struct {
	stage string
	code  int
	text  string
	err   error
}

func (e *smtpError) Error() string {
	if e.code != 0 {
		return fmt.Sprintf("smtp %s: %d %s", e.stage, e.code, e.text)
	}
	if e.err != nil {
		return fmt.Sprintf("smtp %s: %v", e.stage, e.err)
	}
	return fmt.Sprintf("smtp %s: failed", e.stage)
}

func (e *smtpError) Unwrap() error { return e.err }

// clientMessage is safe to return to the caller: it is a fixed template plus a
// sanitised single line from the server.
func (e *smtpError) clientMessage() string {
	if e.code == 0 {
		return "the mail server could not be reached (" + e.stage + ")"
	}
	detail := singleLine(e.text)
	if detail == "" {
		return fmt.Sprintf("the mail server rejected the message during %s (%d)", e.stage, e.code)
	}
	return fmt.Sprintf("the mail server rejected the message during %s: %d %s", e.stage, e.code, detail)
}

func singleLine(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 300 {
		s = s[:300] + "..."
	}
	return s
}

// submitter is a deliberately small SMTP client. net/smtp is not used because it
// hides the DATA reply, which is where Postfix reports the queue id, and because
// it has no per-operation context or deadline.
type submitter struct {
	addr    string
	helo    string
	timeout time.Duration
	sem     chan struct{}
}

func newSubmitter(addr, helo string, timeout time.Duration, concurrency int) *submitter {
	if concurrency < 1 {
		concurrency = 1
	}
	return &submitter{
		addr:    addr,
		helo:    helo,
		timeout: timeout,
		sem:     make(chan struct{}, concurrency),
	}
}

func (s *submitter) submit(ctx context.Context, from string, recipients []string, message []byte) (string, error) {
	// The timeout covers the whole operation, including waiting for a free slot.
	// Without this a burst larger than the concurrency limit would leave requests
	// queued here for as long as the backlog lasts.
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	case <-ctx.Done():
		return "", ctx.Err()
	}

	deadline := time.Now().Add(s.timeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}

	conn, err := (&net.Dialer{Timeout: s.timeout}).DialContext(ctx, "tcp", s.addr)
	if err != nil {
		return "", &smtpError{stage: "connect", err: err}
	}
	defer func() { _ = conn.Close() }()

	if err := conn.SetDeadline(deadline); err != nil {
		return "", &smtpError{stage: "connect", err: err}
	}

	reader := bufio.NewReader(conn)
	writer := bufio.NewWriter(conn)

	if err := expect(reader, "greeting", 220); err != nil {
		return "", err
	}
	if err := writeCommand(writer, "EHLO %s", s.helo); err != nil {
		return "", &smtpError{stage: "EHLO", err: err}
	}
	if err := expect(reader, "EHLO", 250); err != nil {
		return "", err
	}

	if err := writeCommand(writer, "MAIL FROM:<%s>", from); err != nil {
		return "", &smtpError{stage: "MAIL FROM", err: err}
	}
	if err := expect(reader, "MAIL FROM", 250); err != nil {
		return "", err
	}

	for _, recipient := range recipients {
		if err := writeCommand(writer, "RCPT TO:<%s>", recipient); err != nil {
			return "", &smtpError{stage: "RCPT TO", err: err}
		}
		code, text, err := readResponse(reader)
		if err != nil {
			return "", &smtpError{stage: "RCPT TO", err: err}
		}
		// 251 means "user not local, will forward", which is still an accept.
		if code != 250 && code != 251 {
			return "", &smtpError{stage: "RCPT TO " + recipient, code: code, text: text}
		}
	}

	if err := writeCommand(writer, "DATA"); err != nil {
		return "", &smtpError{stage: "DATA", err: err}
	}
	if err := expect(reader, "DATA", 354); err != nil {
		return "", err
	}

	if err := writeMessage(writer, message); err != nil {
		return "", &smtpError{stage: "message body", err: err}
	}
	if err := writer.Flush(); err != nil {
		return "", &smtpError{stage: "message body", err: err}
	}

	code, text, err := readResponse(reader)
	if err != nil {
		return "", &smtpError{stage: "message body", err: err}
	}
	if code != 250 {
		return "", &smtpError{stage: "message body", code: code, text: text}
	}

	// The message is queued at this point; QUIT is best effort.
	_ = writeCommand(writer, "QUIT")

	if match := queueIDPattern.FindStringSubmatch(text); match != nil {
		return match[1], nil
	}
	return "", nil
}

func expect(reader *bufio.Reader, stage string, want int) error {
	code, text, err := readResponse(reader)
	if err != nil {
		return &smtpError{stage: stage, err: err}
	}
	if code != want {
		return &smtpError{stage: stage, code: code, text: text}
	}
	return nil
}

func readResponse(reader *bufio.Reader) (int, string, error) {
	const maxLines = 100

	var texts []string
	for line := 0; line < maxLines; line++ {
		raw, err := reader.ReadString('\n')
		if err != nil {
			return 0, "", err
		}
		raw = strings.TrimRight(raw, "\r\n")
		if len(raw) < 3 {
			return 0, "", fmt.Errorf("malformed SMTP response: %q", raw)
		}

		code, err := strconv.Atoi(raw[:3])
		if err != nil {
			return 0, "", fmt.Errorf("malformed SMTP response code: %q", raw)
		}

		// "250-text" continues, "250 text" ends the reply.
		separator := byte(' ')
		if len(raw) > 3 {
			separator = raw[3]
		}
		if len(raw) > 4 {
			texts = append(texts, raw[4:])
		}
		if separator != '-' {
			return code, strings.Join(texts, " "), nil
		}
	}
	return 0, "", fmt.Errorf("SMTP response exceeded %d lines", maxLines)
}

func writeCommand(writer *bufio.Writer, format string, args ...any) error {
	command := fmt.Sprintf(format, args...)
	// Defense in depth: addresses are validated before they get here, and a
	// line break would let a value append its own SMTP command.
	if strings.ContainsAny(command, "\r\n") {
		return fmt.Errorf("refusing to write an SMTP command containing a line break")
	}
	if _, err := writer.WriteString(command + "\r\n"); err != nil {
		return err
	}
	return writer.Flush()
}

// writeMessage dot-stuffs and terminates the DATA block. The message is already
// CRLF-terminated by the MIME builder. Lines are copied in spans rather than
// byte by byte, because a maximum-size message is several megabytes.
func writeMessage(w *bufio.Writer, message []byte) error {
	for start := 0; start < len(message); {
		line := message[start:]
		if end := bytes.IndexByte(line, '\n'); end >= 0 {
			line = line[:end+1]
		}
		if line[0] == '.' {
			if err := w.WriteByte('.'); err != nil {
				return err
			}
		}
		if _, err := w.Write(line); err != nil {
			return err
		}
		start += len(line)
	}

	if len(message) > 0 && message[len(message)-1] != '\n' {
		if _, err := w.WriteString("\r\n"); err != nil {
			return err
		}
	}
	_, err := w.WriteString(".\r\n")
	return err
}
