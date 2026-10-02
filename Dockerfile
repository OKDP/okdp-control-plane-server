ARG GO_VERSION=1.25

# Cross-compile on the native build platform (no QEMU emulation): the Go
# toolchain runs natively and emits a static binary for the target arch.
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION} AS go-build

ARG TARGETOS=linux
ARG TARGETARCH
# Version of the server (no leading "v"), named in the startup log and in the
# co-author trailer of the commits it writes. CI passes the release version.
ARG VERSION=dev

WORKDIR /workspace/okdp-server

# go.mod replaces github.com/okdp/okdp-lib-chart with ../okdp-lib-chart during the
# no-kubocd migration: build with --build-context okdp-lib=../okdp-lib-chart.
COPY --from=okdp-lib go.mod ../okdp-lib-chart/
COPY --from=okdp-lib contracts/ ../okdp-lib-chart/contracts/
COPY go.* ./
RUN go mod download

COPY cmd/ cmd/
COPY internal/ internal/

RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath \
      -ldflags="-s -w -X github.com/okdp/okdp-control-plane-server/internal/buildinfo.Version=${VERSION}" \
      -o /okdp-server ./cmd/server

FROM alpine:3.21

RUN apk --no-cache add ca-certificates && update-ca-certificates

COPY --from=go-build /okdp-server /usr/local/bin/okdp-server

# Writable HOME for the unprivileged user (Git and registry clients).
ENV HOME=/tmp

USER 65534:65534

EXPOSE 8093

ENTRYPOINT ["okdp-server"]
