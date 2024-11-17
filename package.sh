#!/usr/bin/env bash

set -euo pipefail

packager=docker
if ! command -v docker &>/dev/null; then
  packager=podman
fi

name=jmail
latest_version=$(git describe --tags --abbrev=0 | cut -d'-' -f2)
$packager build \
  --sbom=true \
  --label org.opencontainers.image.created=$(date +%Y-%m-%dT%H:%M:%S%z) \
  --label org.opencontainers.image.authors=gh:@ffimnsr \
  --label org.opencontainers.image.description="Outbound SMTP email sender." \
  --label org.opencontainers.image.revision=$(git rev-parse HEAD) \
  --label org.opencontainers.image.source=$(git remote get-url origin) \
  --label org.opencontainers.image.title=$name \
  --label org.opencontainers.image.url=https://hub.docker.com/r/ifn4/$name \
  --label org.opencontainers.image.documentation=https://hub.docker.com/r/ifn4/$name \
  --label org.opencontainers.image.version=$latest_version\
  -f Containerfile \
  -t ghcr.io/ffimnsr/jmail:latest .
