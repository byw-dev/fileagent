//go:build integration

package grpcclient

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/byw-dev/fileagent/agent/internal/config"
	agentv1 "github.com/byw-dev/fileagent/api/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

type tlsTestServer struct {
	agentv1.UnimplementedAgentServiceServer
}

func (s *tlsTestServer) Connect(stream agentv1.AgentService_ConnectServer) error {
	<-stream.Context().Done()
	return nil
}

func startTLSServer(t *testing.T, cert tls.Certificate) (string, func()) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	grpcSrv := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{cert},
	})))
	agentv1.RegisterAgentServiceServer(grpcSrv, &tlsTestServer{})

	go func() { _ = grpcSrv.Serve(lis) }()
	return lis.Addr().String(), func() {
		grpcSrv.Stop()
		_ = lis.Close()
	}
}

func writeCertFile(t *testing.T, dir, name string, der []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600))
	return path
}

func generateCACertAndServerCert(t *testing.T) (caDER []byte, serverCert tls.Certificate) {
	t.Helper()

	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "fileagent-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	caDER, err = x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	require.NoError(t, err)

	serverKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	serverTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	serverDER, err := x509.CreateCertificate(rand.Reader, serverTmpl, caTmpl, &serverKey.PublicKey, caKey)
	require.NoError(t, err)

	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(serverKey)})
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverDER})
	serverCert, err = tls.X509KeyPair(certPEM, keyPEM)
	require.NoError(t, err)
	return caDER, serverCert
}

func TestTLSIntegration_CustomCAConnects(t *testing.T) {
	caDER, serverCert := generateCACertAndServerCert(t)
	addr, cleanup := startTLSServer(t, serverCert)
	defer cleanup()

	caPath := writeCertFile(t, t.TempDir(), "ca.pem", caDER)
	cfg := &config.Config{
		Server: config.ServerConfig{Endpoint: addr, TLSCACert: caPath},
	}
	client := New(cfg, zap.NewNop())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, client.Connect(ctx))
	defer func() { _ = client.Close() }()

	require.Eventually(t, func() bool {
		client.mu.Lock()
		defer client.mu.Unlock()
		return client.stream != nil
	}, 3*time.Second, 50*time.Millisecond)
}

func TestTLSIntegration_SystemCAConnects(t *testing.T) {
	caDER, serverCert := generateCACertAndServerCert(t)
	addr, cleanup := startTLSServer(t, serverCert)
	defer cleanup()

	caPath := writeCertFile(t, t.TempDir(), "ca.pem", caDER)
	t.Setenv("SSL_CERT_FILE", caPath)

	cfg := &config.Config{
		Server: config.ServerConfig{Endpoint: addr},
	}
	client := New(cfg, zap.NewNop())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, client.Connect(ctx))
	defer func() { _ = client.Close() }()

	require.Eventually(t, func() bool {
		client.mu.Lock()
		defer client.mu.Unlock()
		return client.stream != nil
	}, 3*time.Second, 50*time.Millisecond)
}

func TestTLSIntegration_HandshakeFailureRetries(t *testing.T) {
	_, serverCert := generateCACertAndServerCert(t)
	addr, cleanup := startTLSServer(t, serverCert)
	defer cleanup()

	logCore, observed := observer.New(zap.WarnLevel)
	logger := zap.New(logCore)
	cfg := &config.Config{
		Server: config.ServerConfig{Endpoint: addr},
	}
	client := New(cfg, logger)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	require.NoError(t, client.Connect(ctx))
	defer func() { _ = client.Close() }()

	<-ctx.Done()

	assert.GreaterOrEqual(t, observed.FilterMessage("grpcclient: stream error").Len(), 2)
}
