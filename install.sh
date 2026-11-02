#!/usr/bin/env bash
#
# Interactive installer for jmail.
#
# jmail runs as a container, so installing it means collecting the settings,
# writing a rootless Podman quadlet plus a 0600 environment file into the
# invoking user's XDG directories, and starting it with `systemctl --user`.
# Everything this script writes lives under $HOME.
#
#   ./install.sh                  interactive; asks whether to reconfigure or update
#   ./install.sh --reconfigure    change settings, keep data and DKIM keys
#   ./install.sh --update         pull the image and restart, keep settings
#   ./install.sh --yes            accept defaults, values come from JMAIL_* env vars
#   ./install.sh --generate-only  write the files, do not start anything
#
# Every prompt is pre-seeded from the same variables the container reads
# (JMAIL_HOSTNAME, JMAIL_ORIGIN, JMAIL_DOMAINS, ...), so the script can also be
# driven from a provisioning tool instead of a terminal.

set -euo pipefail

readonly UNIT_NAME="jmail"
readonly DEFAULT_IMAGE="ghcr.io/ffimnsr/jmail:latest"
readonly DEFAULT_PUBLISH_PORT="587"
# Keys install.sh owns. Anything else in the environment file is preserved, so
# hand-added overrides (JMAIL_TLS_LEVEL, JMAIL_HTTP_ALLOWED_FROM, ...) survive a
# reconfigure.
readonly MANAGED_ENV_KEYS="JMAIL_HOSTNAME JMAIL_ORIGIN JMAIL_DOMAINS JMAIL_ROOT_EMAIL JMAIL_SMTP_USER JMAIL_SMTP_PASSWORD JMAIL_MYNETWORKS JMAIL_HTTP_TOKEN JMAIL_IMAGE JMAIL_PUBLISH_PORT JMAIL_HTTP_PUBLISH_BIND JMAIL_HTTP_PUBLISH_PORT"

DATA_DIR="${XDG_DATA_HOME:-$HOME/.local/share}/jmail"
CONFIG_DIR="${XDG_CONFIG_HOME:-$HOME/.config}/jmail"
UNIT_DIR="${XDG_CONFIG_HOME:-$HOME/.config}/containers/systemd"
ENV_FILE="${CONFIG_DIR}/jmail.env"
UNIT_FILE="${UNIT_DIR}/jmail.container"

IMAGE="${JMAIL_IMAGE:-}"
ACTION=""
ASSUME_YES=0
GENERATE_ONLY=0
# Sections selected for a reconfigure; see choose_sections.
SECTIONS=""
# Set once the service is up, so the summary can read the generated DKIM keys.
READ_DKIM=0

usage() {
  cat <<EOF
Install, reconfigure or update jmail as a rootless Podman service for the current user.

Usage: install.sh [options]

Options:
  -y, --yes             Do not prompt; take defaults and JMAIL_* environment values.
      --reconfigure     Change settings, keeping the data directory and DKIM keys.
                        Asks which sections to change, then whether to pull a newer
                        image before restarting.
      --update          Pull the image and restart the service, without changing settings.
      --generate-only   Write the unit and environment file, but do not touch systemd.
      --image <ref>     Container image. Default: ${DEFAULT_IMAGE}
  -h, --help            Show this help message.

Without --reconfigure or --update, an existing installation asks which of the two you
want. A fresh machine is always installed from scratch. With --yes, reconfigure keeps
the local image; use --update to pull.

Prompts, and the variable each one sets:
  JMAIL_ORIGIN           Primary sending domain.
  JMAIL_HOSTNAME         Mail server hostname, announced in the SMTP banner.
  JMAIL_DOMAINS          Additional sending domains (single vs. multiple is asked).
  JMAIL_ROOT_EMAIL       Mailbox that receives the built-in test message.
  JMAIL_SMTP_USER        SASL submission username.
  JMAIL_SMTP_PASSWORD    SASL submission password. Generated when empty.
  JMAIL_PUBLISH_PORT     Host port published as container port 587.
  JMAIL_MYNETWORKS       Networks allowed to relay without authentication.
  JMAIL_HTTP_TOKEN       Enables the HTTP API. A 33-character token is generated.
  JMAIL_HTTP_PUBLISH_BIND  Interface for the HTTP API. Default: 127.0.0.1
  JMAIL_HTTP_PUBLISH_PORT  Host port published as container port 8080.
  JMAIL_HTTP_SUPABASE_<N>_HOOK_SECRET
                           Supabase Auth email hook, one numbered set per project.
  JMAIL_IMAGE              Container image, remembered across runs.
  JMAIL_ENABLE_LINGER      Set to 1 to enable systemd lingering in --yes mode.

Files written:
  ${UNIT_FILE}
  ${ENV_FILE}
  ${DATA_DIR}/{postfix-ssl,opendkim-keys,spool,postfix.d,init.d,http-state}

Reconfiguring asks which sections to change and reuses the values in ${ENV_FILE} as
defaults, so it is safe to run to change one setting. Updating leaves that file and the
data directory untouched.
EOF
}

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

parse_args() {
  while (($# > 0)); do
    case "$1" in
      -y|--yes)
        ASSUME_YES=1
        shift
        ;;
      --generate-only)
        GENERATE_ONLY=1
        shift
        ;;
      --reconfigure)
        [[ -z "$ACTION" || "$ACTION" == "reconfigure" ]] || die "--reconfigure conflicts with --update"
        ACTION="reconfigure"
        shift
        ;;
      --update)
        [[ -z "$ACTION" || "$ACTION" == "update" ]] || die "--update conflicts with --reconfigure"
        ACTION="update"
        shift
        ;;
      --image)
        [[ $# -ge 2 ]] || die "--image needs a value"
        IMAGE="$2"
        shift 2
        ;;
      --image=*)
        IMAGE="${1#*=}"
        shift
        ;;
      -h|--help)
        usage
        exit 0
        ;;
      *)
        die "unknown option: $1 (see --help)"
        ;;
    esac
  done
}

# --- reading existing configuration -----------------------------------------

# Reads a value from the environment file this script writes. Older versions
# stored every value double quoted, so the surrounding quotes are stripped to
# keep such files readable; current files are written verbatim.
env_get() {
  local key="$1"
  [[ -f "$ENV_FILE" ]] || return 0
  awk -F= -v key="$key" '
    $1 == key {
      value = $0
      sub(/^[^=]*=/, "", value)
      if (value ~ /^".*"$/) {
        value = substr(value, 2, length(value) - 2)
      }
      found = 1
    }
    END { if (found) print value }
  ' "$ENV_FILE"
}

# Environment first, then the previous install, so an operator can override a
# single value for one run without editing the file.
env_value() {
  local key="$1" value
  value="${!key-}"
  if [[ -n "$value" ]]; then
    printf '%s' "$value"
    return 0
  fi
  env_get "$key"
}

# Values go into the environment file verbatim. Podman reads the file with
# --env-file, which takes everything after the first `=` literally and does not
# strip quotes, so quoting here would reach the container as part of the value.
# A newline cannot be represented in the file, so reject it instead of silently
# truncating.
env_literal() {
  local value="$1"
  case "$value" in
    *$'\n'*) die "environment file values must be a single line" ;;
  esac
  printf '%s' "$value"
}

# Rewrites one key in the environment file, keeping every other line as it is.
set_env_value() {
  local key="$1" value="$2" tmp
  tmp="$(mktemp)"
  # The line is passed through the environment: awk -v would interpret
  # backslashes in the value as escape sequences.
  ENV_LINE="$key=$(env_literal "$value")" awk -F= -v key="$key" '
    $1 == key {
      if (!done) { print ENVIRON["ENV_LINE"]; done = 1 }
      next
    }
    { print }
    END { if (!done) print ENVIRON["ENV_LINE"] }
  ' "$ENV_FILE" >"$tmp"
  chmod 0600 "$tmp"
  mv "$tmp" "$ENV_FILE"
}

# Everything in the previous environment file that install.sh does not manage:
# comments and blank lines are dropped, keys it rewrites are dropped, the rest
# is carried over so custom overrides are not lost on a reconfigure.
unmanaged_env_lines() {
  [[ -f "$ENV_FILE" ]] || return 0
  awk -F= -v managed="$MANAGED_ENV_KEYS" '
    BEGIN { count = split(managed, keys, " ") }
    /^[[:space:]]*#/ { next }
    /^[[:space:]]*$/ { next }
    # Only carry over well-formed assignments; anything else would make systemd
    # reject the environment file.
    !/^[[:space:]]*[A-Za-z_][A-Za-z0-9_]*[[:space:]]*=/ { next }
    {
      key = $1
      gsub(/^[[:space:]]+/, "", key)
      gsub(/[[:space:]]+$/, "", key)
      # Every Supabase hook key, numbered or not, is rewritten by install.sh.
      if (key ~ /^JMAIL_HTTP_SUPABASE_/) { next }
      for (i = 1; i <= count; i++) {
        if (key == keys[i]) { next }
      }
      print
    }
  ' "$ENV_FILE"
}

# The image to use: --image, then JMAIL_IMAGE, then the previous install, then
# the default. Shared by the install and update paths so they cannot disagree.
resolve_image() {
  local stored
  if [[ -n "$IMAGE" ]]; then
    printf '%s' "$IMAGE"
    return 0
  fi
  stored="$(env_get JMAIL_IMAGE)"
  if [[ -n "$stored" ]]; then
    printf '%s' "$stored"
    return 0
  fi
  printf '%s' "$DEFAULT_IMAGE"
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
  IFS= read -r answer </dev/tty || die "no input available (use --yes for a non-interactive install)"
  printf '%s' "${answer:-$default}"
}

ask_required() {
  local label="$1" default="${2-}" answer
  while :; do
    # errexit is cleared inside command substitutions, so propagate the
    # failure explicitly instead of relying on `set -e` here.
    answer="$(ask "$label" "$default")" || exit 1
    if [[ -n "$answer" ]]; then
      printf '%s' "$answer"
      return 0
    fi
    if ((ASSUME_YES)); then
      die "$label is required; set the matching JMAIL_* variable or run interactively"
    fi
    printf '  a value is required\n' >&2
  done
}

ask_yn() {
  local label="$1" default="${2:-n}" answer
  if ((ASSUME_YES)); then
    case "$default" in
      y|Y|yes|YES) return 0 ;;
      *) return 1 ;;
    esac
  fi
  while :; do
    answer="$(ask "$label (y/n)" "$default")" || exit 1
    case "$(printf '%s' "$answer" | tr '[:upper:]' '[:lower:]')" in
      y|yes) return 0 ;;
      n|no) return 1 ;;
      *) printf '  answer y or n\n' >&2 ;;
    esac
  done
}

ask_secret() {
  local label="$1" default="$2" answer=""
  if ((ASSUME_YES)); then
    printf '%s' "$default"
    return 0
  fi
  printf '%s: ' "$label" >&2
  IFS= read -rs answer </dev/tty || die "no input available (use --yes for a non-interactive install)"
  printf '\n' >&2
  printf '%s' "${answer:-$default}"
}

normalize_domain() {
  local domain
  domain="$(printf '%s' "$1" | tr '[:upper:]' '[:lower:]')"
  # Trim the ends only: an internal space is a typo and must fail validation
  # instead of being silently glued into a different domain.
  domain="${domain#"${domain%%[![:space:]]*}"}"
  domain="${domain%"${domain##*[![:space:]]}"}"
  printf '%s' "$domain"
}

# Same shape the container accepts for JMAIL_ORIGIN and JMAIL_DOMAINS.
valid_domain() {
  [[ "$1" =~ ^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$ ]]
}

ask_domain() {
  local label="$1" default="${2-}" answer
  while :; do
    answer="$(ask_required "$label" "$default")" || exit 1
    answer="$(normalize_domain "$answer")"
    if valid_domain "$answer"; then
      printf '%s' "$answer"
      return 0
    fi
    if ((ASSUME_YES)); then
      die "invalid domain: $answer"
    fi
    printf '  %s is not a valid domain name\n' "$answer" >&2
  done
}

ask_email() {
  local label="$1" default="${2-}" answer
  while :; do
    answer="$(ask_required "$label" "$default")" || exit 1
    if [[ "$answer" =~ ^[^@[:space:]]+@[^@[:space:]]+$ ]]; then
      printf '%s' "$answer"
      return 0
    fi
    if ((ASSUME_YES)); then
      die "invalid email address: $answer"
    fi
    printf '  %s is not a valid email address\n' "$answer" >&2
  done
}

ask_port() {
  local label="$1" default="$2" answer number
  while :; do
    answer="$(ask "$label" "$default")" || exit 1
    if [[ "$answer" =~ ^[0-9]+$ ]]; then
      number=$((10#$answer))
      if ((number >= 1 && number <= 65535)); then
        printf '%s' "$number"
        return 0
      fi
    fi
    if ((ASSUME_YES)); then
      die "invalid port: $answer"
    fi
    printf '  enter a port between 1 and 65535\n' >&2
  done
}

generate_secret() {
  local bytes="${1:-16}"
  if need_cmd openssl; then
    openssl rand -hex "$bytes"
  elif need_cmd od; then
    head -c "$bytes" /dev/urandom | od -An -tx1 | tr -d ' \n'
  else
    die "openssl or od is required to generate secrets"
  fi
}

# A bearer token is pasted into an HTTP header, so keep it to [A-Za-z0-9] and
# avoid quoting problems in the environment file.
generate_token() {
  local length="${1:-33}" token
  if need_cmd openssl; then
    token="$(openssl rand -base64 48 | tr -dc 'A-Za-z0-9' | cut -c "1-${length}")"
  elif need_cmd base64; then
    token="$(head -c 48 /dev/urandom | base64 | tr -dc 'A-Za-z0-9' | cut -c "1-${length}")"
  else
    die "openssl or base64 is required to generate the HTTP API token"
  fi
  if [[ "${#token}" -ne "$length" ]]; then
    die "could not generate a ${length}-character HTTP API token"
  fi
  printf '%s' "$token"
}

# Pulls an image and reports the version label the release workflow sets. The
# unit uses Pull=missing, so this is the only thing that fetches images.
pull_image() {
  local image="$1" version
  printf '\nPulling %s...\n' "$image"
  if ! podman pull "$image"; then
    return 1
  fi
  version="$(podman image inspect --format '{{ index .Labels "org.opencontainers.image.version" }}' "$image" 2>/dev/null || true)"
  if [[ -n "$version" ]] && [[ "$version" != "<no value>" ]]; then
    printf 'Image version: %s\n' "$version"
  fi
  return 0
}

# Adds an item to a comma separated list when it is not already there.
list_add() {
  local list="$1" item="$2"
  case ",$list," in
    *",$item,"*) printf '%s' "$list" ;;
    *) printf '%s' "${list:+$list,}$item" ;;
  esac
}

# --- modes ------------------------------------------------------------------

# --- reconfiguration sections ------------------------------------------------

section_wanted() {
  [[ " $SECTIONS " == *" $1 "* ]]
}

# Only a reconfigure asks. A fresh install and --yes change everything, which is
# what they did before this menu existed.
choose_sections() {
  SECTIONS="1 2 3 4 5 6"
  if [[ "$ACTION" != "reconfigure" ]] || ((ASSUME_YES)); then
    return 0
  fi

  printf '\nWhat do you want to reconfigure? Anything you leave out keeps the value\nfrom %s.\n' "$ENV_FILE" >&2
  printf '  1) domains and hostname\n' >&2
  printf '  2) mailbox and SMTP credentials\n' >&2
  printf '  3) ports and networks\n' >&2
  printf '  4) HTTP API\n' >&2
  printf '  5) Supabase Auth email hook\n' >&2
  printf '  6) image\n' >&2

  local answer token picked ok
  while :; do
    answer="$(ask "Sections to change (for example 2 4, blank for all)" "all")" || exit 1
    if [[ -z "$answer" || "$answer" == "all" ]]; then
      SECTIONS="1 2 3 4 5 6"
      return 0
    fi
    picked=""
    ok=1
    for token in ${answer//,/ }; do
      case "$token" in
        1 | 2 | 3 | 4 | 5 | 6)
          case " $picked " in
            *" $token "*) ;;
            *) picked="${picked:+$picked }$token" ;;
          esac
          ;;
        *) ok=0 ;;
      esac
    done
    if ((ok)) && [[ -n "$picked" ]]; then
      SECTIONS="$picked"
      return 0
    fi
    printf '  enter numbers from 1 to 6, separated by spaces or commas\n' >&2
  done
}

# Mirrors the base64 forms the container accepts for a hook secret.
valid_hook_secret() {
  local value="$1"
  value="${value#v1,}"
  value="${value#whsec_}"
  [[ -n "$value" ]] || return 1
  need_cmd base64 || return 0
  # GNU base64 wants padding; the container accepts unpadded base64 too.
  case $((${#value} % 4)) in
    1) return 1 ;;
    2) value="${value}==" ;;
    3) value="${value}=" ;;
  esac
  printf '%s' "$value" | base64 -d >/dev/null 2>&1
}

# Reads the Supabase projects already configured into sb_count, sb_secret,
# sb_url and sb_from. The unnumbered variables describe one project and are
# migrated to the indexed form on the next write.
load_supabase_projects() {
  local secret i
  sb_count=0
  sb_secret=()
  sb_url=()
  sb_from=()

  secret="$(env_value JMAIL_HTTP_SUPABASE_HOOK_SECRET)"
  if [[ -n "$secret" ]]; then
    sb_secret[0]="$secret"
    sb_url[0]="$(env_value JMAIL_HTTP_SUPABASE_URL)"
    sb_from[0]="$(env_value JMAIL_HTTP_SUPABASE_FROM)"
    sb_count=1
  fi

  for ((i = 1; i <= 16; i++)); do
    secret="$(env_value "JMAIL_HTTP_SUPABASE_${i}_HOOK_SECRET")"
    [[ -n "$secret" ]] || continue
    sb_secret[sb_count]="$secret"
    sb_url[sb_count]="$(env_value "JMAIL_HTTP_SUPABASE_${i}_URL")"
    sb_from[sb_count]="$(env_value "JMAIL_HTTP_SUPABASE_${i}_FROM")"
    sb_count=$((sb_count + 1))
  done
}

# Prompts for the Supabase projects and rewrites sb_*. Only called when the HTTP
# API is enabled, because the hook is served by it.
configure_supabase_projects() {
  local count_default="$sb_count" count default_secret default_url default_from
  local secret url from i
  local -a next_secret=() next_url=() next_from=()

  while :; do
    count="$(ask "Supabase projects to configure (0 disables the hook)" "$count_default")" || exit 1
    if [[ "$count" =~ ^[0-9]+$ ]] && ((10#$count <= 16)); then
      count=$((10#$count))
      break
    fi
    if ((ASSUME_YES)); then
      die "invalid Supabase project count: $count"
    fi
    printf '  enter a number between 0 and 16\n' >&2
  done

  if ((count == 0)); then
    if ((sb_count > 0)); then
      printf 'The Supabase Auth hook will be disabled.\n' >&2
    fi
    sb_count=0
    sb_secret=()
    sb_url=()
    sb_from=()
    return 0
  fi

  for ((i = 1; i <= count; i++)); do
    default_secret="${sb_secret[i - 1]:-}"
    default_url="${sb_url[i - 1]:-}"
    default_from="${sb_from[i - 1]:-}"

    while :; do
      secret="$(ask_secret "Project $i hook secret, v1,whsec_... (Enter keeps the current one)" "$default_secret")" || exit 1
      if valid_hook_secret "$secret"; then
        break
      fi
      if ((ASSUME_YES)); then
        die "project $i hook secret is not base64; copy it from the Supabase dashboard"
      fi
      printf '  that is not a base64 hook secret; copy it from the Supabase dashboard\n' >&2
    done

    url="$(ask "Project $i API URL (blank leaves the link out of the email)" "$default_url")" || exit 1
    if [[ -n "$url" ]] && [[ ! "$url" =~ ^https?://[^/]+ ]]; then
      warn "$url does not look like an http(s) URL; the container will refuse to start"
    fi
    url="${url%/}"

    from="$(ask_email "Project $i send from" "${default_from:-noreply@$origin}")" || exit 1
    # Leaving the origin default unset keeps following JMAIL_ORIGIN if it moves.
    if [[ "$from" == "noreply@$origin" ]]; then
      from=""
    fi

    next_secret[i - 1]="$secret"
    next_url[i - 1]="$url"
    next_from[i - 1]="$from"
  done

  sb_secret=("${next_secret[@]}")
  sb_url=("${next_url[@]}")
  sb_from=("${next_from[@]}")
  sb_count="$count"
}

choose_action() {
  if [[ -n "$ACTION" ]]; then
    return 0
  fi
  if [[ ! -f "$ENV_FILE" ]]; then
    ACTION="install"
    return 0
  fi
  if ((ASSUME_YES)) || ((GENERATE_ONLY)); then
    ACTION="reconfigure"
    return 0
  fi

  printf 'Found an existing configuration at %s.\n\n' "$ENV_FILE" >&2
  printf 'What do you want to do?\n' >&2
  printf '  1) Reconfigure (change settings, keep data and DKIM keys)\n' >&2
  printf '  2) Update (pull the image and restart, keep all settings)\n' >&2
  printf '  3) Cancel\n' >&2

  local answer
  while :; do
    answer="$(ask "Choose 1, 2 or 3" "1")" || exit 1
    case "$answer" in
      1) ACTION="reconfigure"; return 0 ;;
      2) ACTION="update"; return 0 ;;
      3) die "cancelled" ;;
      *) printf '  enter 1, 2 or 3\n' >&2 ;;
    esac
  done
}

wait_for_service() {
  local attempt
  for attempt in 1 2 3 4 5 6 7 8 9 10; do
    if systemctl --user is-active --quiet jmail.service; then
      return 0
    fi
    sleep 1
  done
  systemctl --user is-active --quiet jmail.service
}

restart_or_start() {
  if systemctl --user is-active --quiet jmail.service; then
    systemctl --user restart jmail.service
  else
    # Quadlet turns jmail.container into a generated unit, and systemd refuses
    # to enable a generated unit ("transient or generated"). It does not need
    # enabling: the [Install] WantedBy=default.target in the .container file is
    # what makes the generator pull it in at boot.
    systemctl --user start jmail.service
  fi
}

# Pulling before stopping anything means a failed pull leaves the running
# container and its queue untouched.
run_update() {
  [[ -f "$ENV_FILE" ]] || die "no configuration at $ENV_FILE; run ./install.sh to install first"
  [[ -f "$UNIT_FILE" ]] || die "no service unit at $UNIT_FILE; run ./install.sh to install first"

  local update_image unit_image
  update_image="$(ask_required "Image to update to" "$(resolve_image)")"

  if ! ask_yn "Pull ${update_image} and restart jmail" y; then
    die "cancelled"
  fi

  if ! pull_image "$update_image"; then
    die "podman pull failed; jmail is still running the current image"
  fi

  # An explicit --image changes what the unit runs; the rest of the unit stays.
  unit_image="$(sed -n 's/^Image=//p' "$UNIT_FILE" | sed -n '1p')"
  if [[ "$update_image" != "$unit_image" ]]; then
    sed -i "s|^Image=.*|Image=${update_image}|" "$UNIT_FILE"
    if grep -qxF "Image=${update_image}" "$UNIT_FILE"; then
      printf 'Unit now uses %s\n' "$update_image"
    else
      warn "could not rewrite the Image= line in $UNIT_FILE; set it to $update_image by hand"
    fi
  fi
  if [[ "$update_image" != "$(env_get JMAIL_IMAGE)" ]]; then
    set_env_value JMAIL_IMAGE "$update_image"
  fi

  systemctl --user daemon-reload || die "systemctl --user daemon-reload failed"
  if ! restart_or_start; then
    printf '\nCould not restart jmail.service. Check what happened with:\n  systemctl --user status jmail\n  journalctl --user -u jmail -n 50 --no-pager\n' >&2
    exit 1
  fi

  if wait_for_service; then
    printf 'jmail.service is running.\n'
  else
    warn "jmail.service is not active yet; check: systemctl --user status jmail"
  fi

  printf '\nUpdated jmail\n'
  printf '  image : %s\n' "$update_image"
  printf '  unit  : %s\n' "$UNIT_FILE"
  printf '  data  : %s (untouched)\n\n' "$DATA_DIR"
  printf 'Logs: journalctl --user -u jmail -f\n'
}

# --- main -------------------------------------------------------------------

main() {
  parse_args "$@"

  # The quadlet and environment file are written under $HOME and started with
  # the user manager, which root does not have in the same way. A system-scope
  # service is scripts/quadlet.sh --scope system.
  if ((EUID == 0)); then
    die "run install.sh as the user who should own the service, not as root (for a system service use scripts/quadlet.sh --scope system)"
  fi

  if ((!ASSUME_YES)) && [[ ! -r /dev/tty ]]; then
    die "no terminal available for the prompts; run with --yes and JMAIL_* variables set"
  fi

  if ((!GENERATE_ONLY)); then
    need_cmd podman || die "podman is required; install Podman 4.4 or newer first"
    need_cmd systemctl || die "systemd is required to run the generated user service"
  fi

  choose_action

  if [[ "$ACTION" == "update" ]] && ((GENERATE_ONLY)); then
    die "--update cannot be combined with --generate-only"
  fi

  if [[ "$ACTION" == "update" ]]; then
    run_update
    return 0
  fi

  printf '\n=== jmail installer ===\n\n' >&2
  if [[ "$ACTION" == "reconfigure" ]] && [[ -f "$ENV_FILE" ]]; then
    printf 'Reconfiguring the installation at %s; its values are the defaults below.\n\n' "$ENV_FILE" >&2
  fi

  choose_sections

  # Everything starts from what is on disk, so a section that is not selected is
  # written back unchanged instead of being walked through again.
  local origin hostname root_email smtp_user smtp_password
  local publish_port mynetworks
  local http_enabled=0 http_token="" http_bind="127.0.0.1" http_port="8080"
  local sb_count=0 i
  local -a sb_secret=() sb_url=() sb_from=()

  local extra_domains="" existing_domains_raw="" existing_extras="" seed
  local mode_default choice more root_email_default smtp_user_default
  local existing_password generated_password port_default networks_default
  local image_default do_pull=0

  origin="$(env_value JMAIL_ORIGIN)"
  hostname="$(env_value JMAIL_HOSTNAME)"
  root_email="$(env_value JMAIL_ROOT_EMAIL)"
  smtp_user="$(env_value JMAIL_SMTP_USER)"
  smtp_password="$(env_value JMAIL_SMTP_PASSWORD)"
  publish_port="$(env_value JMAIL_PUBLISH_PORT)"
  mynetworks="$(env_value JMAIL_MYNETWORKS)"

  # Additional domains, minus the origin, which is always signed.
  existing_domains_raw="$(env_value JMAIL_DOMAINS)"
  for seed in ${existing_domains_raw//,/ }; do
    seed="$(normalize_domain "$seed")"
    if [[ -n "$seed" ]] && [[ "$seed" != "$origin" ]]; then
      existing_extras="$(list_add "$existing_extras" "$seed")"
    fi
  done
  extra_domains="$existing_extras"

  http_token="$(env_value JMAIL_HTTP_TOKEN)"
  http_bind="$(env_value JMAIL_HTTP_PUBLISH_BIND)"
  [[ -n "$http_bind" ]] || http_bind="127.0.0.1"
  http_port="$(env_value JMAIL_HTTP_PUBLISH_PORT)"
  [[ -n "$http_port" ]] || http_port="8080"
  if [[ -n "$http_token" ]] || [[ -n "${JMAIL_HTTP_PUBLISH_PORT:-}" ]] ||
    [[ -n "$(env_get JMAIL_HTTP_PUBLISH_PORT)" ]]; then
    http_enabled=1
  fi

  load_supabase_projects
  if ((sb_count > 0)); then
    http_enabled=1
  fi

  # --- 1. domains and hostname ---------------------------------------------
  if section_wanted 1; then
    origin="$(ask_domain "Primary sending domain (JMAIL_ORIGIN)" "$origin")"
    hostname="$(ask_domain "Mail server hostname (JMAIL_HOSTNAME)" "${hostname:-mail.$origin}")"

    # Recompute the extras against the (possibly new) origin.
    existing_extras=""
    for seed in ${existing_domains_raw//,/ }; do
      seed="$(normalize_domain "$seed")"
      if [[ -n "$seed" ]] && [[ "$seed" != "$origin" ]]; then
        existing_extras="$(list_add "$existing_extras" "$seed")"
      fi
    done
    extra_domains="$existing_extras"

    mode_default="1"
    if [[ -n "$existing_extras" ]]; then
      mode_default="2"
    fi

    if ((ASSUME_YES)); then
      choice="$mode_default"
    else
      printf '\nWill jmail send mail from a single domain or from several domains?\n' >&2
      printf '  1) single domain   (%s only)\n' "$origin" >&2
      printf '  2) multiple domains (each one gets its own DKIM key)\n' >&2
      while :; do
        choice="$(ask "Choose 1 or 2" "$mode_default")" || exit 1
        case "$choice" in
          1 | 2) break ;;
          *) printf '  enter 1 or 2\n' >&2 ;;
        esac
      done
    fi

    if [[ "$choice" == "2" ]]; then
      if [[ -n "$existing_extras" ]]; then
        if ask_yn "Keep the existing additional domains (${existing_extras//,/ })" y; then
          extra_domains="$existing_extras"
        else
          extra_domains=""
        fi
      fi
      if ((!ASSUME_YES)); then
        printf 'Enter each additional sending domain. Leave it blank when you are done.\n' >&2
        while :; do
          more="$(ask "Additional domain (blank to finish)" "")" || exit 1
          if [[ -z "$more" ]]; then
            break
          fi
          more="$(normalize_domain "$more")"
          if ! valid_domain "$more"; then
            printf '  %s is not a valid domain name\n' "$more" >&2
            continue
          fi
          if [[ "$more" == "$origin" ]]; then
            printf '  %s is the primary domain and is always signed\n' "$more" >&2
            continue
          fi
          extra_domains="$(list_add "$extra_domains" "$more")"
        done
      fi
    else
      extra_domains=""
    fi
  fi

  # --- 2. mailbox and SMTP credentials -------------------------------------
  if section_wanted 2; then
    root_email_default="${root_email:-postmaster@$origin}"
    root_email="$(ask_email "Mailbox for test messages (JMAIL_ROOT_EMAIL)" "$root_email_default")"

    smtp_user_default="${smtp_user:-jmail}"
    smtp_user="$(ask_required "SMTP username (JMAIL_SMTP_USER)" "$smtp_user_default")"

    existing_password="$(env_get JMAIL_SMTP_PASSWORD)"
    if [[ -z "$smtp_password" ]]; then
      generated_password="$(generate_secret 16)"
      smtp_password="$(ask_secret "SMTP password for $smtp_user (Enter to use a generated one)" "$generated_password")"
    elif [[ -n "$existing_password" ]] && [[ -z "${JMAIL_SMTP_PASSWORD-}" ]]; then
      printf 'Reusing the SMTP password from %s.\n' "$ENV_FILE" >&2
    fi
  fi

  # --- 3. ports and networks -----------------------------------------------
  if section_wanted 3; then
    port_default="${publish_port:-$DEFAULT_PUBLISH_PORT}"
    publish_port="$(ask_port "Host port for SMTP submission (JMAIL_PUBLISH_PORT)" "$port_default")"

    networks_default="${mynetworks:-127.0.0.0/8}"
    mynetworks="$(ask_required "Networks that may relay without authentication (JMAIL_MYNETWORKS)" "$networks_default")"
  else
    [[ -n "$publish_port" ]] || publish_port="$DEFAULT_PUBLISH_PORT"
    [[ -n "$mynetworks" ]] || mynetworks="127.0.0.0/8"
  fi

  # --- 4. HTTP API ----------------------------------------------------------
  if section_wanted 4; then
    local http_default="n" token_default="y"
    if ((http_enabled)); then
      http_default="y"
    fi
    if [[ -z "$http_token" ]] && ((sb_count > 0)); then
      # A hook-only deployment does not need the bearer token.
      token_default="n"
    fi

    if ask_yn "Enable the jmail-http API" "$http_default"; then
      http_enabled=1
      if ask_yn "Require a bearer token for POST /v1/send" "$token_default"; then
        if [[ -z "$http_token" ]]; then
          http_token="$(generate_token 33)"
        fi
      else
        http_token=""
      fi

      http_bind="$(ask_required "Host interface for the HTTP API (JMAIL_HTTP_PUBLISH_BIND)" "$http_bind")"
      # PublishPort wants [addr] for IPv6. Accept a bare address and normalise it
      # once, so the value read back on the next run is already bracketed.
      if [[ "$http_bind" == *:* ]] && [[ "$http_bind" != \[*\] ]]; then
        http_bind="[$http_bind]"
      fi
      http_port="$(ask_port "Host port for the HTTP API (JMAIL_HTTP_PUBLISH_PORT)" "$http_port")"
    else
      http_enabled=0
      http_token=""
      if ((sb_count > 0)); then
        warn "the Supabase hook is served by the HTTP API; dropping $sb_count project(s)"
        sb_count=0
        sb_secret=()
        sb_url=()
        sb_from=()
      fi
    fi
  fi

  # --- 5. Supabase Auth email hook -----------------------------------------
  if section_wanted 5; then
    if ((http_enabled == 0)); then
      warn "the Supabase hook is served by the HTTP API; include section 4 to configure it"
    else
      configure_supabase_projects
    fi
  fi

  # --- 6. image -------------------------------------------------------------
  if section_wanted 6; then
    image_default="$(resolve_image)"
    IMAGE="$(ask_required "Container image" "$image_default")"

    # The unit uses Pull=missing, so this decides whether a newer image is fetched
    # before the service restarts.
    if ((!GENERATE_ONLY)); then
      if [[ "$ACTION" == "install" ]]; then
        do_pull=1
      else
        local pull_default="y"
        if ((ASSUME_YES)); then
          # --yes must not surprise automation with a registry call; --update is
          # the explicit way to pull.
          pull_default="n"
        fi
        if ask_yn "Pull ${IMAGE} before restarting, in case a newer image is published" "$pull_default"; then
          do_pull=1
        fi
      fi
    fi
  else
    IMAGE="$(resolve_image)"
  fi

  # Enabling the API with neither a token nor a project would leave the service
  # with nothing to serve, which the container refuses to start with.
  if ((http_enabled)) && [[ -z "$http_token" ]] && ((sb_count == 0)); then
    warn "the HTTP API was enabled without a bearer token or a Supabase project; generating a token"
    http_token="$(generate_token 33)"
  fi

  # The container refuses to start when a hook sender is outside the allowed
  # sender domains, so catch it here, before the service restart fails.
  if ((sb_count > 0)); then
    local hook_from hook_domain allowed_raw allowed_domain hook_allowed
    allowed_raw="$(env_value JMAIL_HTTP_ALLOWED_FROM)"
    for ((i = 0; i < sb_count; i++)); do
      hook_from="${sb_from[i]:-}"
      [[ -n "$hook_from" ]] || continue
      hook_domain="$(printf '%s' "${hook_from##*@}" | tr '[:upper:]' '[:lower:]')"
      hook_allowed=0
      if [[ "$allowed_raw" == "*" ]] || [[ "$hook_domain" == "$origin" ]] ||
        [[ ",${extra_domains}," == *",${hook_domain},"* ]]; then
        hook_allowed=1
      fi
      # Word splitting is intended: JMAIL_HTTP_ALLOWED_FROM is comma or space separated.
      for allowed_domain in ${allowed_raw//,/ }; do
        if [[ "$(printf '%s' "$allowed_domain" | tr '[:upper:]' '[:lower:]')" == "$hook_domain" ]]; then
          hook_allowed=1
        fi
      done
      if ((hook_allowed == 0)); then
        die "Supabase project $((i + 1)) sends from $hook_from, which is not in JMAIL_ORIGIN, JMAIL_DOMAINS or JMAIL_HTTP_ALLOWED_FROM; add the domain or change the sender"
      fi
    done
  fi

  printf '\nSummary\n' >&2
  printf '  image      : %s\n' "$IMAGE" >&2
  printf '  hostname   : %s\n' "$hostname" >&2
  printf '  origin     : %s\n' "$origin" >&2
  if [[ -n "$extra_domains" ]]; then
    printf '  domains    : %s\n' "${extra_domains//,/ }" >&2
  fi
  printf '  root email : %s\n' "$root_email" >&2
  printf '  SMTP user  : %s\n' "$smtp_user" >&2
  printf '  SMTP port  : %s\n' "$publish_port" >&2
  if ((do_pull)); then
    if [[ "$ACTION" == "install" ]]; then
      printf '  image pull : before start\n' >&2
    else
      printf '  image pull : before restart\n' >&2
    fi
  fi
  if ((http_enabled)); then
    if [[ -n "$http_token" ]]; then
      printf '  HTTP API   : %s:%s (bearer token)\n' "$http_bind" "$http_port" >&2
    else
      printf '  HTTP API   : %s:%s (Supabase hook only)\n' "$http_bind" "$http_port" >&2
    fi
  else
    printf '  HTTP API   : disabled\n' >&2
  fi
  if ((sb_count > 0)); then
    local project project_from project_url
    printf '  Supabase   : %s project(s)\n' "$sb_count" >&2
    for ((i = 0; i < sb_count; i++)); do
      project=$((i + 1))
      project_from="${sb_from[i]:-}"
      project_url="${sb_url[i]:-}"
      [[ -n "$project_from" ]] || project_from="noreply@$origin (default)"
      [[ -n "$project_url" ]] || project_url="unset (code-only emails, no links)"
      printf '    %-9s : from %s\n' "project $project" "$project_from" >&2
      printf '    %-9s   url  %s\n' "" "$project_url" >&2
    done
  fi
  printf '  data dir   : %s\n\n' "$DATA_DIR" >&2

  local confirm_label="Write the files and start jmail"
  if ((GENERATE_ONLY)); then
    confirm_label="Write the files"
  elif [[ "$ACTION" == "reconfigure" ]]; then
    confirm_label="Write the files and restart jmail"
  fi
  if ! ask_yn "$confirm_label" y; then
    die "cancelled"
  fi

  # Pull before writing anything, so a failed pull leaves the previous install
  # exactly as it was. A fresh install may be running offline against an image
  # that is already in the local store, so that case falls back instead.
  if ((do_pull)); then
    if ! pull_image "$IMAGE"; then
      if [[ "$ACTION" == "install" ]] && podman image exists "$IMAGE" >/dev/null 2>&1; then
        warn "could not pull $IMAGE; continuing with the local copy"
      else
        die "podman pull failed; nothing was changed"
      fi
    fi
  fi

  mkdir -p "$CONFIG_DIR" "$UNIT_DIR" \
    "$DATA_DIR/postfix-ssl" "$DATA_DIR/opendkim-keys" "$DATA_DIR/spool" \
    "$DATA_DIR/postfix.d" "$DATA_DIR/init.d" "$DATA_DIR/http-state"

  # Captured before the file is rewritten below.
  local preserved_env=""
  preserved_env="$(unmanaged_env_lines)"

  (
    umask 077
    {
      printf '# Generated by jmail install.sh on %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
      printf 'JMAIL_HOSTNAME=%s\n' "$(env_literal "$hostname")"
      printf 'JMAIL_ORIGIN=%s\n' "$(env_literal "$origin")"
      if [[ -n "$extra_domains" ]]; then
        printf 'JMAIL_DOMAINS=%s\n' "$(env_literal "$extra_domains")"
      fi
      printf 'JMAIL_ROOT_EMAIL=%s\n' "$(env_literal "$root_email")"
      printf 'JMAIL_SMTP_USER=%s\n' "$(env_literal "$smtp_user")"
      printf 'JMAIL_SMTP_PASSWORD=%s\n' "$(env_literal "$smtp_password")"
      printf 'JMAIL_MYNETWORKS=%s\n' "$(env_literal "$mynetworks")"
      if ((http_enabled)) && [[ -n "$http_token" ]]; then
        printf 'JMAIL_HTTP_TOKEN=%s\n' "$(env_literal "$http_token")"
      fi
      # Installer state, read back by the next run. The container ignores these.
      printf 'JMAIL_IMAGE=%s\n' "$(env_literal "$IMAGE")"
      printf 'JMAIL_PUBLISH_PORT=%s\n' "$(env_literal "$publish_port")"
      if ((http_enabled)); then
        printf 'JMAIL_HTTP_PUBLISH_BIND=%s\n' "$(env_literal "$http_bind")"
        printf 'JMAIL_HTTP_PUBLISH_PORT=%s\n' "$(env_literal "$http_port")"
      fi
      if ((http_enabled && sb_count > 0)); then
        for ((i = 1; i <= sb_count; i++)); do
          printf 'JMAIL_HTTP_SUPABASE_%d_HOOK_SECRET=%s\n' "$i" "$(env_literal "${sb_secret[i - 1]}")"
          if [[ -n "${sb_url[i - 1]:-}" ]]; then
            printf 'JMAIL_HTTP_SUPABASE_%d_URL=%s\n' "$i" "$(env_literal "${sb_url[i - 1]}")"
          fi
          if [[ -n "${sb_from[i - 1]:-}" ]]; then
            printf 'JMAIL_HTTP_SUPABASE_%d_FROM=%s\n' "$i" "$(env_literal "${sb_from[i - 1]}")"
          fi
        done
      fi
      if [[ -n "$preserved_env" ]]; then
        printf '# Preserved from the previous install; install.sh does not manage these.\n'
        printf '%s\n' "$preserved_env"
      fi
    } >"$ENV_FILE"
  )
  chmod 0600 "$ENV_FILE"

  local http_publish_line="" health_cmd="nc -z 127.0.0.1 587"
  if ((http_enabled)); then
    http_publish_line="PublishPort=${http_bind}:${http_port}:8080"
    health_cmd="nc -z 127.0.0.1 587 && nc -z 127.0.0.1 8080"
  fi

  cat >"$UNIT_FILE" <<EOF
[Unit]
Description=jmail outbound SMTP sender
Wants=network-online.target
After=network-online.target

[Container]
ContainerName=${UNIT_NAME}
Image=${IMAGE}
# install.sh pulls explicitly (on install, on --update, and when reconfigure is
# asked to), so a plain restart uses the local image instead of silently
# upgrading it.
Pull=missing
HostName=${hostname}
EnvironmentFile=${ENV_FILE}
Volume=${DATA_DIR}/postfix-ssl:/etc/postfix/ssl:Z
Volume=${DATA_DIR}/opendkim-keys:/etc/opendkim/keys:Z
Volume=${DATA_DIR}/spool:/var/spool/postfix:Z
Volume=${DATA_DIR}/postfix.d:/etc/jmail/postfix.d:Z
Volume=${DATA_DIR}/init.d:/etc/jmail/init.d:Z
Volume=${DATA_DIR}/http-state:/var/lib/jmail-http:Z
PublishPort=${publish_port}:587
${http_publish_line}
# Podman does not carry the image HEALTHCHECK into the container, so it is
# declared here. 127.0.0.1 is explicit because Postfix runs ipv4-only.
HealthCmd=${health_cmd}
HealthInterval=30s
HealthRetries=3
HealthStartPeriod=30s
HealthTimeout=5s

[Service]
Restart=always
# Pull=missing only pulls when the image is absent, so this covers that first
# pull on a cold cache.
TimeoutStartSec=300

[Install]
WantedBy=default.target
EOF
  chmod 0644 "$UNIT_FILE"

  printf '\nWrote %s\nWrote %s\n' "$ENV_FILE" "$UNIT_FILE"

  if ((GENERATE_ONLY)); then
    printf '\nStart it with:\n  systemctl --user daemon-reload\n  systemctl --user start jmail.service\n'
    printf 'If it is already running, use `systemctl --user restart jmail.service` instead.\n'
    print_summary "$origin" "$extra_domains" "$hostname" "$publish_port" \
      "$smtp_user" "$smtp_password" "$http_enabled" "$http_bind" "$http_port" "$http_token" "$sb_count"
    exit 0
  fi

  printf '\nApplying the configuration...\n'
  systemctl --user daemon-reload || die "systemctl --user daemon-reload failed"
  # A running unit keeps the environment it started with, so reconfiguring has
  # to restart it; a plain start would be a no-op on an active service.
  if ! restart_or_start; then
    printf '\nCould not start jmail.service. Check what happened with:\n  systemctl --user status jmail\n  journalctl --user -u jmail -n 50 --no-pager\n' >&2
    printf 'If the failure is about the port, re-run the installer with a higher JMAIL_PUBLISH_PORT (for example 10587).\n' >&2
    exit 1
  fi

  if need_cmd loginctl && need_cmd sudo; then
    local linger linger_default="y"
    if ((ASSUME_YES)); then
      # Do not run sudo behind an operator's back: --yes only enables lingering
      # when JMAIL_ENABLE_LINGER=1 asks for it.
      linger_default="n"
    fi
    if [[ "${JMAIL_ENABLE_LINGER:-0}" == "1" ]]; then
      linger_default="y"
    fi
    linger="$(loginctl show-user "$(id -un)" -p Linger 2>/dev/null || true)"
    if [[ "$linger" != "Linger=yes" ]]; then
      if ask_yn "Enable lingering so jmail keeps running after you log out (runs sudo)" "$linger_default"; then
        if ! sudo loginctl enable-linger "$(id -un)"; then
          warn "could not enable lingering; jmail stops when your session ends"
        fi
      fi
    fi
  fi

  if wait_for_service; then
    printf 'jmail.service is running.\n'
    # Keys exist after the container's init step, so the summary can print them.
    READ_DKIM=1
  else
    warn "jmail.service is not active yet; check: systemctl --user status jmail"
  fi

  print_summary "$origin" "$extra_domains" "$hostname" "$publish_port" \
    "$smtp_user" "$smtp_password" "$http_enabled" "$http_bind" "$http_port" "$http_token" "$sb_count"
}

print_summary() {
  local origin="$1" extra_domains="$2" hostname="$3" publish_port="$4"
  local smtp_user="$5" smtp_password="$6" http_enabled="$7" http_bind="$8"
  local http_port="$9" http_token="${10}" supabase_count="${11}" domain dkim_record

  printf '\n=== jmail configuration ===\n\n'
  printf 'Environment file : %s\n' "$ENV_FILE"
  printf 'Service unit     : %s\n' "$UNIT_FILE"
  printf 'Data directory   : %s\n\n' "$DATA_DIR"

  printf 'SMTP submission\n'
  printf '  host     : %s (or 127.0.0.1 from this machine)\n' "$hostname"
  printf '  port     : %s\n' "$publish_port"
  printf '  username : %s\n' "$smtp_user"
  printf '  password : %s\n' "$smtp_password"
  printf '  STARTTLS is required before AUTH\n\n'

  if ((http_enabled)) && [[ -n "$http_token" ]]; then
    printf 'HTTP API (jmail-http)\n'
    printf '  endpoint : http://%s:%s/v1/send\n' "$http_bind" "$http_port"
    printf '  token    : %s\n' "$http_token"
    if [[ "$http_bind" == "127.0.0.1" ]] || [[ "$http_bind" == "[::1]" ]]; then
      printf '  It is bound to loopback; put TLS in front of it before exposing it.\n'
    fi
    printf '\n'
  elif ((http_enabled)); then
    printf 'HTTP API (jmail-http)\n'
    printf '  send API : disabled (no bearer token)\n\n'
  fi

  if ((supabase_count > 0)); then
    printf 'Supabase Auth hook\n'
    printf '  endpoint : http://%s:%s/v1/hooks/supabase/send-email\n' "$http_bind" "$http_port"
    printf '  projects : %s\n' "$supabase_count"
    if [[ "$http_bind" == "127.0.0.1" ]] || [[ "$http_bind" == "[::1]" ]]; then
      printf '  It is bound to loopback; put TLS in front of it before exposing it.\n'
    fi
    printf '\n'
  fi

  printf 'DNS records to publish\n'
  printf '======================\n'
  printf 'Publish each value as a single TXT value; do not include the quotes.\n\n'
  local -a domains=() dkim_values=()
  local dkim_ready idx
  domains=("$origin")
  for domain in ${extra_domains//,/ }; do
    domains+=("$domain")
  done
  for domain in "${domains[@]}"; do
    dkim_record=""
    dkim_ready=0
    if ((READ_DKIM)) && dkim_record="$(read_dkim_record "$domain")"; then
      dkim_ready=1
    fi
    dkim_values+=("$dkim_record")
    printf '%s\n' "$domain"
    printf '  DKIM   sendonly._domainkey.%s  TXT\n' "$domain"
    if ((dkim_ready)); then
      printf '         %s\n' "$dkim_record"
    else
      printf '         Generated on the first start. Read it with:\n'
      printf '           podman exec %s cat /etc/opendkim/keys/%s/sendonly.txt\n' "$UNIT_NAME" "$domain"
      printf '         then join the quoted chunks into one value, without the quotes.\n'
    fi
    printf '  SPF    %s  TXT\n         v=spf1 a:%s -all\n' "$domain" "$hostname"
    printf '  DMARC  _dmarc.%s  TXT\n         v=DMARC1; p=none; rua=mailto:dmarc@%s\n\n' "$domain" "$domain"
  done
  printf 'No MX record is needed: jmail is outbound-only.\n'
  printf 'SPF uses the A record of %s; add ip4:<server-ip> if that is not the sending address.\n' "$hostname"
  printf 'Also point the PTR record of this server at %s.\n' "$hostname"
  printf 'Tighten DMARC to p=reject once the reports look clean.\n\n'

  # A zone import adds records rather than editing them in place, so say what
  # to remove first: a second TXT at the same name breaks SPF and DMARC.
  printf 'Import into Cloudflare (DNS -> Records -> Import, one zone at a time)\n'
  printf '%s\n' '---------------------------------------------------------------------'
  for idx in "${!domains[@]}"; do
    domain="${domains[idx]}"
    printf '; %s\n' "$domain"
    printf '@ IN TXT "v=spf1 a:%s -all"\n' "$hostname"
    if [[ -n "${dkim_values[idx]}" ]]; then
      printf 'sendonly._domainkey IN TXT "%s"\n' "${dkim_values[idx]}"
    else
      printf '; sendonly._domainkey IN TXT "<read from the container after first start>"\n'
    fi
    printf '_dmarc IN TXT "v=DMARC1; p=none; rua=mailto:dmarc@%s"\n\n' "$domain"
  done
  printf 'Remove any existing record with the same name and type before importing:\n'
  printf 'a second TXT value at the same name breaks SPF and DMARC.\n\n'

  printf 'Logs: journalctl --user -u jmail -f   (or: podman logs -f %s)\n' "$UNIT_NAME"
  printf 'Status: systemctl --user status jmail\n'
}

# opendkim-genkey writes the public key as a chunked BIND record:
#
#   sendonly._domainkey IN TXT ( "v=DKIM1; k=rsa; "
#         "p=MIIBIjAN..." "...rest" )  ; ----- DKIM key sendonly for example.com
#
# DNS providers want one value without the quotes, the owner name or the
# comment, and a copy-paste that keeps the quotes publishes a broken key.
format_dkim_record() {
  sed -e 's/; *-----.*$//' \
    -e 's/^.*[[:space:]]TXT[[:space:]]*//' \
    -e 's/[()"]//g' |
    # Joining the chunks before restoring the tag spaces keeps the base64 value
    # intact; a space inside p= would split the key.
    tr -d '[:space:]' |
    sed 's/;/; /g; s/; *$//'
}

# The key is generated inside the container during its first start, so this is
# only useful once the service is up; the caller sets READ_DKIM to say so.
read_dkim_record() {
  local domain="$1" attempt record
  for attempt in 1 2 3 4 5; do
    record="$(podman exec "$UNIT_NAME" cat "/etc/opendkim/keys/$domain/sendonly.txt" 2>/dev/null || true)"
    if [[ -n "$record" ]]; then
      record="$(printf '%s' "$record" | format_dkim_record)"
      if [[ -n "$record" ]]; then
        printf '%s' "$record"
        return 0
      fi
    fi
    sleep 1
  done
  return 1
}

main "$@"
