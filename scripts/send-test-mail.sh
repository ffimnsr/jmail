#!/usr/bin/env bash
#
# send-test-mail.sh - send one message through jmail's submission port and
# report what the server did with it.
#
# It asks for the From and To addresses, a subject and a body (a random quote
# by default), then sends over STARTTLS with SASL. Prompts read /dev/tty, so
# piping this file to bash works the same way as the installer:
#
#   curl -fsSL https://raw.githubusercontent.com/ffimnsr/jmail/master/scripts/send-test-mail.sh | bash
#
# On a machine with jmail installed it pre-fills everything from jmail.env, so
# pressing Enter through the prompts is enough.

set -euo pipefail

DEFAULT_ENV_FILE="${XDG_CONFIG_HOME:-$HOME/.config}/jmail/jmail.env"
ASSUME_YES=0
DRY_RUN=0

# --- output -----------------------------------------------------------------

die() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

warn() {
  printf 'warning: %s\n' "$*" >&2
}

need_cmd() {
  command -v "$1" >/dev/null 2>&1
}

usage() {
  cat <<'EOF'
Usage: ./scripts/send-test-mail.sh [options]

Asks for the From and To addresses, a subject and a body, then sends the
message through a jmail submission port over STARTTLS and reports the result.
On a machine with jmail installed the defaults come from jmail.env.

Options:
  --env FILE        read defaults from FILE (default: $XDG_CONFIG_HOME/jmail/jmail.env)
  --server HOST     SMTP server (default: 127.0.0.1 when the environment file
                    exists, since jmail is then on this machine; otherwise
                    JMAIL_HOSTNAME)
  --port PORT       submission port (default: JMAIL_PUBLISH_PORT, else 587)
  --user USER       SASL username (default: JMAIL_SMTP_USER)
  --password PASS   SASL password (default: JMAIL_SMTP_PASSWORD, otherwise asked)
  --from ADDR       envelope and header sender (default: noreply@JMAIL_ORIGIN)
  --to ADDR         recipient
  --subject TEXT    subject (default: "jmail test <timestamp>")
  --body TEXT       body; the default is a random quote
  -y, --yes         do not prompt; use the options and defaults as they are
  --dry-run         print the message instead of sending it
  -h, --help        show this help

Environment variables are read as a fallback, so provisioning tools can drive
it: JMAIL_TEST_SERVER, JMAIL_TEST_PORT, JMAIL_TEST_USER, JMAIL_TEST_PASSWORD,
JMAIL_TEST_SENDER and JMAIL_TEST_RECEIVER, the same names as the Python test
scripts.

Exit status: 0 when the server accepted the message, 1 when it did not.
EOF
}

# --- prompts ----------------------------------------------------------------

# Prints the answer on stdout and the prompt on stderr, so callers can capture
# the value without swallowing the prompt. An empty answer keeps the default.
ask() {
  local label="$1" default="${2-}" answer=""
  if ((ASSUME_YES)); then
    printf '%s' "$default"
    return 0
  fi
  if [[ -n "$default" ]]; then
    printf '%s [%s]: ' "$label" "$default" >&2
  else
    printf '%s: ' "$label" >&2
  fi
  IFS= read -r answer </dev/tty || die "no input available (use --yes for a non-interactive run)"
  printf '%s' "${answer:-$default}"
}

ask_required() {
  local label="$1" default="${2-}" answer
  while :; do
    answer="$(ask "$label" "$default")" || exit 1
    if [[ -n "$answer" ]]; then
      printf '%s' "$answer"
      return 0
    fi
    if ((ASSUME_YES)); then
      die "$label is required; pass the matching option or run interactively"
    fi
    printf '  a value is required\n' >&2
  done
}

ask_secret() {
  local label="$1" default="${2-}" answer=""
  if ((ASSUME_YES)); then
    printf '%s' "$default"
    return 0
  fi
  printf '%s: ' "$label" >&2
  IFS= read -rs answer </dev/tty || die "no input available (use --yes for a non-interactive run)"
  printf '\n' >&2
  printf '%s' "${answer:-$default}"
}

ask_yn() {
  local label="$1" default="${2:-y}" answer
  if ((ASSUME_YES)); then
    case "$default" in
      y | Y) return 0 ;;
      *) return 1 ;;
    esac
  fi
  while :; do
    answer="$(ask "$label (y/n)" "$default")" || exit 1
    case "$(printf '%s' "$answer" | tr '[:upper:]' '[:lower:]')" in
      y | yes) return 0 ;;
      n | no) return 1 ;;
      *) printf '  answer y or n\n' >&2 ;;
    esac
  done
}

# --- environment file -------------------------------------------------------

# Reads one key, stripping the quotes that versions of install.sh before 1.0.18
# wrote around every value.
env_read() {
  local file="$1" key="$2"
  [[ -f "$file" ]] || return 0
  awk -F= -v key="$key" '
    $1 == key {
      value = $0
      sub(/^[^=]*=/, "", value)
      if (value ~ /^".*"$/) { value = substr(value, 2, length(value) - 2) }
      found = 1
    }
    END { if (found) print value }
  ' "$file"
}

# --- message ----------------------------------------------------------------

# Pairs of <quote>|<attribution>, picked at random for the default body.
pick_quote() {
  local quotes=(
    "Simplicity is prerequisite for reliability.|Edsger W. Dijkstra"
    "Premature optimization is the root of all evil.|Donald Knuth"
    "Talk is cheap. Show me the code.|Linus Torvalds"
    "Make it work, make it right, make it fast.|Kent Beck"
    "The best way to predict the future is to invent it.|Alan Kay"
    "First, solve the problem. Then, write the code.|John Johnson"
    "Deleted code is debugged code.|Jeff Sickel"
    "Programs must be written for people to read, and only incidentally for machines to execute.|Harold Abelson"
    "Any fool can write code that a computer can understand. Good programmers write code that humans can understand.|Martin Fowler"
    "Simplicity is the soul of efficiency.|Austin Freeman"
    "Controlling complexity is the essence of computer programming.|Brian Kernighan"
    "A ship in port is safe, but that is not what ships are built for.|Grace Hopper"
  )
  local pick="${quotes[$((RANDOM % ${#quotes[@]}))]}"
  printf '%s\n  -- %s' "${pick%%|*}" "${pick##*|}"
}

valid_email() {
  [[ "$1" =~ ^[^[:space:]@]+@[^[:space:]@]+\.[^[:space:]@]+$ ]]
}

# Sends through python3's smtplib: STARTTLS, then SASL PLAIN/LOGIN, then the
# message. Values travel through the environment so nothing is interpolated
# into the program text.
send_message() {
  JMAIL_SEND_HOST="$server" \
    JMAIL_SEND_PORT="$port" \
    JMAIL_SEND_USER="$user" \
    JMAIL_SEND_PASSWORD="$password" \
    JMAIL_SEND_FROM="$from_addr" \
    JMAIL_SEND_TO="$to_addr" \
    JMAIL_SEND_SUBJECT="$subject" \
    JMAIL_SEND_BODY="$body" \
    JMAIL_SEND_DRY_RUN="$DRY_RUN" \
    python3 - <<'PY'
import os
import smtplib
import ssl
import sys
from email.message import EmailMessage


def detail(value):
    return value.decode("utf-8", "replace") if isinstance(value, bytes) else str(value)


host = os.environ["JMAIL_SEND_HOST"]
port = int(os.environ["JMAIL_SEND_PORT"])
user = os.environ.get("JMAIL_SEND_USER", "")
password = os.environ.get("JMAIL_SEND_PASSWORD", "")
sender = os.environ["JMAIL_SEND_FROM"]
recipient = os.environ["JMAIL_SEND_TO"]
subject = os.environ["JMAIL_SEND_SUBJECT"]
body = os.environ["JMAIL_SEND_BODY"]
dry_run = os.environ.get("JMAIL_SEND_DRY_RUN") == "1"

msg = EmailMessage()
msg["From"] = sender
msg["To"] = recipient
msg["Subject"] = subject
msg["X-Jmail-Test"] = "send-test-mail.sh"
msg.set_content(body)

if dry_run:
    sys.stdout.write(msg.as_string())
    sys.exit(0)

# jmail serves a self-signed certificate until one is supplied, so the chain is
# not verified here. What matters for a test is that STARTTLS happens and AUTH
# is accepted.
context = ssl.create_default_context()
context.check_hostname = False
context.verify_mode = ssl.CERT_NONE

try:
    with smtplib.SMTP(host, port, timeout=30) as smtp:
        smtp.ehlo()
        if not smtp.has_extn("starttls"):
            print("error: the server did not offer STARTTLS", file=sys.stderr)
            sys.exit(1)
        smtp.starttls(context=context)
        smtp.ehlo()
        if user:
            smtp.login(user, password)
        refused = smtp.sendmail(sender, [recipient], msg.as_string())
except smtplib.SMTPAuthenticationError as exc:
    print(f"error: authentication failed: {exc.smtp_code} {detail(exc.smtp_error)}", file=sys.stderr)
    sys.exit(1)
except smtplib.SMTPRecipientsRefused as exc:
    for addr, (code, why) in exc.recipients.items():
        print(f"error: {addr} refused: {code} {detail(why)}", file=sys.stderr)
    sys.exit(1)
except (smtplib.SMTPException, OSError, ssl.SSLError) as exc:
    print(f"error: {type(exc).__name__}: {exc}", file=sys.stderr)
    sys.exit(1)

if refused:
    for addr, (code, why) in refused.items():
        print(f"error: {addr} refused: {code} {detail(why)}", file=sys.stderr)
    sys.exit(1)

print(f"accepted by {host}:{port} for delivery to {recipient}")
PY
}

# --- main -------------------------------------------------------------------

main() {
  local env_file="$DEFAULT_ENV_FILE"
  local server="" port="" user="" password="" from_addr="" to_addr="" subject="" body=""
  local opt_server="" opt_port="" opt_user="" opt_password="" opt_from="" opt_to=""
  local opt_subject="" opt_body=""
  local origin="" hostname="" publish_port="" root_email="" smtp_user="" smtp_password="" domains_raw=""

  while [[ $# -gt 0 ]]; do
    case "$1" in
      --env)
        [[ $# -ge 2 ]] || die "--env needs a value"
        env_file="$2"
        shift 2
        ;;
      --env=*) env_file="${1#*=}"; shift ;;
      --server)
        [[ $# -ge 2 ]] || die "--server needs a value"
        opt_server="$2"
        shift 2
        ;;
      --server=*) opt_server="${1#*=}"; shift ;;
      --port)
        [[ $# -ge 2 ]] || die "--port needs a value"
        opt_port="$2"
        shift 2
        ;;
      --port=*) opt_port="${1#*=}"; shift ;;
      --user)
        [[ $# -ge 2 ]] || die "--user needs a value"
        opt_user="$2"
        shift 2
        ;;
      --user=*) opt_user="${1#*=}"; shift ;;
      --password)
        [[ $# -ge 2 ]] || die "--password needs a value"
        opt_password="$2"
        shift 2
        ;;
      --password=*) opt_password="${1#*=}"; shift ;;
      --from)
        [[ $# -ge 2 ]] || die "--from needs a value"
        opt_from="$2"
        shift 2
        ;;
      --from=*) opt_from="${1#*=}"; shift ;;
      --to)
        [[ $# -ge 2 ]] || die "--to needs a value"
        opt_to="$2"
        shift 2
        ;;
      --to=*) opt_to="${1#*=}"; shift ;;
      --subject)
        [[ $# -ge 2 ]] || die "--subject needs a value"
        opt_subject="$2"
        shift 2
        ;;
      --subject=*) opt_subject="${1#*=}"; shift ;;
      --body)
        [[ $# -ge 2 ]] || die "--body needs a value"
        opt_body="$2"
        shift 2
        ;;
      --body=*) opt_body="${1#*=}"; shift ;;
      -y | --yes) ASSUME_YES=1; shift ;;
      --dry-run) DRY_RUN=1; shift ;;
      -h | --help) usage; exit 0 ;;
      --) shift; break ;;
      *) die "unknown option: $1 (see --help)" ;;
    esac
  done

  need_cmd python3 || die "python3 not found"

  origin="$(env_read "$env_file" JMAIL_ORIGIN)"
  hostname="$(env_read "$env_file" JMAIL_HOSTNAME)"
  publish_port="$(env_read "$env_file" JMAIL_PUBLISH_PORT)"
  root_email="$(env_read "$env_file" JMAIL_ROOT_EMAIL)"
  smtp_user="$(env_read "$env_file" JMAIL_SMTP_USER)"
  smtp_password="$(env_read "$env_file" JMAIL_SMTP_PASSWORD)"
  domains_raw="$(env_read "$env_file" JMAIL_DOMAINS)"

  if ((ASSUME_YES == 0)); then
    printf '\n=== jmail test message ===\n\n' >&2
  fi

  # On the mail server the published port is reachable on loopback, and the
  # public hostname often is not: connecting to your own public IP depends on
  # NAT hairpinning, which many VPS networks drop.
  if [[ -n "$opt_server" ]]; then
    server="$opt_server"
  elif [[ -n "${JMAIL_TEST_SERVER:-}" ]]; then
    server="${JMAIL_TEST_SERVER}"
  elif [[ -f "$env_file" ]]; then
    server="127.0.0.1"
  else
    server="$hostname"
  fi
  server="$(ask "SMTP server" "$server")"
  [[ -n "$server" ]] || die "a server is required (--server)"

  port="${opt_port:-${JMAIL_TEST_PORT:-${publish_port:-587}}}"
  port="$(ask "Submission port" "$port")"
  [[ "$port" =~ ^[0-9]+$ ]] || die "invalid port: $port"

  user="${opt_user:-${JMAIL_TEST_USER:-${smtp_user:-jmail}}}"
  user="$(ask "SASL username" "$user")"

  password="${opt_password:-${JMAIL_TEST_PASSWORD:-${smtp_password}}}"
  if [[ -n "$password" ]]; then
    password="$(ask_secret "SASL password (Enter to use the one from $env_file)" "$password")"
  else
    password="$(ask_secret "SASL password" "")"
  fi
  [[ -n "$password" ]] || die "a SASL password is required"

  from_addr="${opt_from:-${JMAIL_TEST_SENDER:-${origin:+noreply@$origin}}}"
  from_addr="$(ask_required "Send from" "$from_addr")"
  valid_email "$from_addr" || die "not an email address: $from_addr"

  to_addr="${opt_to:-${JMAIL_TEST_RECEIVER:-${root_email}}}"
  to_addr="$(ask_required "Send to" "$to_addr")"
  valid_email "$to_addr" || die "not an email address: $to_addr"

  subject="${opt_subject:-"jmail test $(date -u '+%Y-%m-%d %H:%M:%S UTC')"}"
  subject="$(ask "Subject" "$subject")"

  body="${opt_body:-}"
  body="$(ask "Body (Enter for a random quote)" "$body")"
  if [[ -z "$body" ]]; then
    body="$(pick_quote)"
  fi

  printf '\n  server  : %s:%s (STARTTLS)\n' "$server" "$port" >&2
  printf '  from    : %s\n' "$from_addr" >&2
  printf '  to      : %s\n' "$to_addr" >&2
  printf '  subject : %s\n' "$subject" >&2
  printf '  body    : %s\n\n' "${body%%$'\n'*}" >&2

  # A From: address outside JMAIL_DOMAINS is still accepted by Postfix, but
  # OpenDKIM has no key for it, so it goes out unsigned. Say so before sending.
  if [[ -n "$origin" ]]; then
    local from_domain="${from_addr##*@}" known=0 d
    if [[ "$from_domain" == "$origin" ]]; then
      known=1
    fi
    # Word splitting is intended: JMAIL_DOMAINS is comma or space separated.
    for d in ${domains_raw//,/ }; do
      if [[ "$(printf '%s' "$d" | tr '[:upper:]' '[:lower:]')" == "${from_domain,,}" ]]; then
        known=1
      fi
    done
    if ((known == 0)); then
      warn "$from_domain is not in JMAIL_DOMAINS, so this message will not be DKIM-signed"
    fi
  fi

  if ((DRY_RUN)); then
    printf 'Dry run: not connecting to %s:%s\n\n' "$server" "$port" >&2
  elif ! ask_yn "Send this message" y; then
    printf 'Nothing sent.\n' >&2
    exit 0
  fi

  send_message || {
    printf '\nThe server did not accept the message.\n' >&2
    exit 1
  }

  if ((DRY_RUN)); then
    exit 0
  fi

  printf '\nCheck the delivered message headers for a DKIM-Signature from %s.\n' "${origin:-your domain}"
  printf 'If it does not arrive, run ./scripts/check-dns.sh and compare the SPF, DKIM and DMARC lines.\n'
}

if [[ -z "${BASH_SOURCE[0]:-}" ]] || [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  main "$@"
fi
