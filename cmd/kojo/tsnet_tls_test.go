package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"math/big"
	"net"
	"net/http"
	"testing"
	"time"
)

// TestTsnetTLSServesHTTP2 wires the listener + server TLS configs the
// tsnet path uses and checks a browser-like client actually gets h2
// (and that an h1-only client still works).
func TestTsnetTLSServesHTTP2(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert := &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}

	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln := tls.NewListener(raw, tsnetListenerTLSConfig(func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
		return cert, nil
	}))
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, r.Proto)
	})}
	srv.TLSConfig = tsnetServeTLSConfig()
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	url := "https://" + raw.Addr().String() + "/"
	get := func(tr *http.Transport) string {
		t.Helper()
		resp, err := (&http.Client{Transport: tr, Timeout: 5 * time.Second}).Get(url)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}
	// Separate configs: the h2 transport appends "h2" to its
	// TLSClientConfig.NextProtos in place.
	h2 := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, ForceAttemptHTTP2: true}
	if got := get(h2); got != "HTTP/2.0" {
		t.Fatalf("h2 client got %q, want HTTP/2.0", got)
	}
	h1Only := new(http.Protocols)
	h1Only.SetHTTP1(true)
	h1 := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, Protocols: h1Only}
	if got := get(h1); got != "HTTP/1.1" {
		t.Fatalf("h1 client got %q, want HTTP/1.1", got)
	}
}
