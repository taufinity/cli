#!/bin/bash
set -e
BINARY=${1:-dist/taufinity_darwin_universal}
VERSION=${2:-dev}

mkdir -p dist

# Inject binary into payload; trap ensures cleanup even if build steps fail
cp "$BINARY" installer/payload/usr/local/bin/taufinity
chmod +x installer/payload/usr/local/bin/taufinity
trap 'rm -f installer/payload/usr/local/bin/taufinity dist/taufinity_component.pkg' EXIT

# Step 1: component pkg (no installer UI)
pkgbuild \
  --root installer/payload \
  --scripts installer/scripts \
  --identifier io.taufinity.cli \
  --version "$VERSION" \
  --install-location / \
  dist/taufinity_component.pkg

# Step 2: distribution pkg — adds the privacy notice / license screen shown
#         during installation (user must click Agree before proceeding).
productbuild \
  --distribution installer/distribution.xml \
  --resources installer/Resources \
  --package-path dist \
  dist/taufinity_darwin_installer.pkg

echo "Built: dist/taufinity_darwin_installer.pkg"
