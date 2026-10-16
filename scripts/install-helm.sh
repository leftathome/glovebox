#!/usr/bin/env bash
# Install a pinned, checksum-verified helm into DEST (default /usr/local/bin)
# for the chart-test CI jobs. Pinned rather than "latest" so a helm release
# cannot change what the chart checks see; bump HELM_VERSION and HELM_SHA256
# together (https://get.helm.sh/helm-<version>-linux-amd64.tar.gz.sha256sum).
set -euo pipefail

HELM_VERSION="${HELM_VERSION:-v4.3.0}"
HELM_SHA256="${HELM_SHA256:-86584a54def73570558f66f5111cc53dfed56689637ae32c1201205d494f54fb}"
DEST="${1:-/usr/local/bin}"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
curl -fsSL -o "$TMP/helm.tgz" "https://get.helm.sh/helm-${HELM_VERSION}-linux-amd64.tar.gz"
echo "${HELM_SHA256}  $TMP/helm.tgz" | sha256sum -c -
tar -xzf "$TMP/helm.tgz" -C "$TMP"
install -m 0755 "$TMP/linux-amd64/helm" "$DEST/helm"
"$DEST/helm" version --short
