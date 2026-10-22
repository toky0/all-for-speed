#!/bin/sh
# SPDX-License-Identifier: GPL-3.0-or-later
# certbot deploy hook: copy the renewed certificate where the speedtest user can read it.
# The server notices the new files within a minute; no restart needed.
set -e
SRC=/etc/letsencrypt/live/speedtest
DST=/etc/speedtest/tls
install -m 0644 -o root -g speedtest "$SRC/fullchain.pem" "$DST/fullchain.pem.new"
install -m 0640 -o root -g speedtest "$SRC/privkey.pem"   "$DST/privkey.pem.new"
mv -f "$DST/privkey.pem.new"   "$DST/privkey.pem"
mv -f "$DST/fullchain.pem.new" "$DST/fullchain.pem"
