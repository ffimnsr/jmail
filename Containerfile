# The HTTP submission API is optional at run time (svc-jmail-http only starts the
# service when JMAIL_HTTP_TOKEN is set), but it is built here so the runtime image
# carries no Go toolchain. Cross-compiling from the builder architecture keeps the
# arm64 build off emulation.
FROM golang:1.25-alpine AS http-build

WORKDIR /src
COPY go.mod ./
COPY cmd/jmail-http ./cmd/jmail-http

ARG TARGETARCH
ARG JMAIL_VERSION=dev
RUN set -eux; \
  arch="${TARGETARCH:-$(go env GOARCH)}"; \
  CGO_ENABLED=0 GOOS=linux GOARCH="$arch" \
    go build -trimpath -ldflags="-s -w -X main.version=${JMAIL_VERSION}" \
      -o /out/jmail-http ./cmd/jmail-http

FROM alpine:3.24

ARG DOCKERIZE_VERSION=v0.16.0
ARG S6_OVERLAY_VERSION=3.2.3.2

ENV JMAIL_HOSTNAME=example.com
ENV JMAIL_ORIGIN=example.com
ENV JMAIL_ROOT_EMAIL=user@example.com
ENV JMAIL_SMTP_USER=jmail
ENV JMAIL_SMTP_PASSWORD=mysecretpassword
ENV JMAIL_MYNETWORKS=127.0.0.0/8
ENV S6_BEHAVIOUR_IF_STAGE2_FAILS=2

LABEL org.opencontainers.image.title="jmail" \
  org.opencontainers.image.description="Outbound SMTP email sender." \
  org.opencontainers.image.source="https://github.com/ffimnsr/jmail" \
  org.opencontainers.image.licenses="Apache-2.0"

RUN apk add --no-cache \
  postfix opendkim opendkim-utils \
  ca-certificates tzdata bash openssl xz \
  cyrus-sasl cyrus-sasl-crammd5 cyrus-sasl-digestmd5 cyrus-sasl-login cyrus-sasl-ntlm \
  mailx musl musl-utils

# s6-overlay is installed from the upstream release tarballs instead of the
# Alpine package so that it can be bumped independently (and kept current).
RUN set -eux; \
  case "$(apk --print-arch)" in \
    x86_64) s6_arch=x86_64 ;; \
    aarch64) s6_arch=aarch64 ;; \
    armv7|armv6|armhf) s6_arch=arm ;; \
    x86) s6_arch=i686 ;; \
    ppc64le) s6_arch=powerpc64le ;; \
    s390x) s6_arch=s390x ;; \
    riscv64) s6_arch=riscv64 ;; \
    *) echo "unsupported architecture: $(apk --print-arch)" >&2; exit 1 ;; \
  esac; \
  s6_base="https://github.com/just-containers/s6-overlay/releases/download/v${S6_OVERLAY_VERSION}"; \
  cd /tmp; \
  for archive in \
    s6-overlay-noarch.tar.xz \
    "s6-overlay-${s6_arch}.tar.xz" \
    s6-overlay-symlinks-noarch.tar.xz \
    s6-overlay-symlinks-arch.tar.xz; do \
    wget -q -O "$archive" "${s6_base}/${archive}"; \
    wget -q -O "$archive.sha256" "${s6_base}/${archive}.sha256"; \
    sha256sum -c "$archive.sha256"; \
    xz -dc "$archive" | tar -C / -xpf -; \
    rm -f "$archive" "$archive.sha256"; \
  done

RUN set -eux; \
  case "$(apk --print-arch)" in \
    x86_64) dockerize_arch=amd64 ;; \
    aarch64) dockerize_arch=arm64 ;; \
    armv7|armv6|armhf) dockerize_arch=armv6 ;; \
    x86) dockerize_arch=386 ;; \
    ppc64le) dockerize_arch=ppc64le ;; \
    s390x) dockerize_arch=s390x ;; \
    riscv64) dockerize_arch=riscv64 ;; \
    *) echo "unsupported architecture: $(apk --print-arch)" >&2; exit 1 ;; \
  esac; \
  archive="dockerize-linux-${dockerize_arch}-${DOCKERIZE_VERSION}.tar.gz"; \
  dockerize_base="https://github.com/jwilder/dockerize/releases/download/${DOCKERIZE_VERSION}"; \
  cd /tmp; \
  wget -q -O "$archive" "${dockerize_base}/${archive}"; \
  wget -q -O checksums.txt "${dockerize_base}/checksums.txt"; \
  grep -F "  ${archive}" checksums.txt > dockerize.sha256; \
  sha256sum -c dockerize.sha256; \
  tar -C /usr/local/bin -xzf "$archive" dockerize; \
  rm -f "$archive" checksums.txt dockerize.sha256

COPY /data/s6-overlay /etc/s6-overlay
COPY /data/templates /etc/templates
COPY --from=http-build /out/jmail-http /usr/local/bin/jmail-http

# Extension points, applied by init-mailer-config on every start. They exist in
# the image so a derived image or a bind mount can drop files into them without
# touching the templates.
RUN mkdir -p /etc/jmail/init.d /etc/jmail/postfix.d

# The HTTP API's idempotency records. Created here with the service's own
# permissions so the directory is usable whether it is a volume, a bind mount or
# the container's writable layer.
RUN install -d -m 0700 /var/lib/jmail-http

VOLUME ["/var/spool/postfix", "/etc/postfix", "/etc/opendkim/keys", "/var/lib/jmail-http"]

ENTRYPOINT ["/init"]

# `postfix status` proves the master is alive; the port checks prove clients can
# connect. Note the explicit IPv4 address: "/etc/postfix/main.cf" sets
# inet_protocols = ipv4, so "localhost" may resolve to ::1 and fail. The HTTP API
# port is only checked when JMAIL_HTTP_TOKEN enables it, because the service parks
# itself otherwise and the port stays closed.
HEALTHCHECK --interval=30s --timeout=5s --start-period=30s --retries=3 \
  CMD postfix status >/dev/null 2>&1 && nc -z 127.0.0.1 587 && \
  { [ -z "${JMAIL_HTTP_TOKEN}${JMAIL_HTTP_SUPABASE_HOOK_SECRET}" ] || nc -z 127.0.0.1 8080; }

STOPSIGNAL SIGINT
EXPOSE 25 587 8080
