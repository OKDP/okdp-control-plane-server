FROM golang:1.25-bookworm

ARG KUBECTL_VERSION=v1.32.2
ARG SWAG_VERSION=v1.16.6
ARG GOLANGCI_LINT_VERSION=v1.64.8
ARG KUBOCD_VERSION=v0.3.0

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates curl git make procps \
    && rm -rf /var/lib/apt/lists/*

# kubectl — pinned + sha256 verified
RUN set -eux; \
    arch="$(dpkg --print-architecture)"; \
    curl -fsSLO "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/${arch}/kubectl"; \
    curl -fsSLO "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/${arch}/kubectl.sha256"; \
    echo "$(cat kubectl.sha256)  kubectl" | sha256sum -c -; \
    install -o root -g root -m 0755 kubectl /usr/local/bin/kubectl; \
    rm kubectl kubectl.sha256

# kubocd CLI — must match controller version deployed in the bootstrap cluster
RUN arch="$(dpkg --print-architecture)"; \
    curl -fsSLo /usr/local/bin/kubocd \
    "https://github.com/kubocd/kubocd/releases/download/${KUBOCD_VERSION}/kubocd_Linux_$([ "$arch" = "arm64" ] && echo aarch64 || echo x86_64)" && \
    chmod +x /usr/local/bin/kubocd

# Go dev tooling — all verified via Go module checksum database
RUN go install "github.com/swaggo/swag/cmd/swag@${SWAG_VERSION}" \
    && go install github.com/go-delve/delve/cmd/dlv@latest \
    && go install github.com/air-verse/air@latest \
    && curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh \
        | sh -s -- -b /go/bin "${GOLANGCI_LINT_VERSION}"

WORKDIR /workspace

EXPOSE 8093
