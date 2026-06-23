FROM golang:1.25-bookworm

ARG KUBECTL_VERSION=v1.32.2
ARG KUBOCD_VERSION=v0.3.0
ARG SWAG_VERSION=v1.16.6
ARG DELVE_VERSION=v1.26.3
ARG AIR_VERSION=v1.65.3
ARG GOLANGCI_LINT_VERSION=v2.5.0

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates curl git make procps sudo \
    && rm -rf /var/lib/apt/lists/*

# kubectl, pinned and sha256-verified
RUN set -eux; \
    arch="$(dpkg --print-architecture)"; \
    curl -fsSLO "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/${arch}/kubectl"; \
    curl -fsSLO "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/${arch}/kubectl.sha256"; \
    echo "$(cat kubectl.sha256)  kubectl" | sha256sum -c -; \
    install -o root -g root -m 0755 kubectl /usr/local/bin/kubectl; \
    rm kubectl kubectl.sha256

# kubocd CLI: the server shells out to `kubocd dump package` (package-schema endpoint)
RUN set -eux; \
    arch="$(dpkg --print-architecture)"; \
    case "$arch" in \
        amd64) asset="kubocd_Linux_x86_64" ;; \
        arm64) asset="kubocd_Linux_arm64" ;; \
        *) echo "unsupported architecture: $arch" >&2; exit 1 ;; \
    esac; \
    base="https://github.com/kubocd/kubocd/releases/download/${KUBOCD_VERSION}"; \
    curl -fsSLO "${base}/${asset}"; \
    curl -fsSLO "${base}/checksums.txt"; \
    grep " ${asset}\$" checksums.txt | sha256sum -c -; \
    install -o root -g root -m 0755 "${asset}" /usr/local/bin/kubocd; \
    rm "${asset}" checksums.txt

# Go dev tooling, built from source so golangci-lint matches the project's Go version
# (its release binaries embed their build Go and refuse a newer target).
RUN go install "github.com/swaggo/swag/cmd/swag@${SWAG_VERSION}" \
    && go install "github.com/go-delve/delve/cmd/dlv@${DELVE_VERSION}" \
    && go install "github.com/air-verse/air@${AIR_VERSION}" \
    && go install "github.com/golangci/golangci-lint/v2/cmd/golangci-lint@${GOLANGCI_LINT_VERSION}"

# Non-root user. GOPATH (/go) is world-writable in the golang image, and sudo lets
# postCreate chown the root-owned cache volumes.
ARG USERNAME=vscode
ARG USER_UID=1000
ARG USER_GID=$USER_UID
RUN groupadd --gid "$USER_GID" "$USERNAME" \
    && useradd --uid "$USER_UID" --gid "$USER_GID" -m -s /bin/bash "$USERNAME" \
    && echo "$USERNAME ALL=(root) NOPASSWD:ALL" > "/etc/sudoers.d/$USERNAME" \
    && chmod 0440 "/etc/sudoers.d/$USERNAME"

USER $USERNAME

WORKDIR /workspace

EXPOSE 8093
