#!/usr/bin/env bash
#
# Smoke test for the optional HTTP submission API. Run it against a container
# started with JMAIL_HTTP_TOKEN set; see docs/src/guide/http-api.md.
#
# Every check exits non-zero on failure, so it can be used as a gate:
#   JMAIL_TEST_HTTP_TOKEN=... ./test-http.sh
set -euo pipefail

base_url="http://${JMAIL_TEST_HTTP_SERVER:-127.0.0.1}:${JMAIL_TEST_HTTP_PORT:-8080}"
token="${JMAIL_TEST_HTTP_TOKEN:-}"
sender="${JMAIL_TEST_SENDER:-noreply@example.com}"
receiver="${JMAIL_TEST_RECEIVER:-user@example.com}"
# .invalid is reserved by RFC 2606, so this can never belong to an allowed domain.
foreign_sender="${JMAIL_TEST_FOREIGN_SENDER:-someone@example.invalid}"

status=""
response=""

step() { printf '\n%s\n' "$1"; }
pass() { printf '  ok    %s\n' "$1"; }
note() { printf '        %s\n' "$1"; }
fail() {
  printf '  fail  %s\n' "$1" >&2
  exit 1
}

command -v curl >/dev/null 2>&1 || fail "curl is required"
[[ -n "$token" ]] || fail "JMAIL_TEST_HTTP_TOKEN is not set"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

# call METHOD PATH BODY [curl args...] sets status and response.
call() {
  local method="$1" path="$2" body="$3"
  shift 3

  : >"$tmp/headers"
  local args=(-sS -o "$tmp/body" -D "$tmp/headers" -w '%{http_code}' -X "$method")
  if [[ -n "$body" ]]; then
    args+=(-H 'Content-Type: application/json' --data "$body")
  fi

  status="$(curl "${args[@]}" "$@" "${base_url}${path}")" ||
    fail "curl could not reach ${base_url}${path}; is the container running on that port?"
  response="$(cat "$tmp/body")"
}

json_string() {
  # Never fails: a missing field is reported by the check that wanted it, not by
  # set -e aborting a command substitution with no message.
  grep -o "\"$1\":\"[^\"]*\"" "$tmp/body" 2>/dev/null | head -n1 | cut -d'"' -f4 || true
}

printf 'jmail HTTP API smoke test against %s\n' "$base_url"

step "healthz"
call GET /healthz ""
[[ "$status" == "200" ]] || fail "GET /healthz returned $status, want 200"
[[ "$response" == ok* ]] || fail "GET /healthz body was '$response'"
pass "GET /healthz is 200 without a token"

step "authentication"
call POST /v1/send '{"from":"'"$sender"'","to":["'"$receiver"'"],"subject":"no token","text":"no token"}'
[[ "$status" == "401" ]] || fail "POST without a token returned $status, want 401: $response"
pass "a request without a token is rejected"

step "validation"
call POST /v1/send '{"from":' -H "Authorization: Bearer $token"
[[ "$status" == "400" ]] || fail "malformed JSON returned $status, want 400: $response"
pass "malformed JSON is rejected"

call POST /v1/send \
  '{"from":"'"$foreign_sender"'","to":["'"$receiver"'"],"subject":"foreign","text":"foreign"}' \
  -H "Authorization: Bearer $token"
case "$status" in
  422)
    [[ "$(json_string code)" == "sender_not_allowed" ]] ||
      fail "a rejectable sender returned 422 '$(json_string code)', want sender_not_allowed"
    pass "a sender outside the allowed domains is rejected"
    ;;
  202)
    note "the sender was accepted, so JMAIL_HTTP_ALLOWED_FROM must be '*'"
    note "skipping the sender policy check"
    ;;
  *)
    fail "a foreign sender returned $status: $response"
    ;;
esac

step "send"
payload='{"from":"'"$sender"'","to":["'"$receiver"'"],"subject":"jmail HTTP API test","text":"This message was sent through the HTTP API.","html":"<p>This message was sent through the HTTP API.</p>","headers":{"List-Unsubscribe":"<mailto:unsubscribe@example.com>, <https://example.com/unsubscribe?id=42>","List-Unsubscribe-Post":"List-Unsubscribe=One-Click"}}'
call POST /v1/send "$payload" -H "Authorization: Bearer $token"
[[ "$status" == "202" ]] || fail "POST /v1/send returned $status, want 202: $response"
queue_id="$(json_string id)"
request_id="$(json_string request_id)"
[[ -n "$request_id" ]] || fail "the response has no request_id: $response"
pass "queued as ${queue_id:-<Postfix returned no queue id>}"

step "idempotency"
idempotency_key="jmail-smoke-$(date +%s)-$$"
call POST /v1/send "$payload" -H "Authorization: Bearer $token" -H "Idempotency-Key: $idempotency_key"
first_status="$status"
first_id="$(json_string id)"
call POST /v1/send "$payload" -H "Authorization: Bearer $token" -H "Idempotency-Key: $idempotency_key"
[[ "$first_status" == "202" ]] || note "the first request returned $first_status, so the replay is not meaningful"
[[ "$first_id" == "$(json_string id)" ]] ||
  fail "the replay returned a different id, so the message was sent twice"
grep -qi '^idempotent-replay: true' "$tmp/headers" ||
  fail "the replay did not send Idempotent-Replay: true"
pass "the repeated Idempotency-Key replayed the first response"

printf '\nPassed\n'
printf 'Queue id:   %s\n' "${queue_id:-<none>}"
printf 'Request id: %s\n' "$request_id"
printf 'Both appear in the container log: podman logs --tail 20 jmail\n'
