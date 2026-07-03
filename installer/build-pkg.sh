#!/bin/bash
set -e
BINARY=${1:-dist/taufinity_darwin_universal}
VERSION=${2:-dev}
cp "$BINARY" installer/payload/usr/local/bin/taufinity
chmod +x installer/payload/usr/local/bin/taufinity
pkgbuild \
  --root installer/payload \
  --scripts installer/scripts \
  --identifier io.taufinity.cli \
  --version "$VERSION" \
  --install-location / \
  dist/taufinity_darwin_installer.pkg
