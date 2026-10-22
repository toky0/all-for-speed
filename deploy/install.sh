#!/usr/bin/env bash
# SPDX-License-Identifier: GPL-3.0-or-later
# install.sh — build and install the speed test on AlmaLinux 10. Run as root from
# the project directory:
#   DOMAIN=speedtest.example.org NAME="Example speed test" EMAIL=noc@example.org bash deploy/install.sh
#
#   DOMAIN   main hostname (required on first install); v4.<DOMAIN>, v4-h1.<DOMAIN> and
#            v4-h2.<DOMAIN> must have only A records, the v6 names only AAAA records
#   NAME     site name shown on the page (default "Speed test")
#   LOCATION server location shown on the page, e.g. "Paris, France" (default: none)
#   EMAIL    Let's Encrypt account email; without it the certificate step is skipped
#   FQ_IFACE interface to give the fq qdisc (default: the IPv4 default-route interface)
#
# Safe to re-run: rebuilds the binary and refreshes units, keeps certificates and data.
set -euo pipefail

# On re-runs, settings come from the existing env file unless given again.
ENV_DOMAIN="" ENV_NAME="" ENV_LOCATION=""
if [[ -f /etc/speedtest/speedtest.env ]]; then
  while IFS='=' read -r key val; do
    val="${val%\"}"; val="${val#\"}"          # values may or may not be quoted
    case "$key" in
      DOMAIN) ENV_DOMAIN="$val" ;;
      NAME)   ENV_NAME="$val" ;;
      LOCATION|LABEL) ENV_LOCATION="$val" ;;   # LABEL: name used by earlier versions
    esac
  done < /etc/speedtest/speedtest.env
fi
DOMAIN="${DOMAIN:-${ENV_DOMAIN:-}}"
NAME="${NAME:-${ENV_NAME:-Speed test}}"
LOCATION="${LOCATION:-${ENV_LOCATION:-}}"
[[ -n "$DOMAIN" ]] || { echo "Set DOMAIN, e.g. DOMAIN=speedtest.example.org bash deploy/install.sh"; exit 1; }
EMAIL="${EMAIL:-}"
SRC="$(cd "$(dirname "$0")/.." && pwd)"

log()  { printf '\n\033[1;32m==> %s\033[0m\n' "$*"; }
warn() { printf '\033[1;33m[!] %s\033[0m\n' "$*"; }
[[ $EUID -eq 0 ]] || { echo "Run as root."; exit 1; }

log "Installing packages"
dnf -y install epel-release
dnf -y install golang certbot iproute-tc curl gzip bind-utils

log "Building"
cd "$SRC"
# Modules come from proxy.golang.org and are checked against sum.golang.org.
# If the distro Go is older than go.mod asks for, Go fetches the right toolchain itself.
export GOTOOLCHAIN=auto GOFLAGS=-mod=mod CGO_ENABLED=0
go version
go mod download
go mod verify
go build -trimpath -ldflags="-s -w" -o /usr/local/bin/speedtest .

log "User and directories"
id speedtest &>/dev/null || useradd --system --no-create-home --home-dir /var/lib/speedtest \
  --shell /sbin/nologin speedtest
install -d -m 0755 -o speedtest -g speedtest /var/lib/speedtest /var/lib/speedtest/geo
install -d -m 0755 -o root -g root /var/lib/speedtest/acme
install -d -m 0755 /etc/speedtest
install -d -m 0750 -o root -g speedtest /etc/speedtest/tls
install -d -m 0755 /opt/speedtest
install -m 0644 README.md /opt/speedtest/README.md 2>/dev/null || true

# Write the settings every run so DOMAIN/NAME/LOCATION given on the command line stick;
# EXTRA_ARGS is kept as it was.
EXTRA_ARGS_KEEP=$(grep -E '^EXTRA_ARGS=' /etc/speedtest/speedtest.env 2>/dev/null || echo 'EXTRA_ARGS=')
cat > /etc/speedtest/speedtest.env <<EOF
DOMAIN=$DOMAIN
NAME="$NAME"
LOCATION="$LOCATION"
# extra flags, e.g. -default-proto h3 or -h3=false
$EXTRA_ARGS_KEEP
EOF

log "systemd units"
install -m 0644 deploy/speedtest.service       /etc/systemd/system/speedtest.service
install -m 0644 deploy/speedtest-geoip.service /etc/systemd/system/speedtest-geoip.service
install -m 0644 deploy/speedtest-geoip.timer   /etc/systemd/system/speedtest-geoip.timer
install -m 0755 deploy/speedtest-geoip-update  /usr/local/sbin/speedtest-geoip-update
install -m 0755 deploy/certbot-deploy-hook.sh  /usr/local/sbin/speedtest-cert-deploy

# fq qdisc on the container's own interface (pacing for BBR at 10G; does not touch the host)
IFACE="${FQ_IFACE:-$(ip -o -4 route show default | awk '{print $5; exit}')}"
if [[ -n "$IFACE" ]]; then
  cat > /etc/systemd/system/speedtest-fq.service <<EOF
[Unit]
Description=fq qdisc on $IFACE for the speed test
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/sbin/tc qdisc replace dev $IFACE root fq
ExecStop=/usr/sbin/tc qdisc del dev $IFACE root

[Install]
WantedBy=multi-user.target
EOF
fi

systemctl daemon-reload
[[ -n "$IFACE" ]] && { systemctl enable --now speedtest-fq.service || warn "Could not set fq on $IFACE (no CAP_NET_ADMIN in the CT?)"; }

log "Firewall: HTTP/3 needs UDP 443"
if firewall-cmd --state &>/dev/null; then
  Z=$(firewall-cmd --get-default-zone)
  # one rule type per call: some firewalld versions reject --add-service with --add-port
  firewall-cmd --permanent --zone="$Z" --add-service=http >/dev/null
  firewall-cmd --permanent --zone="$Z" --add-service=https >/dev/null
  firewall-cmd --permanent --zone="$Z" --query-service=http3 &>/dev/null \
    || firewall-cmd --permanent --zone="$Z" --add-port=443/udp >/dev/null
  firewall-cmd --reload >/dev/null
  echo "Open in zone $Z: $(firewall-cmd --zone="$Z" --list-services) $(firewall-cmd --zone="$Z" --list-ports)"
else
  warn "firewalld not running; make sure TCP 80, TCP 443 and UDP 443 are reachable."
fi

rmax=$(cat /proc/sys/net/core/rmem_max 2>/dev/null || echo 0)
if (( rmax < 7500000 )); then
  warn "UDP buffer limit is $rmax bytes. HTTP/3 works but slows down at high rates."
  warn "On the Proxmox HOST: sysctl -w net.core.rmem_max=7500000 net.core.wmem_max=7500000 (and persist it)."
fi

log "GeoIP databases (DB-IP Lite)"
if [[ ! -s /var/lib/speedtest/geo/city.mmdb || ! -s /var/lib/speedtest/geo/asn.mmdb ]]; then
  /usr/local/sbin/speedtest-geoip-update || warn "GeoIP download failed; the page will show addresses without location."
fi
systemctl enable --now speedtest-geoip.timer

log "Starting the server"
systemctl enable speedtest
systemctl restart speedtest
sleep 1
systemctl --no-pager --lines=5 status speedtest || true

log "TLS certificate"
# One certificate for the page and every test hostname. The -h1/-h2 names are added
# as soon as their DNS records exist; re-run this script after creating them.
ALL_NAMES=("$DOMAIN" "v4.$DOMAIN" "v6.$DOMAIN" "v4-h2.$DOMAIN" "v6-h2.$DOMAIN" "v4-h1.$DOMAIN" "v6-h1.$DOMAIN")
NAMES=()
for n in "${ALL_NAMES[@]}"; do
  if getent ahosts "$n" >/dev/null; then NAMES+=("$n"); else warn "$n has no DNS record yet; left out of the certificate."; fi
done
MISSING=()
if [[ -s /etc/speedtest/tls/fullchain.pem ]]; then
  SANS=$(openssl x509 -in /etc/speedtest/tls/fullchain.pem -noout -ext subjectAltName 2>/dev/null || true)
  for n in "${NAMES[@]}"; do grep -q "DNS:$n\b" <<<"$SANS" || MISSING+=("$n"); done
fi
if [[ -s /etc/speedtest/tls/fullchain.pem && ${#MISSING[@]} -eq 0 ]]; then
  echo "Certificate covers: ${NAMES[*]}. certbot renews it automatically."
elif [[ -z "$EMAIL" ]]; then
  warn "EMAIL not set, skipping. Names still to cover: ${MISSING[*]:-${NAMES[*]}}. Re-run with EMAIL=you@example.org"
else
  [[ ${#MISSING[@]} -gt 0 ]] && echo "Adding to the certificate: ${MISSING[*]}"
  D_ARGS=(); for n in "${NAMES[@]}"; do D_ARGS+=(-d "$n"); done
  certbot certonly --non-interactive --agree-tos -m "$EMAIL" \
    --webroot -w /var/lib/speedtest/acme \
    --cert-name speedtest --key-type ecdsa --expand \
    "${D_ARGS[@]}" \
    --deploy-hook /usr/local/sbin/speedtest-cert-deploy \
    && echo "Certificate installed. The server picks it up within a minute." \
    || warn "certbot failed. Check that every name above points here and TCP 80 is open."
fi
systemctl enable --now certbot-renew.timer 2>/dev/null || true

log "Checks"
PROBLEMS=0 NOTES=0
ok()   { printf '  \033[1;32mok\033[0m    %s\n' "$*"; }
bad()  { printf '  \033[1;31mfix\033[0m   %s\n' "$*"; PROBLEMS=$((PROBLEMS + 1)); }
note() { printf '  \033[1;33mnote\033[0m  %s\n' "$*"; NOTES=$((NOTES + 1)); }
# Ask DNS directly, as visitors do. getent would read /etc/hosts first, where Proxmox
# lists the container's own hostname with its IPv4 address only.
# An empty answer is a normal result here, not an error for set -e / pipefail.
a_of()    { dig +short A "$1" 2>/dev/null | awk '/^[0-9.]+$/ {print; exit}' || true; }
aaaa_of() { dig +short AAAA "$1" 2>/dev/null | awk '/^[0-9a-fA-F:]+$/ && !/^::ffff:/ {print; exit}' || true; }

# DNS: the page name should have both families; each test name exactly one.
A=$(a_of "$DOMAIN"); AAAA=$(aaaa_of "$DOMAIN")
if [[ -n $A && -n $AAAA ]]; then ok "$DOMAIN  A $A  AAAA $AAAA"
elif [[ -n $A ]]; then note "$DOMAIN has no AAAA record, so visitors on IPv6-only networks can't open the page. Add one: $DOMAIN AAAA $(aaaa_of "v6.$DOMAIN")"
elif [[ -n $AAAA ]]; then note "$DOMAIN has no A record, so visitors on IPv4-only networks can't open the page. Add one: $DOMAIN A $(a_of "v4.$DOMAIN")"
else bad "$DOMAIN has no A or AAAA record."
fi
for p in "" -h2 -h1; do
  case $p in -h1) proto="HTTP/1.1" ;; -h2) proto="HTTP/2" ;; *) proto="HTTP/3" ;; esac
  n="v4$p.$DOMAIN"; A=$(a_of "$n"); AAAA=$(aaaa_of "$n")
  if [[ -n $A && -z $AAAA ]]; then ok "$n  A $A  (IPv4 over $proto)"
  elif [[ -z $A ]]; then bad "$n needs an A record; IPv4 tests over $proto are unavailable until it exists."
  else bad "$n must not have an AAAA record ($AAAA); remove it so this name stays IPv4-only."
  fi
  n="v6$p.$DOMAIN"; A=$(a_of "$n"); AAAA=$(aaaa_of "$n")
  if [[ -n $AAAA && -z $A ]]; then ok "$n  AAAA $AAAA  (IPv6 over $proto)"
  elif [[ -z $AAAA ]]; then bad "$n needs an AAAA record; IPv6 tests over $proto are unavailable until it exists."
  else bad "$n must not have an A record ($A); remove it so this name stays IPv6-only."
  fi
done

# Certificate
if [[ -s /etc/speedtest/tls/fullchain.pem ]]; then
  SANS=$(openssl x509 -in /etc/speedtest/tls/fullchain.pem -noout -ext subjectAltName 2>/dev/null || true)
  UNCOVERED=()
  for n in "${ALL_NAMES[@]}"; do grep -q "DNS:$n\b" <<<"$SANS" || UNCOVERED+=("$n"); done
  END=$(openssl x509 -in /etc/speedtest/tls/fullchain.pem -noout -enddate 2>/dev/null | cut -d= -f2)
  if [[ ${#UNCOVERED[@]} -eq 0 ]]; then ok "TLS certificate covers all 7 names, valid until $END"
  else bad "TLS certificate is missing ${UNCOVERED[*]}; re-run with EMAIL=... once their DNS records exist."
  fi
else
  bad "No TLS certificate yet; re-run with EMAIL=you@example.org once DNS points here."
fi

# Services and data
if systemctl is-active --quiet speedtest; then ok "speedtest service is running"
else bad "speedtest service is not running; see: journalctl -u speedtest -n 50"
fi
if curl -fsS -o /dev/null --max-time 5 --resolve "$DOMAIN:443:127.0.0.1" "https://$DOMAIN/api/ping" 2>/dev/null; then
  ok "https://$DOMAIN answers with a valid certificate"
elif [[ -s /etc/speedtest/tls/fullchain.pem ]]; then
  bad "https://$DOMAIN doesn't answer locally; see: journalctl -u speedtest -n 50"
fi
for t in speedtest-geoip.timer certbot-renew.timer; do
  if systemctl is-active --quiet "$t"; then ok "$t is scheduled"; else note "$t is not active: systemctl enable --now $t"; fi
done
if [[ -s /var/lib/speedtest/geo/city.mmdb && -s /var/lib/speedtest/geo/asn.mmdb ]]; then ok "GeoIP databases installed"
else note "GeoIP databases missing; the page shows addresses without ASN or city. Run: /usr/local/sbin/speedtest-geoip-update"
fi
rmax=$(cat /proc/sys/net/core/rmem_max 2>/dev/null || echo 0)
if (( rmax >= 7500000 )); then ok "UDP buffers allow full-speed HTTP/3"
else note "UDP buffer limit is $rmax bytes; raise net.core.rmem_max/wmem_max to 7500000 on the host for full-speed HTTP/3."
fi

if (( PROBLEMS == 0 )); then
  log "Install complete"
  echo "The page is live at https://$DOMAIN"
  if (( NOTES == 1 )); then echo "The note above is an optional improvement."
  elif (( NOTES > 1 )); then echo "The $NOTES notes above are optional improvements."
  fi
else
  if (( PROBLEMS == 1 )); then msg="1 problem"; else msg="$PROBLEMS problems"; fi
  printf '\n\033[1;31m==> Install finished with %s to fix\033[0m\n' "$msg"
  echo "Fix the lines marked \"fix\" above, then run this script again."
  echo "Once fixed, the page will be at https://$DOMAIN"
fi
cat <<EOF

Useful commands:
  systemctl status speedtest                           server status
  journalctl -u speedtest -f                           live server log
  systemctl list-timers speedtest-geoip certbot-renew  GeoIP and certificate renewals
  /etc/speedtest/speedtest.env                         settings (then: systemctl restart speedtest)
EOF
