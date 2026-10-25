#!/usr/bin/env bash
set -euo pipefail

ARCH="${1:-amd64}"
VERSION="${2:-0.1.0}"
BIN_PATH="${3:-dist/agy-pool-linux-${ARCH}}"
OUT_DIR="${4:-dist}"

# Strip leading 'v' from version if present
VERSION="${VERSION#v}"

if [ ! -f "$BIN_PATH" ]; then
  echo "Error: binary not found at $BIN_PATH" >&2
  exit 1
fi

TMP_DIR=$(mktemp -d)
trap 'rm -rf "$TMP_DIR"' EXIT

PKG_DIR="$TMP_DIR/pkg"
mkdir -p "$PKG_DIR/DEBIAN"
mkdir -p "$PKG_DIR/usr/bin"
mkdir -p "$PKG_DIR/usr/lib/systemd/system"
mkdir -p "$PKG_DIR/usr/lib/systemd/user"

# Install binary
install -m 0755 "$BIN_PATH" "$PKG_DIR/usr/bin/agy-pool"

# Install systemd service files
cat << 'SERVICE_SYS' > "$PKG_DIR/usr/lib/systemd/system/agy-pool.service"
[Unit]
Description=Antigravity Multi-Account Quota Pool & Reverse Proxy Gateway
Documentation=https://github.com/vlxlv/agy-pool-go
After=network.target

[Service]
Type=simple
User=agy-pool
Group=agy-pool
Environment=AGY_POOL_DAEMON_CHILD=1
Environment=AGY_CONFIG_FILE=/etc/agy-pool/config.json
Environment=AGY_DATA_DIR=/var/lib/agy-pool
ExecStart=/usr/bin/agy-pool
Restart=always
RestartSec=3s
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
SERVICE_SYS
chmod 0644 "$PKG_DIR/usr/lib/systemd/system/agy-pool.service"

cat << 'SERVICE_USER' > "$PKG_DIR/usr/lib/systemd/user/agy-pool.service"
[Unit]
Description=Antigravity Multi-Account Quota Pool & Reverse Proxy Gateway
Documentation=https://github.com/vlxlv/agy-pool-go
After=network.target

[Service]
Type=simple
Environment=AGY_POOL_DAEMON_CHILD=1
ExecStart=/usr/bin/agy-pool
Restart=always
RestartSec=3s
LimitNOFILE=65536

[Install]
WantedBy=default.target
SERVICE_USER
chmod 0644 "$PKG_DIR/usr/lib/systemd/user/agy-pool.service"

# Generate control file
cat << CONTROL > "$PKG_DIR/DEBIAN/control"
Package: agy-pool
Version: ${VERSION}
Section: net
Priority: optional
Architecture: ${ARCH}
Maintainer: vlxlv <https://github.com/vlxlv/agy-pool-go>
Homepage: https://github.com/vlxlv/agy-pool-go
Description: Antigravity Multi-Account Quota Pool & Reverse Proxy Gateway
 High-performance reverse proxy gateway and multi-account quota pool
 for Antigravity CLI (agy). Includes background daemon supervision,
 intelligent model-level cooldowns, and automatic failover.
CONTROL

# Generate postinst script
cat << 'POSTINST' > "$PKG_DIR/DEBIAN/postinst"
#!/bin/sh
set -e
if [ "$1" = "configure" ]; then
    if command -v systemctl >/dev/null 2>&1; then
        systemctl daemon-reload >/dev/null 2>&1 || true
    fi
fi
POSTINST
chmod 0755 "$PKG_DIR/DEBIAN/postinst"

# Generate prerm script
cat << 'PRERM' > "$PKG_DIR/DEBIAN/prerm"
#!/bin/sh
set -e
if [ "$1" = "remove" ]; then
    if command -v systemctl >/dev/null 2>&1; then
        systemctl stop agy-pool.service >/dev/null 2>&1 || true
        systemctl disable agy-pool.service >/dev/null 2>&1 || true
    fi
fi
PRERM
chmod 0755 "$PKG_DIR/DEBIAN/prerm"

mkdir -p "$OUT_DIR"
DEB_NAME="agy-pool_${VERSION}_${ARCH}.deb"
dpkg-deb --build --root-owner-group "$PKG_DIR" "$OUT_DIR/$DEB_NAME"
echo "Built Debian package: $OUT_DIR/$DEB_NAME"
