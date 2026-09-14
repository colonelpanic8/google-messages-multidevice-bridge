#!/usr/bin/env bash
# Rasterizes every shipped icon from client/favicon.svg (and the full-bleed
# tools/icons/maskable.svg, which PWA maskable icons need because the disc
# leaves the corners transparent).
set -euo pipefail

cd "$(dirname "$0")/.."

disc=client/favicon.svg
maskable=tools/icons/maskable.svg

render() {
  rsvg-convert -w "$2" -h "$2" "$1" -o "$3"
}

render "$disc" 32 client/icons/favicon-32.png
render "$disc" 192 client/icons/icon-192.png
render "$disc" 512 client/icons/icon-512.png
render "$maskable" 512 client/icons/icon-maskable-512.png
render "$disc" 512 desktop/icons/icon.png
render "$disc" 128 desktop/icons/icon-128.png
render "$disc" 128 desktop/icons/tray.png
