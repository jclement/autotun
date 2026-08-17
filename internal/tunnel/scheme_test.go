package tunnel

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/jclement/autotun/internal/config"
	"github.com/jclement/autotun/internal/probe"
)

func TestSchemeNext(t *testing.T) {
	tests := []struct {
		in   Scheme
		want Scheme
	}{
		{SchemeUnknown, SchemeHTTP},
		{SchemeHTTP, SchemeHTTPS},
		{SchemeHTTPS, SchemeUnknown},
		{Scheme("garbage"), SchemeHTTP},
	}
	for _, tt := range tests {
		if got := tt.in.Next(); got != tt.want {
			t.Errorf("Scheme(%q).Next() = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestParseScheme(t *testing.T) {
	tests := map[string]Scheme{
		"http":  SchemeHTTP,
		"https": SchemeHTTPS,
		"":      SchemeUnknown,
		"ftp":   SchemeUnknown,
	}
	for in, want := range tests {
		if got := ParseScheme(in); got != want {
			t.Errorf("ParseScheme(%q) = %q, want %q", in, got, want)
		}
	}
}

// An unknown scheme still opens as http, because that is right far more often
// than not on a development box.
func TestSchemeURLScheme(t *testing.T) {
	tests := map[Scheme]string{
		SchemeUnknown: "http",
		SchemeHTTP:    "http",
		SchemeHTTPS:   "https",
	}
	for in, want := range tests {
		if got := in.URLScheme(); got != want {
			t.Errorf("Scheme(%q).URLScheme() = %q, want %q", in, got, want)
		}
	}
}

func TestSchemeLabel(t *testing.T) {
	// Plain words, not punctuation the reader has to decode.
	if got := SchemeUnknown.Label(); got != "unknown" {
		t.Errorf("unknown label = %q, want unknown", got)
	}
	if got := SchemeHTTPS.Label(); got != "https" {
		t.Errorf("https label = %q", got)
	}
}

// tlsServer starts a TLS listener with a self-signed certificate.
func tlsServer(t *testing.T) string {
	t.Helper()

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := tls.X509KeyPair(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
	)
	if err != nil {
		t.Fatal(err)
	}

	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_, _ = io.WriteString(conn, "HTTP/1.1 200 OK\r\n\r\n")
			}()
		}
	}()
	return ln.Addr().String()
}

func TestDetectSchemeFindsTLS(t *testing.T) {
	addr := tlsServer(t)
	if got := detectScheme(&net.Dialer{}, addr); got != SchemeHTTPS {
		t.Errorf("detectScheme against a TLS server = %q, want https", got)
	}
}

// A plaintext server must be left unknown rather than asserted as http: the
// probe only sends a ClientHello, so a failure proves nothing.
func TestDetectSchemeLeavesPlaintextUnknown(t *testing.T) {
	echo := newEchoServer(t)
	if got := detectScheme(&net.Dialer{}, echo.addr()); got != SchemeUnknown {
		t.Errorf("detectScheme against a plaintext server = %q, want unknown", got)
	}
}

func TestDetectSchemeHandlesADeadPort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	if got := detectScheme(&net.Dialer{}, addr); got != SchemeUnknown {
		t.Errorf("detectScheme against a closed port = %q, want unknown", got)
	}
}

// memoryStore is an in-memory Settings implementation.
type memoryStore struct {
	mu     sync.Mutex
	values map[string]config.Port
}

func newMemoryStore() *memoryStore { return &memoryStore{values: map[string]config.Port{}} }

func (m *memoryStore) key(host string, port int) string {
	return fmt.Sprintf("%s:%d", host, port)
}

func (m *memoryStore) Port(host string, port int) config.Port {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.values[m.key(host, port)]
}

func (m *memoryStore) SetPort(host string, port int, p config.Port) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p.IsZero() {
		delete(m.values, m.key(host, port))
		return
	}
	m.values[m.key(host, port)] = p
}

// scheme returns the remembered scheme for a port, if any.
func (m *memoryStore) scheme(host string, port int) (string, bool) {
	p := m.Port(host, port)
	return p.Scheme, p.Scheme != ""
}

func TestManagerCycleSchemeIsRemembered(t *testing.T) {
	echo := newEchoServer(t)
	memory := newMemoryStore()
	m := New(NewAllocator("127.0.0.1", false), &fixedDialer{addr: echo.addr()}, Options{
		Policy:   DefaultPolicy(),
		Host:     "devbox",
		Settings: memory,
	})
	defer m.Close()

	port := freePort(t)
	m.Sync(probe.Snapshot{})
	m.Sync(snapshotOf(port))

	if got := m.CycleScheme(port); got != SchemeHTTP {
		t.Fatalf("first cycle = %q, want http", got)
	}
	if got, ok := memory.scheme("devbox", port); !ok || got != "http" {
		t.Errorf("the choice was not persisted: %q, %v", got, ok)
	}
	if st := stateFor(t, m, port); st.Scheme != SchemeHTTP || !st.SchemePinned {
		t.Errorf("state = %q pinned %v, want http/true", st.Scheme, st.SchemePinned)
	}
	if got := stateFor(t, m, port).URL(); got == "" {
		t.Error("an active tunnel should have a URL")
	}

	if got := m.CycleScheme(port); got != SchemeHTTPS {
		t.Fatalf("second cycle = %q, want https", got)
	}
	if got := stateFor(t, m, port).URL(); got[:5] != "https" {
		t.Errorf("URL = %q, want an https URL", got)
	}

	// Cycling back to unknown clears the memory rather than storing a blank.
	if got := m.CycleScheme(port); got != SchemeUnknown {
		t.Fatalf("third cycle = %q, want unknown", got)
	}
	if _, ok := memory.scheme("devbox", port); ok {
		t.Error("cycling back to unknown should forget the entry")
	}
	if st := stateFor(t, m, port); st.SchemePinned {
		t.Error("unknown should release the pin so detection can run again")
	}
}

func TestManagerRestoresRememberedScheme(t *testing.T) {
	echo := newEchoServer(t)
	memory := newMemoryStore()
	port := freePort(t)
	memory.SetPort("devbox", port, config.Port{Scheme: "https"})

	m := New(NewAllocator("127.0.0.1", false), &fixedDialer{addr: echo.addr()}, Options{
		Policy:   DefaultPolicy(),
		Host:     "devbox",
		Settings: memory,
	})
	defer m.Close()

	m.Sync(probe.Snapshot{})
	m.Sync(snapshotOf(port))

	st := stateFor(t, m, port)
	if st.Scheme != SchemeHTTPS {
		t.Errorf("scheme = %q, want the remembered https", st.Scheme)
	}
	if !st.SchemePinned {
		t.Error("a remembered scheme should be pinned")
	}
}

func TestManagerDetectionDoesNotOverridePinnedSchemes(t *testing.T) {
	memory := newMemoryStore()
	port := freePort(t)
	memory.SetPort("devbox", port, config.Port{Scheme: "http"})

	// The dialer points at a real TLS server, so detection would say https.
	m := New(NewAllocator("127.0.0.1", false), &fixedDialer{addr: tlsServer(t)}, Options{
		Policy:        DefaultPolicy(),
		Host:          "devbox",
		Settings:      memory,
		DetectSchemes: true,
	})
	defer m.Close()

	m.Sync(probe.Snapshot{})
	m.Sync(snapshotOf(port))

	// Give any detection goroutine a chance to run and be ignored.
	time.Sleep(300 * time.Millisecond)
	if st := stateFor(t, m, port); st.Scheme != SchemeHTTP {
		t.Errorf("scheme = %q, want the pinned http to survive detection", st.Scheme)
	}
}

func TestManagerDetectsHTTPS(t *testing.T) {
	m := New(NewAllocator("127.0.0.1", false), &fixedDialer{addr: tlsServer(t)}, Options{
		Policy:        DefaultPolicy(),
		Host:          "devbox",
		DetectSchemes: true,
	})
	defer m.Close()

	port := freePort(t)
	m.Sync(probe.Snapshot{})
	m.Sync(snapshotOf(port))

	waitUntil(t, func() bool {
		return stateFor(t, m, port).Scheme == SchemeHTTPS
	}, "the TLS probe to report https")

	// A detected scheme is not pinned, so it is not written to the store.
	if st := stateFor(t, m, port); st.SchemePinned {
		t.Error("a detected scheme should not be pinned")
	}
}

func TestManagerDetectionCanBeDisabled(t *testing.T) {
	m := New(NewAllocator("127.0.0.1", false), &fixedDialer{addr: tlsServer(t)}, Options{
		Policy:        DefaultPolicy(),
		Host:          "devbox",
		DetectSchemes: false,
	})
	defer m.Close()

	port := freePort(t)
	m.Sync(probe.Snapshot{})
	m.Sync(snapshotOf(port))

	time.Sleep(300 * time.Millisecond)
	if st := stateFor(t, m, port); st.Scheme != SchemeUnknown {
		t.Errorf("scheme = %q, want unknown with detection off", st.Scheme)
	}
}

func TestManagerCycleSchemeOnAnUnknownPort(t *testing.T) {
	echo := newEchoServer(t)
	m := New(NewAllocator("127.0.0.1", false), &fixedDialer{addr: echo.addr()}, Options{Policy: DefaultPolicy()})
	defer m.Close()

	if got := m.CycleScheme(9999); got != SchemeUnknown {
		t.Errorf("CycleScheme on an unknown port = %q, want unknown", got)
	}
}
