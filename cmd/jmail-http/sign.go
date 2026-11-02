package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
)

const (
	// responseSignatureHeader carries the signature of the exact response body.
	responseSignatureHeader = "X-Jmail-Signature"

	// responseSignatureVersion prefixes the header so the scheme can change
	// without a client having to guess which one it received.
	responseSignatureVersion = "v1"
)

// signResponseBody is HMAC-SHA256 over the body bytes, base64 encoded. The key is
// the bearer token the caller already sent, so a client can verify a reply with
// the same credential it used for the request.
func signResponseBody(secret, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return responseSignatureVersion + "," + base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// signResponses buffers the response so the signature covers exactly the bytes
// written to the wire. The header has to be set before WriteHeader, which is why
// the body cannot be signed as it streams.
func signResponses(secret []byte, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buffered := &bufferedResponseWriter{header: make(http.Header)}
		next.ServeHTTP(buffered, r)

		for name, values := range buffered.header {
			w.Header()[name] = values
		}
		// The outer middleware has already set X-Request-Id on w, so only the
		// handler's headers are copied here.
		w.Header().Set(responseSignatureHeader, signResponseBody(secret, buffered.body))

		status := buffered.status
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
		_, _ = w.Write(buffered.body)
	})
}

// bufferedResponseWriter records a response instead of sending it. The handlers
// on this route write small, fully formed bodies, so holding one in memory is
// cheaper than a streaming hash with a trailer.
type bufferedResponseWriter struct {
	header      http.Header
	status      int
	wroteHeader bool
	body        []byte
}

func (b *bufferedResponseWriter) Header() http.Header { return b.header }

func (b *bufferedResponseWriter) WriteHeader(status int) {
	if b.wroteHeader {
		return
	}
	b.status = status
	b.wroteHeader = true
}

func (b *bufferedResponseWriter) Write(p []byte) (int, error) {
	if !b.wroteHeader {
		b.WriteHeader(http.StatusOK)
	}
	b.body = append(b.body, p...)
	return len(p), nil
}
