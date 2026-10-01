#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# vendor.sh -- pull every third-party frontend asset into web/vendor/.
#
# Why this exists: a tamper-evidence product that fetches its stylesheet from a
# third-party CDN at runtime has a supply-chain hole. The whole argument of the
# project is "do not trust third parties", and cdn.jsdelivr.net is a third party
# that can change the interface at any moment.
#
# Re-runnable: it downloads from the pinned versions and rewrites absolute CDN
# URLs to local relative ones. Run it after bumping a version, then commit the
# result -- the repository is the source of truth, not the CDN.

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BEER_VERSION="5.0.3"
MDC_VERSION="1.1.4"
BEER_CDN="https://cdn.jsdelivr.net/npm/beercss@${BEER_VERSION}/dist/cdn"
MDC_CDN="https://cdn.jsdelivr.net/npm/material-dynamic-colors@${MDC_VERSION}/dist/cdn"

BEER_DIR="$ROOT/web/vendor/beer"
FONT_DIR="$ROOT/web/vendor/fonts"

# Google serves woff2 only to browsers it recognises.
UA="Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0 Safari/537.36"

FAMILIES='family=Bricolage+Grotesque:opsz,wdth,wght@12..96,75..100,200..800&family=Martian+Mono:wght@300..600&family=Roboto+Flex:wght@300..800&display=swap'

rm -rf "$BEER_DIR" "$FONT_DIR"
mkdir -p "$BEER_DIR" "$FONT_DIR"

echo "==> Beer CSS ${BEER_VERSION}"
curl -sSL --max-time 120 -o "$BEER_DIR/beer.min.css" "$BEER_CDN/beer.min.css"
curl -sSL --max-time 120 -o "$BEER_DIR/beer.min.js" "$BEER_CDN/beer.min.js"
curl -sSL --max-time 120 -o "$BEER_DIR/material-dynamic-colors.min.js" "$MDC_CDN/material-dynamic-colors.min.js"

# The 35 shape SVGs are referenced relatively from beer.min.css; fetch each one.
shapes=$(grep -oE 'url\([a-z0-9-]+\.svg\)' "$BEER_DIR/beer.min.css" | sed 's/url(//;s/)//' | sort -u)
for s in $shapes; do
  curl -sSL --max-time 60 -o "$BEER_DIR/$s" "$BEER_CDN/$s"
done
echo "    $(echo "$shapes" | wc -l) shape SVGs"

# The icon fonts are absolute CDN URLs inside the CSS; fetch them and rewrite.
fonts=$(grep -oE 'https://cdn\.jsdelivr\.net/npm/beercss@[^)]*\.woff2' "$BEER_DIR/beer.min.css" | sort -u)
for u in $fonts; do
  name="${u##*/}"
  curl -sSL --max-time 120 -o "$BEER_DIR/$name" "$u"
  # Rewrite the absolute URL to a sibling file, so the CSS resolves locally.
  sed -i "s|$u|$name|g" "$BEER_DIR/beer.min.css"
done
echo "    $(echo "$fonts" | wc -l) icon fonts"

echo "==> Google Fonts"
curl -sSL --max-time 120 -A "$UA" -o "$FONT_DIR/fonts.css" \
  "https://fonts.googleapis.com/css2?${FAMILIES}"

# Every @font-face points at fonts.gstatic.com; download each and rewrite.
count=0
for u in $(grep -oE 'https://fonts\.gstatic\.com/[^)]*' "$FONT_DIR/fonts.css" | sort -u); do
  name="$(echo "$u" | md5sum | cut -c1-12)-${u##*/}"
  curl -sSL --max-time 120 -o "$FONT_DIR/$name" "$u"
  sed -i "s|$u|$name|g" "$FONT_DIR/fonts.css"
  count=$((count + 1))
done
echo "    $count font files"

echo
echo "==> Left-over external URLs (must be empty):"
if grep -rhoE 'https?://[^)"'"'"' ]*' "$BEER_DIR" "$FONT_DIR" \
     | grep -vE '^https?://(www\.)?(w3\.org|schema\.org)' | sort -u | head; then :; fi
echo
echo "==> Total size:"
du -sh "$ROOT/web/vendor" | sed 's/^/    /'
