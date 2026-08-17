package tunnel

import (
	"crypto/tls"
	"net"
	"time"
)

// Scheme is what a forwarded port turns out to speak. It decides the URL the
// `o` and space keys open, and is remembered per host and port between runs.
type Scheme string

const (
	// SchemeUnknown means we have not been told and could not tell. Opening
	// still assumes HTTP, which is right far more often than not on a dev box.
	SchemeUnknown Scheme = ""
	SchemeHTTP    Scheme = "http"
	SchemeHTTPS   Scheme = "https"
)

// schemeCycle is the order the `t` key steps through.
var schemeCycle = []Scheme{SchemeUnknown, SchemeHTTP, SchemeHTTPS}

// Next returns the following scheme in the cycle.
func (s Scheme) Next() Scheme {
	for i, c := range schemeCycle {
		if c == s {
			return schemeCycle[(i+1)%len(schemeCycle)]
		}
	}
	return SchemeHTTP
}

// ParseScheme validates a remembered value.
func ParseScheme(s string) Scheme {
	switch Scheme(s) {
	case SchemeHTTP:
		return SchemeHTTP
	case SchemeHTTPS:
		return SchemeHTTPS
	default:
		return SchemeUnknown
	}
}

// URLScheme returns the scheme to build a URL with, defaulting to http.
func (s Scheme) URLScheme() string {
	if s == SchemeHTTPS {
		return "https"
	}
	return "http"
}

// Label renders the scheme for the table.
func (s Scheme) Label() string {
	if s == SchemeUnknown {
		return "—"
	}
	return string(s)
}

// probeTimeout bounds the TLS detection handshake.
const probeTimeout = 4 * time.Second

// detectScheme guesses whether a remote service speaks TLS.
//
// It only ever attempts a TLS handshake. A successful one is conclusive
// evidence of HTTPS; a failure proves nothing, so the scheme is left unknown
// rather than being asserted as HTTP. Deliberately no plaintext HTTP probe is
// sent: firing a GET at whatever happens to be listening — a database, a
// message broker — is not something a port forwarder should do uninvited.
func detectScheme(d Dialer, addr string) Scheme {
	conn, err := d.Dial("tcp", addr)
	if err != nil {
		return SchemeUnknown
	}
	defer conn.Close()

	_ = conn.SetDeadline(time.Now().Add(probeTimeout))
	tlsConn := tls.Client(conn, &tls.Config{
		InsecureSkipVerify: true, //nolint:gosec // identity is irrelevant; we only want to know if it speaks TLS
		ServerName:         "localhost",
	})
	if err := tlsConn.Handshake(); err != nil {
		return SchemeUnknown
	}
	return SchemeHTTPS
}

// ensure detectScheme's signature stays compatible with net.Dialer in tests.
var _ = func() Dialer { return &net.Dialer{} }
