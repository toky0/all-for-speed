module git.marwan.ma/MARWAN/all-for-speed

go 1.26.10

require github.com/quic-go/quic-go v0.59.1

// quic-go requires older versions of these; the newer ones fix known
// vulnerabilities in packages compiled into the server (GO-2026-5026, GO-2026-5970).
require (
	golang.org/x/crypto v0.58.0 // indirect
	golang.org/x/net v0.62.0 // indirect
	golang.org/x/sys v0.49.0 // indirect
	golang.org/x/text v0.43.0 // indirect
)
