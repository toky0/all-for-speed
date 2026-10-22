#!/usr/bin/env bash
# SPDX-License-Identifier: GPL-3.0-or-later
#
# Regenerate every page icon from one square SVG logo.
#   dnf install librsvg2-tools        # provides rsvg-convert
#   tools/make-icons.sh path/to/logo.svg [background-colour]
#
# The logo should be square and look right with its own background. iOS rounds
# the corners of apple-touch-icon.png itself and shows transparency as black, so
# that file is flattened onto the background colour (default #f7f9fb).
set -euo pipefail
SRC=${1:?usage: make-icons.sh logo.svg [background]}
BG=${2:-#f7f9fb}
OUT="$(cd "$(dirname "$0")/.." && pwd)/web"
command -v rsvg-convert >/dev/null || { echo "rsvg-convert not found: dnf install librsvg2-tools"; exit 1; }

cp "$SRC" "$OUT/favicon.svg"
rsvg-convert -w 32  -h 32  "$SRC" -o "$OUT/favicon-32.png"
rsvg-convert -w 180 -h 180 -b "$BG" "$SRC" -o "$OUT/apple-touch-icon.png"
rsvg-convert -w 192 -h 192 "$SRC" -o "$OUT/icon-192.png"
rsvg-convert -w 512 -h 512 "$SRC" -o "$OUT/icon-512.png"
# Android "maskable" icon: logo at 80% on a full-bleed background (safe zone)
TMP=$(mktemp --suffix=.svg)
trap 'rm -f "$TMP"' EXIT
cat > "$TMP" <<EOF
<svg xmlns="http://www.w3.org/2000/svg" xmlns:xlink="http://www.w3.org/1999/xlink" viewBox="0 0 512 512">
  <rect width="512" height="512" fill="$BG"/>
  <image x="51" y="51" width="410" height="410" xlink:href="file://$(realpath "$SRC")"/>
</svg>
EOF
rsvg-convert -w 512 -h 512 "$TMP" -o "$OUT/icon-maskable-512.png"
echo "Icons written to $OUT. Rebuild the binary to embed them."
