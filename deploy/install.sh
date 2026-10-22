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
dnf -y install golang certbot iproute-tc curl gzip

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
for n in "${ALL_NAMES[@]}"; do
  printf '%-34s A: %-16s AAAA: %s\n' "$n" \
    "$(getent ahostsv4 "$n" | awk 'NR==1{print $1}')" \
    "$(getent ahostsv6 "$n" | awk '$1 !~ /^::ffff:/ {print $1; exit}')"
done
echo
echo "Expected: every v4* name has an A record and no AAAA; every v6* name has an AAAA record and no A."
echo "The -h1 and -h2 names can be CNAMEs of v4.$DOMAIN / v6.$DOMAIN."
echo "Optional: HTTPS records (type 65) with alpn=\"h3,h2\" on v4/v6 only; see README."
echo "Logs: journalctl -u speedtest -f"
