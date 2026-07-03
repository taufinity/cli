#!/bin/bash
set -e
BINARY=${1:?Usage: build-pkg.sh <binary-path> <version>}
VERSION=${2:?Usage: build-pkg.sh <binary-path> <version>}

mkdir -p dist

# Inject binary into payload (not committed; cleaned up after build)
cp "$BINARY" installer/payload/usr/local/bin/taufinity
chmod +x installer/payload/usr/local/bin/taufinity

pkgbuild \
  --root installer/payload \
  --scripts installer/scripts \
  --identifier io.taufinity.cli \
  --version "$VERSION" \
  --install-location / \
  dist/taufinity_darwin_installer.pkg

# Remove injected binary so it doesn't accidentally get committed
rm -f installer/payload/usr/local/bin/taufinity

echo "Built: dist/taufinity_darwin_installer.pkg"
