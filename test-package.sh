#!/usr/bin/env bash

set -euo pipefail

packager=docker
if ! command -v docker &>/dev/null; then
  packager=podman
fi

$packager run --rm -it \
  -h example.com \
  --env-file .env.test \
  ghcr.io/ffimnsr/jmail:latest bash
