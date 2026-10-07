package cmd

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	adkAgent "github.com/colinrgodsey/wackypub/pkg/agent"
)

// serveTokenEnv is the shared bearer secret for TCP serving. The protocol is
// unauthenticated at the gRPC layer (stdio never needed auth - the pipe is the
// boundary), so a TCP listener that is reachable by anything other than
// loopback REQUIRES this token. Loopback bindings may leave it unset and stay
// open to local processes - state the policy, don't surprise.
const serveTokenEnv = "WACKYPUB_SERVE_TOKEN"

// isLoopback reports whether addr binds a loopback interface (127.0.0.0/8 or ::1).
func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	host = strings.Trim(host, "[]")
	ip := net.ParseIP(host)
	if ip != nil {
		return ip.IsLoopback()
	}
	// Empty host (":8423") binds all interfaces - not loopback.
	return false
}

// tokenInterceptor enforces the bearer token for every RPC when one is set.
// When no token is configured (loopback default), no check is applied.
func tokenInterceptor(token string) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if err := checkToken(ctx, token); err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}

// tokenStreamInterceptor enforces the bearer token for streaming RPCs.
func tokenStreamInterceptor(token string) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if err := checkToken(ss.Context(), token); err != nil {
			return err
		}
		return handler(srv, ss)
	}
}

func checkToken(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return status.Error(codes.Unauthenticated, "missing bearer token")
	}
	vals := md.Get("authorization")
	for _, v := range vals {
		if v == "Bearer "+token {
			return nil
		}
	}
	return status.Error(codes.Unauthenticated, "invalid bearer token")
}

// tcpServeCmd serves the AgentService protocol over a TCP listener (the
// network face of stdio-serve). The same RoutingServer registration carries
// over unchanged: multi-agent resolution (native vs bridged per agent) is the
// serve-side shape, identical to stdio-serve.
//
// This is the daemon deployment shape: systemd user unit, scripts, or remote
// clients sharing one runtime process. Session/lock semantics carry over from
// the per-process model: each agent turn takes the cross-process session flock
// (D117) under the SDK, so concurrent clients serialize per agent exactly as
// concurrent local processes do. Multiple clients sharing one server get the
// same guarantees as multiple processes sharing one workspace - documented,
// not invented here.
var tcpServeCmd = &cobra.Command{
	Use:   "tcp-serve [--listen :8423]",
	Short: "Serve the AgentService protocol over TCP (daemon lifecycle, network clients)",
	Long: `Serves the AgentService protocol over a TCP listener with the daemon lifecycle.

Start -\u003e serve -\u003e SIGTERM drains in-flight turns -\u003e exit. The workspace is
resolved from the current directory the same way every other wackypub command
resolves it (walk up for WACKYPUB_ROOT), so run this from the workspace root.

Auth: bind loopback by default and remain open to local processes. Binding a
non-loopback address REQUIRES WACKYPUB_SERVE_TOKEN; every RPC then carries
a Bearer token. For remote access prefer an SSH tunnel (ssh -L) so the bearer
token stays a fallback, not the perimeter.\n
TLS: provide --tls-cert and --tls-key (a real cert pair) for an encrypted
listener, or --tls-self-signed to generate an ECDSA P-256 certificate at
startup (SANs bound to the listen address; the sha256 fingerprint is logged
for client pinning, and --tls-cert-out writes the PEM to a file). TLS and the
bearer token are ORTHOGONAL: TLS encrypts the transport, the token authorizes
the caller - a TLS listener still requires the token exactly like a plaintext
one. A gRPC client dialing a self-signed endpoint must present the generated
cert (or InsecureSkipVerify, not recommended); the ssh-tunnel pattern keeps
the transport local and can stay plaintext. Default is plaintext with no TLS
flags.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		wsDir, err := GetWorkspaceDir()
		if err != nil {
			return fmt.Errorf("resolve workspace: %w", err)
		}

		token := os.Getenv(serveTokenEnv)
		if !isLoopback(listenAddr) && token == "" {
			return fmt.Errorf("refusing to bind non-loopback %s without %s: remote serving requires a bearer token", listenAddr, serveTokenEnv)
		}

		sdk := adkAgent.NewSDK(wsDir)
		sdk.MaxToolTurns = GetMaxToolTurns()
		sdk.CommandTimeoutSeconds = GetCommandTimeoutSeconds()

		tlsConf, err := tlsConfigForFlags()
		if err != nil {
			return err
		}
		var serverOpts []grpc.ServerOption
		if tlsConf != nil {
			serverOpts = append(serverOpts, grpc.Creds(credentials.NewTLS(tlsConf)))
		}
		router := adkAgent.NewRoutingServer(sdk)
		grpcServer := grpc.NewServer(append(serverOpts,
			router.UnknownServiceHandler(),
			grpc.ChainUnaryInterceptor(tokenInterceptor(token)),
			grpc.ChainStreamInterceptor(tokenStreamInterceptor(token)),
		)...)

		lis, err := net.Listen("tcp", listenAddr)
		if err != nil {
			return fmt.Errorf("listen %s: %w", listenAddr, err)
		}

		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()

		if tlsConf != nil {
			fmt.Fprintf(os.Stderr, "wackypub tcp-serve: listening on %s (workspace %s) with TLS (token auth still required for every RPC)\n", lis.Addr(), wsDir)
		} else {
			fmt.Fprintf(os.Stderr, "wackypub tcp-serve: listening on %s (workspace %s) plaintext\n", lis.Addr(), wsDir)
		}

		serveErr := make(chan error, 1)
		go func() {
			serveErr <- grpcServer.Serve(lis)
		}()

		select {
		case <-ctx.Done():
			fmt.Fprintln(os.Stderr, "wackypub tcp-serve: SIGTERM received, draining in-flight turns")
			// Close the listener FIRST so Serve unblocks (GracefulStop alone does
			// not unblock Accept on a single-conn listener), then let in-flight
			// turns finish or cancel per the existing cancellation semantics.
			_ = lis.Close()
			grpcServer.GracefulStop()
			fmt.Fprintln(os.Stderr, "wackypub tcp-serve: drained, exiting")
			return nil
		case err := <-serveErr:
			return err
		}
	},
}

var (
	listenAddr    string
	tlsCertPath   string
	tlsKeyPath    string
	tlsSelfSigned bool
	// tlsCertOut, when non-empty with tlsSelfSigned, writes the generated self-signed
	// certificate (PEM) to this path so a client can pin it. The fingerprint is always
	// logged at startup regardless.
	tlsCertOut string
)

func init() {
	tcpServeCmd.Flags().StringVar(&listenAddr, "listen", "127.0.0.1:8423", "TCP listen address (loopback by default; non-loopback requires "+serveTokenEnv+")")
	tcpServeCmd.Flags().StringVar(&tlsCertPath, "tls-cert", "", "path to the TLS certificate (PEM); requires --tls-key")
	tcpServeCmd.Flags().StringVar(&tlsKeyPath, "tls-key", "", "path to the TLS private key (PEM); requires --tls-cert")
	tcpServeCmd.Flags().BoolVar(&tlsSelfSigned, "tls-self-signed", false, "generate an ECDSA P-256 self-signed certificate at startup bound to the listen addresses")
	tcpServeCmd.Flags().StringVar(&tlsCertOut, "tls-cert-out", "", "with --tls-self-signed, write the generated certificate (PEM) here for client pinning")
}

// tlsConfigForFlags builds the grpc TLS credentials for the server, or nil for
// plaintext. TLS and the bearer token are orthogonal layers: TLS encrypts the
// transport, the token authorizes the caller - a TLS listener STILL requires
// the token (same as plaintext). Providing kess than a full cert+key pair is a
// startup error, not a silent fallback to plaintext.
func tlsConfigForFlags() (*tls.Config, error) {
	provided := tlsCertPath != "" || tlsKeyPath != ""
	selfSigned := tlsSelfSigned
	if provided && selfSigned {
		return nil, fmt.Errorf("--tls-cert/--tls-key and --tls-self-signed are mutually exclusive: provide one or the other")
	}
	if (tlsCertPath == "") != (tlsKeyPath == "") {
		return nil, fmt.Errorf("--tls-cert and --tls-key must be provided together")
	}
	if !provided && !selfSigned {
		return nil, nil // plaintext default unchanged
	}

	var cert tls.Certificate
	if provided {
		var err error
		cert, err = tls.LoadX509KeyPair(tlsCertPath, tlsKeyPath)
		if err != nil {
			return nil, fmt.Errorf("load TLS cert/key: %w", err)
		}
	} else {
		var err error
		cert, err = generateSelfSignedCert(listenAddr, tlsCertOut)
		if err != nil {
			return nil, err
		}
	}

	return &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}, nil
}

// generateSelfSignedCert creates an ECDSA P-256 self-signed certificate valid for
// one year, with SANs bound to the listen address hostname + IP, writes the PEM to
// writePath when non-empty, and logs the SHA-256 fingerprint for client pinning.
func generateSelfSignedCert(listenAddr, writePath string) (tls.Certificate, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("generate ECDSA key: %w", err)
	}

	host := "127.0.0.1"
	if h, _, err := net.SplitHostPort(listenAddr); err == nil && h != "" {
		host = strings.Trim(h, "[]")
	}

	var ips []net.IP
	if ip := net.ParseIP(host); ip != nil {
		ips = append(ips, ip)
	}
	if ip := net.ParseIP("127.0.0.1"); ip != nil {
		ips = append(ips, ip)
	}
	template := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "wackypub tcp-serve self-signed"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IPAddresses:           ips,
		DNSNames:              []string{"localhost", host},
	}

	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("create self-signed cert: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("marshal ECDSA key: %w", err)
	}

	if writePath != "" {
		var joined []byte
		joined = append(joined, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
		if err := os.WriteFile(writePath, joined, 0644); err != nil {
			return tls.Certificate{}, fmt.Errorf("write self-signed cert to %s: %w", writePath, err)
		}
		fmt.Fprintf(os.Stderr, "wackypub tcp-serve: self-signed cert written to %s for client pinning\n", writePath)
	}

	cert, err := tls.X509KeyPair(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("assemble tls.Certificate: %w", err)
	}

	if parsed, err := x509.ParseCertificate(der); err == nil {
		fingerprint := sha256Sum(parsed.Raw)
		fmt.Fprintf(os.Stderr, "wackypub tcp-serve: self-signed TLS fingerprint sha256:%s (pin this in the client)\n", fingerprint)
	}
	return cert, nil
}

func sha256Sum(b []byte) string {
	sum := sha256.Sum256(b)
	return fmt.Sprintf("%x", sum)
}
