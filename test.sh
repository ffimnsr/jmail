#!/usr/bin/env bash

set -e

podman run --rm -it \
  -h example.com \
  --env-file .env.test \
  ghcr.io/ffimnsr/jmail:latest bash
