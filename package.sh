#!/usr/bin/env bash

set -euo pipefail

packager=podman
if command -v docker &>/dev/null; then
  # The podman-docker wrapper reports itself as podman; a real Docker client does not.
  if ! docker --version 2>/dev/null | grep -qi podman; then
    packager=docker
  fi
fi

name=jmail
image=ghcr.io/ffimnsr/$name
# Release tags are `vX.Y.Z`; VERSION lets the release script label a build before the tag exists.
latest_version=${VERSION:-$(git describe --tags --abbrev=0 2>/dev/null || true)}
latest_version=${latest_version#v}
latest_version=${latest_version#release-}
latest_version=${latest_version:-dev}

build_args=(
  --label org.opencontainers.image.created=$(date +%Y-%m-%dT%H:%M:%S%z)
  --label org.opencontainers.image.authors=gh:@ffimnsr
  --label org.opencontainers.image.description="Outbound SMTP email sender."
  --label org.opencontainers.image.revision=$(git rev-parse HEAD)
  --label org.opencontainers.image.source=$(git remote get-url origin)
  --label org.opencontainers.image.title=$name
  --label org.opencontainers.image.url=https://hub.docker.com/r/ifn4/$name
  --label org.opencontainers.image.documentation=https://hub.docker.com/r/ifn4/$name
  --label org.opencontainers.image.version=$latest_version
  # Reported by jmail-http in its startup log.
  --build-arg JMAIL_VERSION=$latest_version
  -f Containerfile
  -t $image:$latest_version
  -t $image:latest
)

# SBOM generation is a Docker BuildKit flag; podman/buildah does not accept it.
if [[ $packager == docker ]] && docker buildx version &>/dev/null; then
  build_args+=(--sbom=true)
fi

$packager build "${build_args[@]}" .
