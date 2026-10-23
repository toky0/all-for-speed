#!/bin/sh
# SPDX-License-Identifier: GPL-3.0-or-later
# certbot deploy hook: copy a renewed certificate where the speedtest user can read
# it, one subdirectory per certificate (/etc/speedtest/tls/<cert-name>/). The server
# notices new files within a minute; no restart needed.
set -e
: "${RENEWED_LINEAGE:?run by certbot, or set RENEWED_LINEAGE=/etc/letsencrypt/live/<name>}"
DST=/etc/speedtest/tls/$(basename "$RENEWED_LINEAGE")
install -d -m 0750 -o root -g speedtest "$DST"
install -m 0644 -o root -g speedtest "$RENEWED_LINEAGE/fullchain.pem" "$DST/fullchain.pem.new"
install -m 0640 -o root -g speedtest "$RENEWED_LINEAGE/privkey.pem"   "$DST/privkey.pem.new"
mv -f "$DST/privkey.pem.new"   "$DST/privkey.pem"
mv -f "$DST/fullchain.pem.new" "$DST/fullchain.pem"
