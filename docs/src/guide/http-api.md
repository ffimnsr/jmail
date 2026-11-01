# HTTP API

jmail can also accept messages over HTTP. The API is **off by default** and only
starts when `JMAIL_HTTP_TOKEN` is set.

It is worth enabling when the client cannot open an outbound SMTP connection, for
example a serverless function, an edge runtime, a CI job, or a provider that
allows `443` but blocks `25` and `587`. If your client can reach port `587`, use
SMTP: it is the same mail path with one less moving part.

The API does not deliver mail itself. It builds the MIME message, hands it to
Postfix over the loopback interface, and returns the queue id. Signing, retries,
TLS policy and the queue all stay in Postfix and OpenDKIM.

## Enabling it

```sh
podman run -d --name jmail \
  -h mail.example.com \
  -p 587:587 \
  -p 8080:8080 \
  -e JMAIL_HOSTNAME=mail.example.com \
  -e JMAIL_ORIGIN=example.com \
  -e JMAIL_SMTP_USER=jmail \
  -e JMAIL_SMTP_PASSWORD='change-me' \
  -e JMAIL_HTTP_TOKEN="$(openssl rand -hex 32)" \
  ghcr.io/ffimnsr/jmail:latest
```

The credential is deliberately separate from `JMAIL_SMTP_PASSWORD`: it is sent as a
bearer token on every request, so it should be rotatable on its own.

There is no TLS listener. Terminate TLS in front of the container, with Caddy,
Traefik or nginx, and publish the API only where it needs to be reachable. The
`8080` port inside the container speaks plain HTTP.

## `POST /v1/send`

```sh
curl -X POST http://127.0.0.1:8080/v1/send \
  -H "Authorization: Bearer $JMAIL_HTTP_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{
    "from": "App <noreply@example.com>",
    "to": ["alice@example.net"],
    "subject": "Hello",
    "text": "Plain text body",
    "html": "<p>HTML body</p>"
  }'
```

```json
{"id": "4F3A1B2C", "status": "queued", "request_id": "9c1f5e2a7b3d4c60"}
```

`202 Accepted` means Postfix queued the message. It does not mean the message was
delivered, and there is no endpoint that reports delivery: jmail is outbound-only
and never accepts a DSN. `id` is the Postfix queue id when Postfix echoed one, so
it matches `postqueue -p` and the container log.

### Request fields

| Field | Type | Notes |
| --- | --- | --- |
| `from` | string | Required. Must be inside an allowed domain, see below. |
| `to` | string[] | Required, at least one. Each entry may also be `"a@b, c@d"`. |
| `cc` | string[] | Optional. |
| `bcc` | string[] | Optional. Envelope only: it never appears in the headers. |
| `reply_to` | string[] | Optional. |
| `subject` | string | Optional. Encoded as UTF-8 if it contains non-ASCII. |
| `text` | string | Plain text body. |
| `html` | string | HTML body. |
| `headers` | object | Extra headers, including the subscription headers described in [Bulk and marketing mail](#bulk-and-marketing-mail). Reserved names are rejected, see below. |
| `attachments` | object[] | `filename`, optional `content_type`, `content_base64`. |

At least one of `text` or `html` is required. When both are present they are sent
as `multipart/alternative`; attachments wrap that in `multipart/mixed`.

`Date`, `Message-ID`, `MIME-Version` and all `Content-*` headers are generated.
`headers` rejects anything that would let a caller spoof the envelope or the
authentication result: the fields above, `Return-Path`, `Received`, `Sender`,
`Authentication-Results`, and any `Content-*`, `DKIM-*`, `ARC-*` or `Resent-*`
name. Header values must be single-line.

### Sender policy

`from` must be in a domain listed in `JMAIL_HTTP_ALLOWED_FROM`, which defaults to
`JMAIL_ORIGIN`. This is stricter than it looks: the OpenDKIM signing table covers
`*@JMAIL_ORIGIN` with `SubDomains no`, so mail from any other domain would go out
**unsigned**. Such a request is rejected with `422 sender_not_allowed` instead of
being sent unsigned.

Set `JMAIL_HTTP_ALLOWED_FROM` to a space or comma separated list to widen the
policy, and `*` to accept any sender. A wildcard does not make other domains
DKIM-signed; it only stops the API from refusing them.

### Bulk and marketing mail

Subscription headers are ordinary headers, so they go through `headers`
unchanged; none of them collide with the reserved names or prefixes. Gmail and
Yahoo require one-click unsubscribe from bulk senders, which is the
`List-Unsubscribe` / `List-Unsubscribe-Post` pair:

```json
{
  "from": "news@example.com",
  "to": ["subscriber@example.net"],
  "subject": "Spring news",
  "text": "Hello",
  "html": "<p>Hello</p>",
  "headers": {
    "List-Unsubscribe": "<mailto:unsubscribe@example.com?subject=unsubscribe>, <https://example.com/unsubscribe?id=42>",
    "List-Unsubscribe-Post": "List-Unsubscribe=One-Click",
    "List-Id": "\"Example Newsletter\" <news.example.com>",
    "Precedence": "bulk",
    "Feedback-ID": "campaign:spring:example"
  }
}
```

- One-click unsubscribe needs the `https` URI. A mailto-only value satisfies the
  older `List-Unsubscribe` requirement but not one-click, and the `-Post` header
  is meaningless without a URI a mailbox provider can POST to.
- jmail is outbound-only, so a `mailto:` unsubscribe address has to be handled
  somewhere else. Prefer the HTTPS URI.
- Keep `from` inside `JMAIL_ORIGIN`. Bulk mail from a domain that jmail does not
  sign fails DMARC alignment at the large providers, and the unsubscribe headers
  do not compensate for that.
- These headers are written before the milter runs, so OpenDKIM signs them.
  `List-Unsubscribe`, `List-Unsubscribe-Post`, `List-Id` and `Feedback-ID` are all
  inside the signature; see [DKIM](usage.md#dkim) for the full header set.
- `Feedback-ID` is the grouping key in Google Postmaster Tools, so set it per
  stream if you read those reports.
- The API accepts one value per header name. Use the comma separated form RFC
  2369 defines when you need more than one `List-Unsubscribe` URI.
- The rate limit is a burst guard, not a list scheduler: jmail does no
  per-recipient pacing, so a campaign is handed to Postfix as fast as you submit
  it. Throttle on the sending side, and keep the recipient list out of a single
  request.

### Idempotency

Send an `Idempotency-Key` header to make retries safe. A repeat with the same key
within `JMAIL_HTTP_IDEMPOTENCY_TTL` (one hour by default) replays the original
response and sends nothing; the replay carries `Idempotent-Replay: true`. An
attempt that fails does not reserve the key, so the client can retry.

Records are written to `JMAIL_HTTP_IDEMPOTENCY_DIR` before the response is sent, so
they survive a restart of the process or the container. That directory is a declared
volume: `podman rm` without `-v` leaves the records behind in an anonymous volume
rather than deleting them, and a named volume or bind mount lets you attach them to a
new container. They are not atomic with the SMTP submission itself: a crash between
handing the message to Postfix and recording the reply can still produce a duplicate
on retry. Set the variable to `off` to keep records in memory only, which is what a
read-only root filesystem effectively does anyway.

### Errors

Errors are JSON: `{"request_id": "...", "error": {"code": "...", "message": "..."}}`.

| Status | `code` | Meaning |
| --- | --- | --- |
| `400` | `invalid_json` | Body is not a single valid JSON object, or has an unknown field. |
| `401` | `unauthorized` | Missing or wrong bearer token. |
| `413` | `payload_too_large` | Body, or decoded attachments, exceed `JMAIL_HTTP_MAX_BYTES`. |
| `415` | `unsupported_media_type` | `Content-Type` is not `application/json`. |
| `422` | `invalid_request` | A field is missing or malformed. |
| `422` | `sender_not_allowed` | `from` is outside the allowed domains. |
| `429` | `rate_limited` | Over `JMAIL_HTTP_RATE`; honor `Retry-After`. |
| `502` | `submission_failed` | Postfix refused the message, or could not be reached. |
| `504` | `submission_timeout` | Postfix did not answer within `JMAIL_HTTP_TIMEOUT`. |

A `4xx` means nothing was sent. A `502` may still have queued the message if the
connection failed after `DATA`; when that matters, send an `Idempotency-Key`.

## `GET /healthz`

Unauthenticated, returns `200 ok` when the process is serving. It does not check
Postfix, because the container health check already does.

## Shutdown

On `SIGTERM` the listener closes, in-flight submissions get `JMAIL_HTTP_DRAIN` (2s by
default) to finish, and the process exits. The window is deliberately short: a submission
to Postfix on the loopback completes in milliseconds, and a wedged Postfix should not hold
a container stop open for the full submission timeout. s6 restarts the service if it ever
exits unexpectedly.

## Podman quadlet

`scripts/quadlet.sh --http` publishes the API and writes a generated token into
the unit's `0600` environment file:

```sh
./scripts/quadlet.sh --http --install
```

It publishes `127.0.0.1:10808` by default, because the container serves this port
over plain HTTP. Set `JMAIL_HTTP_PUBLISH_PORT` to change the port, and
`JMAIL_HTTP_PUBLISH_BIND=0.0.0.0` to listen on every interface, which is only
sensible behind a TLS terminator.

## Smoke test

`test-http.sh` in the repository root walks the checks above with curl: health, a
missing token, malformed JSON, the sender policy, a real send, and an idempotent
replay. It exits non-zero on the first failure, so it works as a gate.

```sh
JMAIL_TEST_HTTP_TOKEN=<token> ./test-http.sh
```

It reads `JMAIL_TEST_HTTP_SERVER` (default `127.0.0.1`), `JMAIL_TEST_HTTP_PORT`
(default `8080`), `JMAIL_TEST_HTTP_TOKEN`, `JMAIL_TEST_SENDER` and
`JMAIL_TEST_RECEIVER`. `mk test-http` runs it against the dev container with
those variables already set.

## Limits

Every limit is an environment variable, see [Environment](../refs/environment.md):

- `JMAIL_HTTP_MAX_BYTES` caps the request body and the decoded attachments.
- `JMAIL_HTTP_MAX_RECIPIENTS` caps `to` + `cc` + `bcc`. Duplicates are collapsed.
- `JMAIL_HTTP_MAX_ATTACHMENTS` caps the attachment count.
- `subject` is capped at 900 bytes, and `headers` at 50 entries of 900 bytes each. The
  cap is there because OpenDKIM rejects an oversized header block and the milter is set to
  `accept`, so passing it would send the message **unsigned** instead of failing loudly.
- `JMAIL_HTTP_CONCURRENCY` caps simultaneous SMTP submissions, and `JMAIL_HTTP_TIMEOUT`
  bounds one submission including its wait for a free slot, so the queue of waiting
  requests cannot grow without limit.
- `JMAIL_HTTP_RATE` and `JMAIL_HTTP_RATE_BURST` are a token bucket over accepted
  requests, shared by all clients because there is one credential.

These are the only thing between an exposed endpoint and a full Postfix queue, so
keep them tighter than the SMTP relay settings if the endpoint is reachable from
the internet.

## Logs

The service logs one line per request to stdout, which s6 forwards into the
container log:

```
level=INFO msg=queued id=9c1f5e2a7b3d4c60 queue_id=4F3A1B2C from=noreply@example.com recipients=1 attachments=0 bytes=754
level=INFO msg=request id=9c1f5e2a7b3d4c60 method=POST path=/v1/send status=202 bytes=68 duration=3ms remote=10.0.0.5:52414
```

`id` is the value returned as `request_id` and echoed in the `X-Request-Id` header,
which is the one to quote when a caller reports a problem.

Access lines are sampled at `JMAIL_HTTP_LOG_RATE` per second, because a client that
ignores `429` would otherwise write a line per rejected request and bury everything
else. Dropped lines are counted, and the next line that gets through reports them as
`suppressed=N`; while a flood continues a summary is emitted every five seconds.
Successful `/healthz` probes are not logged; failures are.
