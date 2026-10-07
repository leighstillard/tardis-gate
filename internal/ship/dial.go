package ship

import (
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"os"

	"go.temporal.io/sdk/client"
	tlog "go.temporal.io/sdk/log"
)

// Dial connects to Temporal using the same environment variables as the
// temporal CLI. With TEMPORAL_API_KEY set it uses TLS and the key (Temporal
// Cloud); without it, plaintext, which it allows only to this machine.
func Dial() (client.Client, error) {
	opts := client.Options{
		HostPort:  envOr("TEMPORAL_ADDRESS", "localhost:7233"),
		Namespace: envOr("TEMPORAL_NAMESPACE", "default"),
		Logger:    tlog.NewStructuredLogger(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))),
	}
	if key := os.Getenv("TEMPORAL_API_KEY"); key != "" {
		opts.Credentials = client.NewAPIKeyStaticCredentials(key)
		opts.ConnectionOptions.TLS = &tls.Config{MinVersion: tls.VersionTLS12}
	} else if !loopback(opts.HostPort) {
		return nil, fmt.Errorf("%s is not this machine, and without TEMPORAL_API_KEY the connection would be plaintext and unauthenticated; set TEMPORAL_API_KEY", opts.HostPort)
	}
	c, err := client.Dial(opts)
	if err != nil {
		return nil, fmt.Errorf("%s (set TEMPORAL_ADDRESS, TEMPORAL_NAMESPACE, TEMPORAL_API_KEY): %w", opts.HostPort, err)
	}
	return c, nil
}

// loopback reports whether hostPort names this machine.
func loopback(hostPort string) bool {
	host, _, err := net.SplitHostPort(hostPort)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return host == "localhost" || ip != nil && ip.IsLoopback()
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
