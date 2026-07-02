# Copyright 2026 Li Qiankun
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Multi-arch (linux/amd64, linux/arm64). hostel is pure Go, so the builder runs
# NATIVELY on the build host ($BUILDPLATFORM) and cross-compiles for the target
# arch ($TARGETARCH) — no QEMU emulation of the Go toolchain. Only the runtime
# stage runs as $TARGETPLATFORM, where apt resolves the right-arch bwrap/chromium.
ARG BUILDPLATFORM
ARG TARGETOS
ARG TARGETARCH

# --- build stage: static, pure-Go hostel binary --------------------------
FROM --platform=$BUILDPLATFORM golang:1.25-bookworm AS builder
ARG TARGETOS
ARG TARGETARCH
# GOPROXY overridable for networks without proxy.golang.org (e.g. corp mirrors).
ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=${GOPROXY}
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go mod download
COPY . .
ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/hostel ./cmd/hostel

# --- runtime stage --------------------------------------------------------
# Debian (not distroless/alpine) because the optional facilities need a real
# base: bubblewrap for --isolation bwrap, chromium for the browser amenity.
# Both stay OPTIONAL at runtime — hostel probes them at boot and degrades
# honestly, so a locked-down pod without namespaces still serves. No --platform
# here: this stage IS the target platform, so apt installs the correct arch.
FROM debian:bookworm-slim

# WITH_CHROMIUM=false builds a lean image (~150MB) with no browser amenity;
# use the attach mode (--chromium-cdp-url) or a sidecar in that case.
ARG WITH_CHROMIUM=true

RUN apt-get update && apt-get install -y --no-install-recommends \
        bubblewrap ca-certificates tini bash \
    && if [ "$WITH_CHROMIUM" = "true" ]; then \
        apt-get install -y --no-install-recommends chromium fonts-liberation; \
    fi \
    && rm -rf /var/lib/apt/lists/*

COPY --from=builder /out/hostel /usr/local/bin/hostel

# Runs as root by default (like execd): bwrap must mkdir the /workspace mount
# point on the read-only root and needs user namespaces, and chromium runs with
# --no-sandbox — all awkward as non-root. Harden per deployment by running with
# a dropped-capability securityContext, or build a non-root variant once your
# pod grants bwrap the specific caps it needs.

# Sensible in-container defaults; every one is overridable via HOSTEL_* env.
ENV HOSTEL_WORKSPACE_ROOT=/workspace \
    HOSTEL_ISOLATION=bwrap \
    HOSTEL_CHROMIUM_PATH=/usr/bin/chromium
# Persist beds across container restarts when no object store is configured
# (with --store s3 the workspace is just a disposable working copy).
VOLUME ["/workspace"]
EXPOSE 8872

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s \
    CMD ["hostel", "--health"]

# tini as PID 1: hostel spawns shells and (via the amenity) chromium, both of
# which fork children — a real init reaps zombies and forwards signals.
ENTRYPOINT ["/usr/bin/tini", "--", "hostel"]
