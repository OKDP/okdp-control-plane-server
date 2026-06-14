ARG GO_VERSION=1.25
ARG KUBOCD_VERSION=v0.3.0

# Cross-compile on the native build platform (no QEMU emulation): the Go
# toolchain runs natively and emits a static binary for the target arch.
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION} AS go-build

ARG TARGETOS=linux
ARG TARGETARCH

WORKDIR /workspace/okdp-server

COPY go.* ./
RUN go mod download

COPY cmd/ cmd/
COPY internal/ internal/

RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /okdp-server ./cmd/server

# kubocd CLI: the server shells out to `kubocd dump` to render service package
# schemas (internal/service/package_schema_service.go). Downloaded per target
# arch from the official release, on the native build platform (no QEMU), with
# checksum verification.
FROM --platform=$BUILDPLATFORM alpine:3.21 AS kubocd
ARG TARGETARCH
ARG KUBOCD_VERSION
RUN apk add --no-cache curl coreutils && \
    case "$TARGETARCH" in \
      amd64) KARCH=x86_64 ;; \
      arm64) KARCH=arm64 ;; \
      *) echo "unsupported TARGETARCH: $TARGETARCH" >&2; exit 1 ;; \
    esac && \
    cd /tmp && \
    BASE="https://github.com/kubocd/kubocd/releases/download/${KUBOCD_VERSION}" && \
    curl -fsSL -O "${BASE}/kubocd_Linux_${KARCH}" && \
    curl -fsSL -O "${BASE}/checksums.txt" && \
    grep "kubocd_Linux_${KARCH}\$" checksums.txt | sha256sum -c - && \
    install -m 0755 "kubocd_Linux_${KARCH}" /kubocd

FROM alpine:3.21

RUN apk --no-cache add ca-certificates && update-ca-certificates

COPY --from=go-build /okdp-server /usr/local/bin/okdp-server
COPY --from=kubocd /kubocd /usr/local/bin/kubocd

# Writable HOME for the unprivileged user so `kubocd dump` can use its cache.
ENV HOME=/tmp

USER 65534:65534

EXPOSE 8093

ENTRYPOINT ["okdp-server"]
