package challtestsrv

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"fmt"
	"math"
	"math/big"
	"net/http"
	"time"

	"github.com/letsencrypt/boulder/va"
)

var cert = selfSignedCert()

func selfSignedCert() tls.Certificate {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic(fmt.Sprintf("Unable to generate HTTPS ECDSA key: %v", err))
	}

	serial, err := rand.Int(rand.Reader, big.NewInt(math.MaxInt64))
	if err != nil {
		panic(fmt.Sprintf("Unable to generate HTTPS cert serial number: %v", err))
	}

	template := &x509.Certificate{
		Subject: pkix.Name{
			CommonName: "challenge test server",
		},
		SerialNumber:          serial,
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(1, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	der, err := x509.CreateCertificate(rand.Reader, template, template, key.Public(), key)
	if err != nil {
		panic(fmt.Sprintf("Unable to issue HTTPS cert: %v", err))
	}

	return tls.Certificate{
		Certificate: [][]byte{der},
		PrivateKey:  key,
	}
}

// AddTLSALPNChallenge adds a new TLS-ALPN-01 key authorization for the given host
func (s *ChallSrv) AddTLSALPNChallenge(host, content string) {
	s.challMu.Lock()
	defer s.challMu.Unlock()
	s.tlsALPNOne[host] = content
}

// DeleteTLSALPNChallenge deletes the key authorization for a given host
func (s *ChallSrv) DeleteTLSALPNChallenge(host string) {
	s.challMu.Lock()
	defer s.challMu.Unlock()
	if _, ok := s.tlsALPNOne[host]; ok {
		delete(s.tlsALPNOne, host)
	}
}

// GetTLSALPNChallenge checks the s.tlsALPNOne map for the given host.
// If it is present it returns the key authorization and true, if not
// it returns an empty string and false.
func (s *ChallSrv) GetTLSALPNChallenge(host string) (string, bool) {
	s.challMu.RLock()
	defer s.challMu.RUnlock()
	content, present := s.tlsALPNOne[host]
	return content, present
}

func (s *ChallSrv) ServeChallengeCertFunc(k *ecdsa.PrivateKey) func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
		if len(hello.SupportedProtos) != 1 || hello.SupportedProtos[0] != va.ACMETLS1Protocol {
			return &cert, nil
		}

		ka, found := s.GetTLSALPNChallenge(hello.ServerName)
		if !found {
			return nil, fmt.Errorf("unknown ClientHelloInfo.ServerName: %s", hello.ServerName)
		}

		kaHash := sha256.Sum256([]byte(ka))
		extValue, err := asn1.Marshal(kaHash[:])
		if err != nil {
			return nil, fmt.Errorf("failed marshalling hash OCTET STRING: %s", err)
		}
		certTmpl := x509.Certificate{
			SerialNumber: big.NewInt(1729),
			DNSNames:     []string{hello.ServerName},
			ExtraExtensions: []pkix.Extension{
				{
					Id:       va.IdPeAcmeIdentifier,
					Critical: true,
					Value:    extValue,
				},
			},
		}
		certBytes, err := x509.CreateCertificate(rand.Reader, &certTmpl, &certTmpl, k.Public(), k)
		if err != nil {
			return nil, fmt.Errorf("failed creating challenge certificate: %s", err)
		}
		return &tls.Certificate{
			Certificate: [][]byte{certBytes},
			PrivateKey:  k,
		}, nil
	}
}

type challTLSServer struct {
	*http.Server
}

func (c challTLSServer) Shutdown() error {
	return c.Server.Shutdown(context.Background())
}

func (c challTLSServer) ListenAndServe() error {
	// We never want to serve a plain cert so leave certFile and keyFile
	// empty. If we don't know the SNI name/ALPN fails the handshake will
	// fail anyway.
	return c.Server.ListenAndServeTLS("", "")
}

func tlsALPNOneServer(address string, challSrv *ChallSrv) challengeServer {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic(err)
	}
	srv := &http.Server{
		Addr:         address,
		Handler:      challSrv,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 5 * time.Second,
		TLSConfig: &tls.Config{
			NextProtos:     []string{va.ACMETLS1Protocol},
			GetCertificate: challSrv.ServeChallengeCertFunc(key),
		},
	}
	srv.SetKeepAlivesEnabled(false)
	return challTLSServer{srv}
}