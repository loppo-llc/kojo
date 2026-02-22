package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/loppo-llc/kojo/internal/server"
	"github.com/loppo-llc/kojo/web"
	"tailscale.com/tsnet"
)

var version = "0.1.0"

func main() {
	port := flag.Int("port", 8080, "port number (auto-increments if busy)")
	dev := flag.Bool("dev", false, "enable dev mode (proxy to Vite)")
	local := flag.Bool("local", false, "listen on localhost only (no Tailscale)")
	showVersion := flag.Bool("version", false, "show version")
	flag.Parse()

	if *showVersion {
		fmt.Println("kojo", version)
		return
	}

	logLevel := slog.LevelInfo
	if *dev {
		logLevel = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: logLevel,
	}))

	// embed static files (sub to strip "dist/" prefix)
	var staticFS fs.FS
	if !*dev {
		var err error
		staticFS, err = fs.Sub(web.StaticFiles, "dist")
		if err != nil {
			logger.Error("failed to load embedded static files", "err", err)
			os.Exit(1)
		}
	}

	srv := server.New(server.Config{
		Addr:     fmt.Sprintf(":%d", *port),
		DevMode:  *dev,
		Logger:   logger,
		StaticFS: staticFS,
		Version:  version,
	})

	// graceful shutdown
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if *local || *dev {
		// local mode: listen on localhost with port fallback
		ln, err := listenWithFallback("127.0.0.1", *port, 10, logger)
		if err != nil {
			logger.Error("failed to listen", "err", err)
			os.Exit(1)
		}
		actualAddr := ln.Addr().String()
		fmt.Fprintf(os.Stderr, "\n  kojo v%s running at:\n\n    http://%s\n\n", version, actualAddr)
		go func() {
			if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
				logger.Error("server error", "err", err)
				os.Exit(1)
			}
		}()
	} else {
		// tailscale mode: listen via tsnet with HTTPS
		tsServer := &tsnet.Server{
			Hostname: "kojo",
			Logf:     func(format string, args ...any) { logger.Debug(fmt.Sprintf(format, args...)) },
		}

		ln, err := tsServer.ListenTLS("tcp", fmt.Sprintf(":%d", *port))
		if err != nil {
			logger.Error("failed to listen on tailscale", "err", err)
			os.Exit(1)
		}

		// get tailscale addresses for display
		fmt.Fprintf(os.Stderr, "\n  kojo v%s running at:\n\n", version)
		lc, _ := tsServer.LocalClient()
		if lc != nil {
			if status, err := lc.Status(ctx); err == nil {
				// print DNS name (e.g. kojo.<tailnet>.ts.net)
				if status.Self != nil {
					dnsName := strings.TrimSuffix(status.Self.DNSName, ".")
					if dnsName != "" {
						if *port == 443 {
							fmt.Fprintf(os.Stderr, "    https://%s\n", dnsName)
						} else {
							fmt.Fprintf(os.Stderr, "    https://%s:%d\n", dnsName, *port)
						}
					}
				}
				// print IP addresses
				for _, ip := range status.TailscaleIPs {
					fmt.Fprintf(os.Stderr, "    https://%s:%d\n", ip, *port)
				}
			} else {
				logger.Warn("could not get tailscale status", "err", err)
				fmt.Fprintf(os.Stderr, "    https://kojo:<tailnet>.ts.net:%d  (getting status...)\n", *port)
			}
		}
		fmt.Fprintln(os.Stderr)

		// tsnet.ListenTLS returns a tls.Listener, serve directly
		go func() {
			// ServeTLS with empty cert/key since TLS is already handled by the listener
			srv.SetTLSConfig(&tls.Config{})
			if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
				logger.Error("server error", "err", err)
				os.Exit(1)
			}
		}()

		defer tsServer.Close()
	}

	<-ctx.Done()
	logger.Info("received shutdown signal")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("shutdown error", "err", err)
	}
}

func listenWithFallback(host string, startPort, maxAttempts int, logger *slog.Logger) (net.Listener, error) {
	for i := range maxAttempts {
		port := startPort + i
		addr := net.JoinHostPort(host, strconv.Itoa(port))
		ln, err := net.Listen("tcp", addr)
		if err == nil {
			if i > 0 {
				logger.Info("port was busy, using fallback", "requested", startPort, "actual", port)
			}
			return ln, nil
		}
		if !strings.Contains(err.Error(), "address already in use") {
			return nil, err
		}
	}
	return nil, fmt.Errorf("all ports %d-%d are in use", startPort, startPort+maxAttempts-1)
}
