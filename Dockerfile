# Copyright (c) 2026 Jody Scott
# SPDX-License-Identifier: Apache-2.0
#
# Release image: the pre-built static coop binary on a minimal base. The binary
# is built by the fleet's image pipeline against the fleet's merged protos.
#
# Every service runs under the launcher (github.com/tacplus-dev/launcher): PID 1
# that forwards signals, propagates the exit code, and kills a deadlocked child
# on gRPC health. It is the pinned release LAUNCHER_VERSION, downloaded from its
# GitHub release and verified against that release's checksums.txt (the sha256 on
# each ADD below): a mismatch fails the build. To move it, change the version and
# both sha256s together (ci/release_test.sh checks the pin).
ARG BASE_IMAGE=alpine:3.24
ARG LAUNCHER_VERSION=v0.1.0

# One stage holds both platforms' binaries, each under its own name; the image copies
# its own platform's (TARGETARCH). No FROM names a stage through an ARG, which trivy
# reads as an untagged image (DS-0001, #16).
FROM scratch AS launcher
ARG LAUNCHER_VERSION
ADD --checksum=sha256:903693ab56879b5a5b54779c3e20efa3ddd5f1269fb0c2274882befb7df69e0f --chmod=755 https://github.com/tacplus-dev/launcher/releases/download/${LAUNCHER_VERSION}/launcher-linux-amd64 /launcher-linux-amd64
ADD --checksum=sha256:f07933da11e08812afa87d814ecbd187087f20c2af7f62d0c1d356754643ec16 --chmod=755 https://github.com/tacplus-dev/launcher/releases/download/${LAUNCHER_VERSION}/launcher-linux-arm64 /launcher-linux-arm64

FROM ${BASE_IMAGE}
ARG TARGETARCH
ARG VERSION=0.0.0
ARG VCS_REF=local
LABEL org.opencontainers.image.title="coop" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${VCS_REF}" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.source="https://github.com/tacplus-dev/coop"
RUN apk add --no-cache ca-certificates && adduser -D -u 65532 app
COPY bin/coop /usr/local/bin/coop
COPY --from=launcher /launcher-linux-${TARGETARCH} /usr/local/bin/launcher
USER 65532
# The launcher's probe is plaintext or verified TLS, never TLS unverified
# (launcher v0.1.0, tacplus-dev/launcher#7): it probes the chassis's plaintext
# grpc.health.v1 listener, health_probe_port (8444 by default), which reports the
# same status as the TLS port (8443), not 8443 itself, which with no HEALTH_CA it
# would fail every probe on and kill the coop after HEALTH_TIMEOUT (#14).
ENV LAUNCH_CMD="coop" HEALTH_PORT=8444
ENTRYPOINT ["launcher"]
