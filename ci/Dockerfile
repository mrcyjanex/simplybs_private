# CI toolchain for package-worker jobs (Go + git + gh).
# Package artifacts stay on the GitHub Release cache, not in this image.
#
# Seed/native binaries (e.g. clang21 ./bin/mkdir) need a new-enough glibc.
# bookworm (Debian 12) is glibc 2.36 and fails with GLIBC_2.38 not found.
# trixie (Debian 13) is glibc 2.41 and fails on linux-arm64 clang21
# (./bin/mkdir requires GLIBC_2.42).
# forky (Debian 14 / testing) is glibc 2.42.
FROM golang:1.25-trixie AS go

FROM debian:forky

COPY --from=go /usr/local/go /usr/local/go
ENV PATH=/usr/local/go/bin:$PATH

# clang21 still uses the distro linker and C runtime; LLVM cmake also
# looks for a system Python3. The official golang:*-trixie image
# inherits gcc/libc-dev/python3 from buildpack-deps; debian:forky does not.
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      bash \
      build-essential \
      ca-certificates \
      curl \
      git \
      gnupg \
      python3 \
 && curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg \
      -o /usr/share/keyrings/githubcli-archive-keyring.gpg \
 && chmod go+r /usr/share/keyrings/githubcli-archive-keyring.gpg \
 && echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" \
      > /etc/apt/sources.list.d/github-cli.list \
 && apt-get update \
 && apt-get install -y --no-install-recommends gh \
 && rm -rf /var/lib/apt/lists/*

# GitHub-hosted ubuntu-latest uses uid 1001 (runner). Match it so the
# /__w workspace bind-mount is writable without running as root.
# crosstool-NG (native/ct-tools) refuses uid 0.
RUN groupadd --gid 1001 ci \
 && useradd --uid 1001 --gid 1001 --create-home --shell /bin/bash ci

ENV GOCACHE=/tmp/go-cache \
    GOMODCACHE=/tmp/go-modcache \
    GOTMPDIR=/tmp/go-tmp

USER 1001
RUN mkdir -p /tmp/go-cache /tmp/go-modcache /tmp/go-tmp
WORKDIR /workspace
