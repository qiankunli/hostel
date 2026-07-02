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

# --- build stage: static, pure-Go hostel binary --------------------------
FROM golang:1.25 AS builder
WORKDIR /src
# Deps first for layer caching.
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/hostel ./cmd/hostel

# --- runtime stage --------------------------------------------------------
# Debian (not distroless/alpine) because the optional facilities need a real
# base: bubblewrap for --isolation bwrap, chromium for the browser amenity.
# Both stay OPTIONAL at runtime — hostel probes them at boot and degrades
# honestly, so a locked-down pod without namespaces still serves.
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

# Sensible in-container defaults; every one is overridable via HOSTEL_* env.
ENV HOSTEL_WORKSPACE_ROOT=/workspace \
    HOSTEL_ISOLATION=bwrap \
    HOSTEL_CHROMIUM_PATH=/usr/bin/chromium
# Persist beds across container restarts when no object store is configured
# (with --store s3 the workspace is just a disposable working copy).
VOLUME ["/workspace"]
EXPOSE 44772

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s \
    CMD ["hostel", "--health"]

# tini as PID 1: hostel spawns shells and (via the amenity) chromium, both of
# which fork children — a real init reaps zombies and forwards signals.
ENTRYPOINT ["/usr/bin/tini", "--", "hostel"]
