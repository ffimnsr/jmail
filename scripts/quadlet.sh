#!/usr/bin/env bash

set -euo pipefail

SCOPE="user"
OUTPUT_DIR="quadlet"
INSTALL=0

usage() {
  cat <<'EOF'
Usage: scripts/quadlet.sh [options]

Generate a podman quadlet unit and its environment file for jmail, and optionally
install them.

Options:
  --scope user|system   Install target. Default: user.
  --output <dir>        Where to write the generated files. Default: quadlet/.
  --install             Copy the files into place, reload systemd and start the unit.
  -h, --help            Show this help message.

Environment:
  JMAIL_IMAGE           Image reference. Default: ghcr.io/ffimnsr/jmail:latest
  JMAIL_HOSTNAME        Postfix myhostname. Default: mail.example.com
  JMAIL_ORIGIN          Signing domain. Default: $JMAIL_HOSTNAME
  JMAIL_ROOT_EMAIL      Root alias target. Default: postmaster@$JMAIL_ORIGIN
  JMAIL_SMTP_USER       SASL user. Default: jmail
  JMAIL_SMTP_PASSWORD   SASL password. Generated when unset.
  JMAIL_MYNETWORKS      Unauthenticated relay networks. Default: 127.0.0.0/8
  JMAIL_PUBLISH_PORT    Host port published as container 587. Default: 10587

Examples:
  scripts/quadlet.sh
  scripts/quadlet.sh --install
  sudo scripts/quadlet.sh --scope system --install
EOF
}

die() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

while (($# > 0)); do
  case "$1" in
    --scope)
      [[ $# -ge 2 ]] || die "--scope needs a value"
      SCOPE="$2"
      shift 2
      ;;
    --output)
      [[ $# -ge 2 ]] || die "--output needs a value"
      OUTPUT_DIR="$2"
      shift 2
      ;;
    --install)
      INSTALL=1
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      die "unknown option: $1"
      ;;
  esac
done

case "$SCOPE" in
  user)
    # %h is expanded by systemd to the invoking user's home directory.
    DATA_ROOT="%h/.local/share/jmail"
    ENV_FILE="%h/.config/jmail/jmail.env"
    UNIT_DIR="${HOME}/.config/containers/systemd"
    ENV_FILE_PATH="${HOME}/.config/jmail/jmail.env"
    WANTED_BY="default.target"
    ;;
  system)
    DATA_ROOT="/var/lib/jmail"
    ENV_FILE="/etc/jmail/jmail.env"
    UNIT_DIR="/etc/containers/systemd"
    ENV_FILE_PATH="/etc/jmail/jmail.env"
    WANTED_BY="multi-user.target"
    ;;
  *)
    die "scope must be 'user' or 'system', got: $SCOPE"
    ;;
esac

image="${JMAIL_IMAGE:-ghcr.io/ffimnsr/jmail:latest}"
hostname="${JMAIL_HOSTNAME:-mail.example.com}"
origin="${JMAIL_ORIGIN:-$hostname}"
root_email="${JMAIL_ROOT_EMAIL:-postmaster@${origin}}"
smtp_user="${JMAIL_SMTP_USER:-jmail}"
mynetworks="${JMAIL_MYNETWORKS:-127.0.0.0/8}"
publish_port="${JMAIL_PUBLISH_PORT:-10587}"

generated_password=0
if [[ -z "${JMAIL_SMTP_PASSWORD:-}" ]]; then
  command -v openssl >/dev/null 2>&1 || die "openssl is required to generate JMAIL_SMTP_PASSWORD"
  smtp_password="$(openssl rand -hex 16)"
  generated_password=1
else
  smtp_password="$JMAIL_SMTP_PASSWORD"
fi

mkdir -p "$OUTPUT_DIR"

cat >"${OUTPUT_DIR}/jmail.container" <<EOF
[Unit]
Description=jmail outbound SMTP sender
Wants=network-online.target
After=network-online.target

[Container]
ContainerName=jmail
Image=${image}
Pull=always
HostName=${hostname}
EnvironmentFile=${ENV_FILE}
Volume=${DATA_ROOT}/postfix-ssl:/etc/postfix/ssl:Z
Volume=${DATA_ROOT}/opendkim-keys:/etc/opendkim/keys:Z
Volume=${DATA_ROOT}/spool:/var/spool/postfix:Z
Volume=${DATA_ROOT}/postfix.d:/etc/jmail/postfix.d:Z
Volume=${DATA_ROOT}/init.d:/etc/jmail/init.d:Z
PublishPort=${publish_port}:587
# Podman does not carry the image HEALTHCHECK into the container, so it is
# declared here. 127.0.0.1 is explicit because Postfix runs ipv4-only.
HealthCmd=nc -z 127.0.0.1 587
HealthInterval=30s
HealthRetries=3
HealthStartPeriod=30s
HealthTimeout=5s

[Service]
Restart=always
# Pull=always means the start waits for a registry pull before the first boot.
TimeoutStartSec=300

[Install]
WantedBy=${WANTED_BY}
EOF

cat >"${OUTPUT_DIR}/jmail.env" <<EOF
JMAIL_HOSTNAME=${hostname}
JMAIL_ORIGIN=${origin}
JMAIL_ROOT_EMAIL=${root_email}
JMAIL_SMTP_USER=${smtp_user}
JMAIL_SMTP_PASSWORD=${smtp_password}
JMAIL_MYNETWORKS=${mynetworks}
EOF

chmod 0600 "${OUTPUT_DIR}/jmail.env"

printf 'Wrote %s and %s\n' "${OUTPUT_DIR}/jmail.container" "${OUTPUT_DIR}/jmail.env"
if ((generated_password)); then
  printf 'Generated SMTP password for %s: %s\n' "$smtp_user" "$smtp_password"
fi

if ((INSTALL == 0)); then
  printf 'Install with: %s --scope %s --install\n' "$0" "$SCOPE"
  exit 0
fi

systemctl_for_scope() {
  if [[ "$SCOPE" == "user" ]]; then
    systemctl --user "$@"
  else
    sudo systemctl "$@"
  fi
}

install_file() {
  local source="$1"
  local destination="$2"
  local mode="$3"

  if [[ "$SCOPE" == "user" ]]; then
    install -D -m "$mode" "$source" "$destination"
  else
    sudo install -D -m "$mode" "$source" "$destination"
  fi
}

install_file "${OUTPUT_DIR}/jmail.container" "${UNIT_DIR}/jmail.container" 0644
install_file "${OUTPUT_DIR}/jmail.env" "${ENV_FILE_PATH}" 0600

systemctl_for_scope daemon-reload
systemctl_for_scope enable --now jmail.service

printf '\nInstalled %s\n' "${UNIT_DIR}/jmail.container"
if [[ "$SCOPE" == "user" ]]; then
  printf 'Manage it with: systemctl --user status jmail\n'
  printf '\nTo keep jmail running after logout, enable lingering:\n  sudo loginctl enable-linger %s\n' "$(id -un)"
else
  printf 'Manage it with: sudo systemctl status jmail\n'
fi
