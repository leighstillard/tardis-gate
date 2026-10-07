package ship

import (
	"crypto/tls"
	"log/slog"
	"os"

	"go.temporal.io/sdk/client"
	tlog "go.temporal.io/sdk/log"
)

// Dial connects to Temporal using the same environment variables as the
// temporal CLI. With TEMPORAL_API_KEY set it uses TLS and the key (Temporal
// Cloud); without it, plaintext to a local dev server.
func Dial() (client.Client, error) {
	opts := client.Options{
		HostPort:  envOr("TEMPORAL_ADDRESS", "localhost:7233"),
		Namespace: envOr("TEMPORAL_NAMESPACE", "default"),
		Logger:    tlog.NewStructuredLogger(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))),
	}
	if key := os.Getenv("TEMPORAL_API_KEY"); key != "" {
		opts.Credentials = client.NewAPIKeyStaticCredentials(key)
		opts.ConnectionOptions.TLS = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	return client.Dial(opts)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
