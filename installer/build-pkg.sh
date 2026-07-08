#!/bin/bash
set -e
BINARY=${1:-dist/taufinity_darwin_universal}
VERSION=${2:-dev}
PIXL_BASE_URL=${3:-https://studio.taufinity.io/pixl}

mkdir -p dist

# Substitute PIXL_BASE_URL placeholder in postinstall script
sed -i.bak "s|@@PIXL_BASE_URL@@|${PIXL_BASE_URL}|g" installer/scripts/postinstall
trap 'mv installer/scripts/postinstall.bak installer/scripts/postinstall 2>/dev/null; rm -f installer/payload/usr/local/bin/taufinity dist/taufinity_component.pkg' EXIT

# Inject binary into payload
cp "$BINARY" installer/payload/usr/local/bin/taufinity
chmod +x installer/payload/usr/local/bin/taufinity

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
