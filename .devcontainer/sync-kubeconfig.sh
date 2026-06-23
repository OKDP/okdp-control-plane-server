#!/usr/bin/env bash
# Derive a container-reachable kubeconfig from the host one. The host serves the API on
# 127.0.0.1 (cert for 127.0.0.1), so repoint it at host.docker.internal and skip TLS
# verification (the cert does not cover that name).
set -euo pipefail

SRC="${KUBECONFIG_SRC:-$HOME/.kube/okdp-dev-config}"
DST="${KUBECONFIG_DC:-.dev/kubeconfig}"

if [ ! -f "$SRC" ]; then
  echo "!! host kubeconfig not found at $SRC (is the dev-sandbox running and ~/.kube mounted?)" >&2
  exit 1
fi

mkdir -p "$(dirname "$DST")"
cp "$SRC" "$DST"

cl=$(KUBECONFIG="$DST" kubectl config view -o jsonpath='{.clusters[0].name}')
port=$(KUBECONFIG="$DST" kubectl config view -o jsonpath='{.clusters[0].cluster.server}' | sed -E 's#.*:##')
KUBECONFIG="$DST" kubectl config unset "clusters.${cl}.certificate-authority-data" >/dev/null || true
KUBECONFIG="$DST" kubectl config set-cluster "$cl" \
  --server="https://host.docker.internal:${port}" --insecure-skip-tls-verify=true >/dev/null

echo ">> kubeconfig ready: $DST (API -> host.docker.internal:${port})"
