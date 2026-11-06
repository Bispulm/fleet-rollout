# One image holds every binary: rolloutd, rollout-operator, rollout-agent, rollout-sim, and
# rolloutctl. Each container picks its command. Both stages are pinned by digest.

# The build stage runs on the builder's own platform and cross-compiles, so a multi-platform build
# needs no emulation.
FROM --platform=$BUILDPLATFORM golang:1.27.3-alpine@sha256:fa4881266890ef051f0ce63afdfaccb69bbe8186084387f4fafd60d7ecbabc71 AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY api ./api
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/ ./cmd/...
# The distroless image has no shell to make this with. A volume mounted here starts out with
# the directory's ownership, so the non-root user can write artifacts to it.
RUN mkdir -p /data/var/lib/fleet-rollout/artifacts

FROM gcr.io/distroless/static-debian13:nonroot@sha256:25ac8f0c3b2334a9eb05452266bd4697864a8c8ef0ef71e5e6480d7f92868329
COPY --from=build /out/ /usr/local/bin/
COPY --from=build --chown=65532:65532 /data/ /
# A numeric user, because Kubernetes can't check that a named one isn't root.
USER 65532:65532
