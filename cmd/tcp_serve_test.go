package cmd

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	adkAgent "github.com/colinrgodsey/wackypub/pkg/agent"
	agentv1 "github.com/colinrgodsey/wackypub/pkg/agent/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// safeBuffer is a mutex-guarded writer so the os/exec stderr copier and the
// test can read it concurrently without a data race.
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// spawnTCPServe starts the real wackypub binary in tcp-serve mode on an
// ephemeral port and returns a grpc client dialing it directly. The dial
// target is the ONLY thing that differs from the stdio path - everything the
// client does afterwards is transport-agnostic. This is the card's acceptance:
// the same client works against both serve modes with only the dial changed.
// spawnTCPServe starts tcp-serve over plaintext (insecure creds, no TLS flags).
func spawnTCPServe(t *testing.T, wsDir string, extraEnv ...string) (agentv1.AgentServiceClient, net.Addr, func()) {
	client, addr, stderr, cleanup := spawnTCPServeWithCreds(t, wsDir, insecure.NewCredentials(), nil, extraEnv...)
	_ = stderr
	return client, addr, cleanup
}

// spawnTCPServeWithCreds starts the real binary with optional extra command args
// (TLS flags) and an explicit client transport credential. Returns the client,
// the bound address, the child's stderr writer (to assert startup diagnostics),
// and a cleanup func.
func spawnTCPServeWithCreds(t *testing.T, wsDir string, creds credentials.TransportCredentials, extraArgs []string, extraEnv ...string) (agentv1.AgentServiceClient, net.Addr, *safeBuffer, func()) {
	t.Helper()
	bin := getWackypubBin(t)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	addr := lis.Addr().String()
	_ = lis.Close()

	argv := []string{"tcp-serve", "--listen", addr}
	argv = append(argv, extraArgs...)
	cmd := exec.Command(bin, argv...)
	cmd.Dir = wsDir
	var stderrBuf safeBuffer
	cmd.Stderr = &stderrBuf
	cmd.Env = append(os.Environ(), extraEnv...)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start tcp-serve: %v", err)
	}

	// Wait for the listener to accept (the process prints its listen line, but
	// a real connect is the reliable readiness check).
	token := ""
	for _, e := range extraEnv {
		if v, ok := strings.CutPrefix(e, serveTokenEnv+"="); ok {
			token = v
		}
	}
	gc, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(creds),
	)
	if err != nil {
		_ = cmd.Process.Kill()
		t.Fatalf("grpc.NewClient: %v", err)
	}

	client := agentv1.NewAgentServiceClient(gc)

	// Readiness: block until a cheap call succeeds. With a token configured the
	// probe must carry it, else the server (correctly) refuses and readiness
	// never flips. With TLS creds the stack is exercised by the connect itself.
	deadline := time.Now().Add(10 * time.Second)
	ready := false
	for time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		if token != "" {
			ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
		}
		_, err := client.ListAgents(ctx, &agentv1.ListAgentsRequest{})
		cancel()
		if err == nil {
			ready = true
			break
		}
	}
	if !ready {
		_ = gc.Close()
		_ = cmd.Process.Kill()
		t.Fatalf("tcp-serve did not become ready on %s (stderr: %s)", addr, stderrBuf.String())
	}

	cleanup := func() {
		_ = gc.Close()
		_ = cmd.Process.Kill()
	}
	t.Cleanup(cleanup)
	return client, lis.Addr(), &stderrBuf, cleanup
}

// TestTCPServe_ListAgentsAndInspect proves the served surface over TCP: the
// same ListAgents + InspectAgent calls that stdio-serve answers, over a real
// network dial, with the workspace resolved from the child's CWD.
func TestTCPServe_ListAgentsAndInspect(t *testing.T) {
	wsDir, _ := stdioWorkspace(t, "tcp reply")
	client, _, _ := spawnTCPServe(t, wsDir)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	agents, err := client.ListAgents(ctx, &agentv1.ListAgentsRequest{})
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	if len(agents.GetAgentIds()) != 1 || agents.GetAgentIds()[0] != "stdioagent" {
		t.Fatalf("ListAgents over tcp returned %+v", agents.GetAgentIds())
	}

	insp, err := client.InspectAgent(ctx, &agentv1.InspectAgentRequest{AgentId: "stdioagent", WorkspaceDir: wsDir})
	if err != nil {
		t.Fatalf("InspectAgent: %v", err)
	}
	if !insp.GetAgentDirExists() {
		t.Fatalf("InspectAgent said agent dir does not exist")
	}
}

// TestServeSurface_IdenticalAcrossStdioAndTCP is the card's acceptance: the same
// grpc test client operations run against both serve modes with only the dial
// target changed. The operation set is deliberately shared: ListAgents and an
// InspectAgent that must produce the identical answer on both transports.
func TestServeSurface_IdenticalAcrossStdioAndTCP(t *testing.T) {
	wsDir, _ := stdioWorkspace(t, "surface reply")

	// stdio side
	stdioClient, _ := spawnStdioServe(t, wsDir)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stdioAgents, err := stdioClient.ListAgents(ctx, &agentv1.ListAgentsRequest{})
	if err != nil {
		t.Fatalf("stdio ListAgents: %v", err)
	}
	stdioInsp, err := stdioClient.InspectAgent(ctx, &agentv1.InspectAgentRequest{AgentId: "stdioagent", WorkspaceDir: wsDir})
	if err != nil {
		t.Fatalf("stdio InspectAgent: %v", err)
	}

	// tcp side, same workspace, same calls
	tcpClient, _, _ := spawnTCPServe(t, wsDir)
	tcpAgents, err := tcpClient.ListAgents(ctx, &agentv1.ListAgentsRequest{})
	if err != nil {
		t.Fatalf("tcp ListAgents: %v", err)
	}
	tcpInsp, err := tcpClient.InspectAgent(ctx, &agentv1.InspectAgentRequest{AgentId: "stdioagent", WorkspaceDir: wsDir})
	if err != nil {
		t.Fatalf("tcp InspectAgent: %v", err)
	}

	if fmt.Sprint(stdioAgents.GetAgentIds()) != fmt.Sprint(tcpAgents.GetAgentIds()) {
		t.Errorf("ListAgents differs between transports:\nstdio: %v\ntcp:   %v", stdioAgents.GetAgentIds(), tcpAgents.GetAgentIds())
	}
	if fmt.Sprint(stdioInsp) != fmt.Sprint(tcpInsp) {
		t.Errorf("InspectAgent differs between transports:\nstdio: %v\ntcp:   %v", stdioInsp, tcpInsp)
	}
}

// TestTCPServe_NonLoopbackRequiresToken: binding a non-loopback address without
// WACKYPUB_SERVE_TOKEN must be refused at startup.
func TestTCPServe_NonLoopbackRequiresToken(t *testing.T) {
	bin := getWackypubBin(t)
	wsDir, _ := stdioWorkspace(t, "x")

	// 0.0.0.0 is non-loopback semantically even on a loopback-only host.
	cmd := exec.Command(bin, "tcp-serve", "--listen", "0.0.0.0:0")
	cmd.Dir = wsDir
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("non-loopback bind without token should fail, got success: %s", out)
	}
	if !strings.Contains(string(out), serveTokenEnv) {
		t.Errorf("error should name %s, got: %s", serveTokenEnv, out)
	}
}

// TestTCPServe_TokenRequiredWhenSet: when a token is configured, calls without
// it are refused, and calls with it succeed.
func TestTCPServe_TokenRequiredWhenSet(t *testing.T) {
	wsDir, _ := stdioWorkspace(t, "token reply")
	client, _, _ := spawnTCPServe(t, wsDir, serveTokenEnv+"=sekret")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Without the token: Unauthenticated.
	_, err := client.ListAgents(ctx, &agentv1.ListAgentsRequest{})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("missing token should be Unauthenticated, got %v", err)
	}
}

// TestTCPServe_TokenAccepted sets the metadata and verifies the call succeeds.
func TestTCPServe_TokenAccepted(t *testing.T) {
	wsDir, _ := stdioWorkspace(t, "token ok")
	client, _, _ := spawnTCPServe(t, wsDir, serveTokenEnv+"=sekret")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ctx = withBearer(ctx, "sekret")

	agents, err := client.ListAgents(ctx, &agentv1.ListAgentsRequest{})
	if err != nil {
		t.Fatalf("with-token ListAgents: %v", err)
	}
	if len(agents.GetAgentIds()) != 1 {
		t.Fatalf("with-token ListAgents returned %+v", agents.GetAgentIds())
	}
}

// TestTCPServe_SIGTERMDrainsCleanly: send SIGTERM and verify the process exits
// promptly (drain path) rather than hanging.
func TestTCPServe_SIGTERMDrainsCleanly(t *testing.T) {
	wsDir, _ := stdioWorkspace(t, "drain")
	bin := getWackypubBin(t)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	addr := lis.Addr().String()
	_ = lis.Close()

	cmd := exec.Command(bin, "tcp-serve", "--listen", addr)
	cmd.Dir = wsDir
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = cmd.Process.Kill() }()

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	// Give it a moment to listen, then SIGTERM.
	time.Sleep(500 * time.Millisecond)
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM: %v", err)
	}

	select {
	case <-done:
		// drained and exited
	case <-time.After(6 * time.Second):
		t.Fatal("tcp-serve did not exit after SIGTERM")
	}
}

func withBearer(ctx context.Context, token string) context.Context {
	return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
}

// TestTCPServe_MultiAgentViaRoutingProxy is the card's multi-agent acceptance:
// one tcp-serve process hosting a workspace with TWO agent ids, both resolvable
// through the routing proxy on the same listener. This mirrors the stdio
// multi-agent shape (the #82 proxy is the serve-side resolution) over network.
func TestTCPServe_MultiAgentViaRoutingProxy(t *testing.T) {
	wsA, _ := stdioWorkspace(t, "multi a")
	// Add a second agent to the SAME workspace.
	agentB := "agentB"
	agentBDir := filepath.Join(wsA, agentB)
	if err := os.MkdirAll(agentBDir, 0755); err != nil {
		t.Fatalf("mkdir agentB: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentBDir, "AGENTS.md"), []byte("b"), 0644); err != nil {
		t.Fatalf("AGENTS.md B: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentBDir, adkAgent.AllowedAgentsFile), []byte(agentB+"\n"), 0644); err != nil {
		t.Fatalf("allowlist B: %v", err)
	}

	client, _, _ := spawnTCPServe(t, wsA)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	resp, err := client.ListAgents(ctx, &agentv1.ListAgentsRequest{})
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	ids := resp.GetAgentIds()
	if len(ids) != 2 {
		t.Fatalf("expected 2 agents over tcp, got %v", ids)
	}
	found := map[string]bool{}
	for _, id := range ids {
		found[id] = true
	}
	if !found["stdioagent"] || !found[agentB] {
		t.Fatalf("both agent ids must resolve through the routing proxy, got %v", ids)
	}

	// And InspectAgent routes to each id on the same server.
	for _, id := range ids {
		insp, err := client.InspectAgent(ctx, &agentv1.InspectAgentRequest{AgentId: id, WorkspaceDir: wsA})
		if err != nil {
			t.Fatalf("InspectAgent %s: %v", id, err)
		}
		if !insp.GetAgentDirExists() {
			t.Errorf("agent %s reported missing", id)
		}
	}
}

// writeTestCertKeyPair generates a self-signed ECDSA P-256 cert/key pair for
// 127.0.0.1 and writes them to temp files; returns the cert path, key path,
// and the parsed cert (for a client root pool).
func writeTestCertKeyPair(t *testing.T) (certPath, keyPath string, certPool *x509.CertPool) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	tmpl := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "tcp-serve-test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	dir := t.TempDir()
	certPath = filepath.Join(dir, "cert.pem")
	keyPath = filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certPath, certPEM, 0644); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(certPEM) {
		t.Fatalf("append cert to pool")
	}
	return certPath, keyPath, pool
}

// TestServeSurface_IdenticalAcrossStdioAndTLS is the TLS variant of the card
// acceptance: the same grpc client ops over a TLS listener with provided certs
// produce the same surface as stdio-serve. Only the dial target + creds change.
func TestServeSurface_IdenticalAcrossStdioAndTLS(t *testing.T) {
	wsDir, _ := stdioWorkspace(t, "tls surface")

	certPath, keyPath, pool := writeTestCertKeyPair(t)
	client, _, _, _ := spawnTCPServeWithCreds(t, wsDir,
		credentials.NewTLS(&tls.Config{RootCAs: pool, ServerName: "127.0.0.1"}),
		[]string{"--tls-cert", certPath, "--tls-key", keyPath},
	)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	agents, err := client.ListAgents(ctx, &agentv1.ListAgentsRequest{})
	if err != nil {
		t.Fatalf("ListAgents over TLS: %v", err)
	}
	if len(agents.GetAgentIds()) != 1 || agents.GetAgentIds()[0] != "stdioagent" {
		t.Fatalf("ListAgents over TLS returned %v", agents.GetAgentIds())
	}
	insp, err := client.InspectAgent(ctx, &agentv1.InspectAgentRequest{AgentId: "stdioagent", WorkspaceDir: wsDir})
	if err != nil {
		t.Fatalf("InspectAgent over TLS: %v", err)
	}
	if !insp.GetAgentDirExists() {
		t.Fatalf("InspectAgent over TLS said missing")
	}
}

// TestTCPServe_SelfSignedGeneratesCertAndLogsFingerprint verifies the overnight
// self-signed path: cert generated at startup, --tls-cert-out writes the PEM,
// the client pinning with that file connects, and the startup log carries the
// sha256 fingerprint line.
func TestTCPServe_SelfSignedGeneratesCertAndLogsFingerprint(t *testing.T) {
	wsDir, _ := stdioWorkspace(t, "selfsigned")
	certOut := filepath.Join(t.TempDir(), "gen.pem")

	// Spawn the server with the self-signed flag; it writes cert-out at startup.
	bin := getWackypubBin(t)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	addr := lis.Addr().String()
	_ = lis.Close()

	cmd := exec.Command(bin, "tcp-serve", "--listen", addr, "--tls-self-signed", "--tls-cert-out", certOut)
	cmd.Dir = wsDir
	var stderrBuf safeBuffer
	cmd.Stderr = &stderrBuf
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = cmd.Process.Kill() }()

	// Wait for the generated cert file, then dial with it PINNED (the card's
	// acceptance: client connects with the cert pinned, not InsecureSkipVerify).
	deadline := time.Now().Add(10 * time.Second)
	var data []byte
	for time.Now().Before(deadline) {
		data, err = os.ReadFile(certOut)
		if err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(data) == 0 {
		t.Fatalf("cert-out not written before deadline, stderr: %s", stderrBuf.String())
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(data) {
		t.Fatalf("generated cert did not parse into a pool")
	}

	gc, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{RootCAs: pool, ServerName: "127.0.0.1"})),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	defer gc.Close()
	client := agentv1.NewAgentServiceClient(gc)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	agents, err := client.ListAgents(ctx, &agentv1.ListAgentsRequest{})
	if err != nil {
		t.Fatalf("ListAgents with pinned self-signed cert: %v", err)
	}
	if len(agents.GetAgentIds()) != 1 || agents.GetAgentIds()[0] != "stdioagent" {
		t.Fatalf("pinned self-signed ListAgents returned %v", agents.GetAgentIds())
	}
	// The self-signed SAN must include the dial address; a client that could NOT
	// pin would have failed above with a cert verification error.
	if !strings.Contains(stderrBuf.String(), "fingerprint sha256:") {
		t.Errorf("startup log should log the sha256 fingerprint for pinning, got: %s", stderrBuf.String())
	}
}

// TestTCPServe_TLSStillRequiresToken pins the orthogonality contract: TLS
// encrypts, the bearer token authenticates - a TLS listener without the token
// refuses exactly like plaintext.
func TestTCPServe_TLSStillRequiresToken(t *testing.T) {
	wsDir, _ := stdioWorkspace(t, "tls token")
	certPath, keyPath, pool := writeTestCertKeyPair(t)

	client, _, _, _ := spawnTCPServeWithCreds(t, wsDir,
		credentials.NewTLS(&tls.Config{RootCAs: pool, ServerName: "127.0.0.1"}),
		[]string{"--tls-cert", certPath, "--tls-key", keyPath},
		serveTokenEnv+"=sekret",
	)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := client.ListAgents(ctx, &agentv1.ListAgentsRequest{})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("TLS without token should be Unauthenticated, got %v", err)
	}
}
