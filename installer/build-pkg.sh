#!/bin/bash
set -e
BINARY=${1:-dist/taufinity_darwin_universal}
VERSION=${2:-dev}

mkdir -p dist

# Inject binary into payload; trap ensures cleanup even if pkgbuild fails
cp "$BINARY" installer/payload/usr/local/bin/taufinity
chmod +x installer/payload/usr/local/bin/taufinity
trap 'rm -f installer/payload/usr/local/bin/taufinity' EXIT

pkgbuild \
  --root installer/payload \
  --scripts installer/scripts \
  --identifier io.taufinity.cli \
  --version "$VERSION" \
  --install-location / \
  dist/taufinity_darwin_installer.pkg

echo "Built: dist/taufinity_darwin_installer.pkg"
