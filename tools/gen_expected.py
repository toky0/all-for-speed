# SPDX-License-Identifier: GPL-3.0-or-later
"""Generate reference lookups with the official Python reader (pure-Python mode)."""
import base64, ipaddress, json, os, random, sys

os.environ["MAXMINDDB_PURE_PYTHON"] = "1"
import maxminddb

d = sys.argv[1]
out = {}
random.seed(7)
fixed = ["1.1.1.1", "1.1.1.3", "2.125.160.216", "81.2.69.160", "216.160.83.56", "89.160.20.112",
         "67.43.156.0", "202.196.224.0", "149.101.100.0", "::1.1.1.1", "::ffff:1.1.1.1",
         "2001:218::", "2001:220::1", "2a02:cf40::", "2c0f:ff40::", "::2:0:0", "::2:0:1",
         "1.2.3.4", "8.8.8.8", "::", "ffff::1"]

def norm(v):
    if isinstance(v, dict):
        return {k: norm(x) for k, x in v.items()}
    if isinstance(v, list):
        return [norm(x) for x in v]
    if isinstance(v, (bytes, bytearray)):
        return {"$bytes": base64.b64encode(v).decode()}
    if isinstance(v, int) and v >= 2**64:
        return {"$big": str(v)}
    return v

for name in sorted(os.listdir(d)):
    if not name.endswith(".mmdb") or "broken" in name.lower() or "invalid" in name.lower() or "limit" in name or "dos" in name:
        continue
    try:
        r = maxminddb.open_database(os.path.join(d, name), maxminddb.MODE_FILE)
    except Exception as e:
        continue
    ips = list(fixed)
    ips += [str(ipaddress.IPv4Address(random.getrandbits(32))) for _ in range(300)]
    ips += [str(ipaddress.IPv6Address(random.getrandbits(128))) for _ in range(150)]
    ips += [str(ipaddress.IPv6Address((0x2001 << 112) | random.getrandbits(112))) for _ in range(150)]
    res = {}
    for ip in ips:
        try:
            res[ip] = norm(r.get(ip))
        except (ValueError, maxminddb.InvalidDatabaseError):
            pass  # IPv6 lookup in an IPv4-only database
    out[name] = res
json.dump(out, open(sys.argv[2], "w"))
print(len(out), "databases,", sum(len(v) for v in out.values()), "lookups")
