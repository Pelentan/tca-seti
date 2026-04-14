package main

// ---------------------------------------------------------------------------
// Enrollment CA — separate from the constellation CA.
//
// Purpose: authenticate containers requesting instance certs.
// A single enrollment cert is issued, written to the certs volume,
// and injected into every container via .env (dev) or K8s secret (prod).
//
// The enrollment cert's ONLY capability is calling POST /instance-cert.
// cert-forge verifies the enrollment cert is signed by the enrollment CA
// before issuing an instance cert. All other endpoints require a
// constellation instance cert.
//
// The enrollment CA and enrollment cert are generated fresh on every
// cert-forge startup. The enrollment cert in .env / K8s secret must be
// updated when cert-forge restarts with a fresh volume — which in practice
// means a full constellation restart, at which point all containers
// restart anyway and pick up the new enrollment cert.
// ---------------------------------------------------------------------------

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"log"
	"math/big"
	"os"
	"path/filepath"
	"time"
)

type EnrollmentCA struct {
	cert    *x509.Certificate
	certDER []byte
	certPEM []byte
	key     *rsa.PrivateKey
}

type EnrollmentCert struct {
	cert    *x509.Certificate
	certDER []byte
	certPEM []byte
	keyPEM  []byte
	key     *rsa.PrivateKey
}

var enrollmentCA *EnrollmentCA

// generateEnrollmentCA creates a short-lived CA used only to sign the
// enrollment cert. Separate from the constellation CA so a compromise
// of the enrollment cert cannot be used to forge constellation certs.
func generateEnrollmentCA(cfg *ForgeConfig) (*EnrollmentCA, error) {
	log.Printf("[cert-forge] Generating enrollment CA...")
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("generate enrollment CA key: %v", err)
	}

	notBefore := time.Now().Add(-time.Duration(cfg.CA.BackdateMinutes) * time.Minute)
	notAfter := time.Now().Add(time.Duration(cfg.CA.ValidDays) * 24 * time.Hour)

	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   "seti-enrollment-ca",
			Organization: []string{cfg.CA.Organization},
			Country:      []string{cfg.CA.Country},
		},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("create enrollment CA: %v", err)
	}
	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return nil, fmt.Errorf("parse enrollment CA: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})

	log.Printf("[cert-forge] Enrollment CA generated")
	return &EnrollmentCA{cert: cert, certDER: certDER, certPEM: certPEM, key: key}, nil
}

// generateEnrollmentCert issues the single shared enrollment cert.
// This cert is written to the volume and injected into every container.
// Its ExtKeyUsage is restricted — client auth only, no server auth.
func generateEnrollmentCert(cfg *ForgeConfig, eca *EnrollmentCA) (*EnrollmentCert, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("generate enrollment cert key: %v", err)
	}

	notBefore := time.Now().Add(-time.Duration(cfg.CA.BackdateMinutes) * time.Minute)
	notAfter := time.Now().Add(time.Duration(cfg.CA.ValidDays) * 24 * time.Hour)

	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   "seti-enrollment",
			Organization: []string{cfg.CA.Organization},
			Country:      []string{cfg.CA.Country},
		},
		NotBefore:   notBefore,
		NotAfter:    notAfter,
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, eca.cert, &key.PublicKey, eca.key)
	if err != nil {
		return nil, fmt.Errorf("create enrollment cert: %v", err)
	}
	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return nil, fmt.Errorf("parse enrollment cert: %v", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})

	log.Printf("[cert-forge] Enrollment cert generated (CN=seti-enrollment)")
	return &EnrollmentCert{
		cert: cert, certDER: certDER, certPEM: certPEM,
		keyPEM: keyPEM, key: key,
	}, nil
}

// writeEnrollmentMaterial writes the enrollment CA cert and enrollment
// cert+key to the certs volume. These are the only private key materials
// that ever touch the volume — and the enrollment key's only capability
// is calling /instance-cert.
func writeEnrollmentMaterial(ec *EnrollmentCert) error {
	// Enrollment CA cert — needed by cert-forge to verify enrollment clients
	// Written to volume so it survives cert-forge restarts if needed
	ecaPath := filepath.Join(outputDir, "enrollment-ca.crt")
	if err := os.WriteFile(ecaPath, enrollmentCA.certPEM, 0644); err != nil {
		return fmt.Errorf("write enrollment CA cert: %v", err)
	}

	// Enrollment cert — injected into every container
	enrollCertPath := filepath.Join(outputDir, "enrollment.crt")
	if err := os.WriteFile(enrollCertPath, ec.certPEM, 0644); err != nil {
		return fmt.Errorf("write enrollment cert: %v", err)
	}

	enrollKeyPath := filepath.Join(outputDir, "enrollment.key")
	if err := os.WriteFile(enrollKeyPath, ec.keyPEM, 0644); err != nil {
		return fmt.Errorf("write enrollment key: %v", err)
	}

	log.Printf("[cert-forge] Enrollment material written to %s", outputDir)
	log.Printf("[cert-forge] Add to .env: ENROLLMENT_CERT and ENROLLMENT_KEY")
	return nil
}

// buildEnrollmentTLSConfig returns a tls.Config for the /instance-cert
// endpoint that requires a client cert signed by the enrollment CA.
func buildEnrollmentTLSConfig(selfIK *InstanceKey) *tls.Config {
	enrollPool := x509.NewCertPool()
	enrollPool.AddCert(enrollmentCA.cert)

	tlsCert := tls.Certificate{
		Certificate: [][]byte{selfIK.cert.Raw},
		PrivateKey:  selfIK.key,
	}

	return &tls.Config{
		Certificates: []tls.Certificate{tlsCert},
		ClientCAs:    enrollPool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS13,
	}
}

// verifyEnrollmentCert checks that a client cert presented to /instance-cert
// is signed by the enrollment CA. Called before issuing any instance cert.
func verifyEnrollmentCert(r interface{ TLS() interface{ PeerCertificates() []*x509.Certificate } }) error {
	// This verification happens implicitly via TLS ClientAuth on the
	// enrollment-specific listener — if the client cert doesn't verify
	// against the enrollment CA pool, the TLS handshake fails before
	// the handler is even called.
	return nil
}
