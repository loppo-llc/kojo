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
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/loppo-llc/kojo/internal/agent"
	"github.com/loppo-llc/kojo/internal/auth"
	"github.com/loppo-llc/kojo/internal/configdir"
	"github.com/loppo-llc/kojo/internal/notify"
	"github.com/loppo-llc/kojo/internal/server"
	"github.com/loppo-llc/kojo/internal/session"
	"github.com/loppo-llc/kojo/web"
	"tailscale.com/tsnet"
)

var version = "0.18.0"

func main() {
	port := flag.Int("port", 8080, "port number (auto-increments if busy)")
	dev := flag.Bool("dev", false, "enable dev mode (proxy to Vite)")
	local := flag.Bool("local", false, "listen on localhost only (no Tailscale)")
	configDir := flag.String("config-dir", "", "override config directory (default: ~/.config/kojo)")
	showVersion := flag.Bool("version", false, "show version")
	noAuth := flag.Bool("no-auth", false, "disable agent-facing auth listener (--local/--dev only)")
	flag.Parse()

	if *showVersion {
		fmt.Println("kojo", version)
		return
	}

	logLevel := slog.LevelInfo
	if *dev {
		logLevel = slog.LevelDebug
	}
	if lvl := os.Getenv("KOJO_LOG_LEVEL"); lvl != "" {
		switch strings.ToLower(lvl) {
		case "debug":
			logLevel = slog.LevelDebug
		case "info":
			logLevel = slog.LevelInfo
		case "warn", "warning":
			logLevel = slog.LevelWarn
		case "error":
			logLevel = slog.LevelError
		default:
			// Defer logging the invalid value until the logger exists, so
			// operators notice the misconfiguration instead of silently
			// getting the default level.
			fmt.Fprintf(os.Stderr, "kojo: ignoring invalid KOJO_LOG_LEVEL=%q (valid: debug|info|warn|warning|error)\n", lvl)
		}
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: logLevel,
	}))

	// Resolve the config directory before any subsystem reads it.
	if *configDir != "" {
		configdir.Set(*configDir)
	}
	resolvedDir := configdir.Path()
	logger.Info("config directory", "path", resolvedDir)

	// Acquire an exclusive advisory lock on the config dir so a second kojo
	// instance cannot attach to the same directory and clobber shared state
	// (agents.json, credentials.db, vapid.json).
	lock, err := configdir.Acquire(resolvedDir)
	if err != nil {
		logger.Error("could not lock config directory — another kojo instance may be running", "dir", resolvedDir, "err", err)
		fmt.Fprintf(os.Stderr, "\nAnother kojo instance is already using %s.\n", resolvedDir)
		fmt.Fprintf(os.Stderr, "Use --config-dir to point this instance at a different directory.\n\n")
		os.Exit(1)
	}
	defer lock.Release()

	// tmux is required for user tool sessions on Unix
	if runtime.GOOS != "windows" {
		if _, err := exec.LookPath("tmux"); err != nil {
			logger.Warn("tmux not found in PATH; user tool sessions (claude, codex, gemini) will not work")
		}
	}

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

	var notifyMgr *notify.Manager
	if nm, err := notify.NewManager(logger); err != nil {
		logger.Warn("web push notifications disabled", "err", err)
	} else {
		notifyMgr = nm
	}

	agentMgr := agent.NewManager(logger)
	groupDMMgr := agent.NewGroupDMManager(agentMgr, logger)
	agentMgr.SetGroupDMManager(groupDMMgr)

	// --no-auth is dev-only — refuse to bypass auth on the public Tailscale
	// listener where another user could reach the API directly.
	if *noAuth && !*local && !*dev {
		fmt.Fprintln(os.Stderr, "kojo: --no-auth requires --local or --dev")
		os.Exit(1)
	}

	// Token store. Owner token is loaded/created at <configdir>/auth/owner.token
	// unless KOJO_OWNER_TOKEN overrides it (handy for tests / scripted runs).
	authBase := filepath.Join(resolvedDir, "auth")
	tokens, err := auth.NewTokenStore(authBase, os.Getenv("KOJO_OWNER_TOKEN"))
	if err != nil {
		logger.Error("failed to initialize auth token store", "err", err)
		os.Exit(1)
	}
	agentMgr.SetTokenStore(tokens)
	agent.SetAgentTokenLookup(func(id string) (string, bool) {
		t, err := tokens.AgentToken(id)
		if err != nil {
			return "", false
		}
		return t, true
	})
	resolver := auth.NewResolver(tokens, agentMgr.IsPrivileged)

	srv := server.New(server.Config{
		Addr:           fmt.Sprintf(":%d", *port),
		DevMode:        *dev,
		Logger:         logger,
		StaticFS:       staticFS,
		Version:        version,
		NotifyManager:  notifyMgr,
		AgentManager:   agentMgr,
		GroupDMManager: groupDMMgr,
	})

	// graceful shutdown
	ctx, stop := signal.NotifyContext(context.Background(), session.ShutdownSignals()...)
	defer stop()

	if *local || *dev {
		ln, err := listenWithFallback("127.0.0.1", *port, 10, logger)
		if err != nil {
			logger.Error("failed to listen", "err", err)
			os.Exit(1)
		}
		actualAddr := ln.Addr().String()

		if *noAuth {
			// --no-auth (--local/--dev only): the loopback listener is
			// Owner-trusted. Suitable for hacking on the UI itself; not
			// suitable for running real agents because they can read
			// the full API as Owner. Group DM curl examples stay
			// pointed at this listener.
			fmt.Fprintf(os.Stderr, "\n  kojo v%s running at:\n\n    http://%s\n\n", version, actualAddr)
			groupDMMgr.SetAPIBase("http://" + actualAddr)
			logger.Warn("agent auth listener disabled via --no-auth — agents can read full API as Owner")
			go func() {
				if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
					logger.Error("server error", "err", err)
					stop()
				}
			}()
		} else {
			// Single auth-required listener. The UI and agents share
			// the same port; the difference is the Bearer token they
			// present (Owner vs per-agent). On first visit the UI
			// follows the printed URL whose ?token= query param
			// bootstraps the Owner token into localStorage.
			agentAPIBase := "http://" + actualAddr
			agent.SetKojoAPIBase(agentAPIBase)
			groupDMMgr.SetAPIBase(agentAPIBase)
			// KOJO_OWNER_TOKEN may be a custom string; URL-escape so
			// special chars (& # %) don't break the link.
			ownerTok := url.QueryEscape(tokens.OwnerToken())
			fmt.Fprintf(os.Stderr,
				"\n  kojo v%s running at:\n\n    http://%s\n\n  open this URL once to authorize the UI:\n    http://%s/?token=%s\n\n",
				version, actualAddr, actualAddr, ownerTok)
			go func() {
				if err := srv.ServeAuth(ln, resolver); err != nil && err != http.ErrServerClosed {
					logger.Error("server error", "err", err)
					stop()
				}
			}()
		}
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

		// get tailscale addresses for display. Display only — the agent
		// API base is wired further below to the local auth listener so
		// system prompts / PreCompact curl examples point Bearer-required.
		fmt.Fprintf(os.Stderr, "\n  kojo v%s running at:\n\n", version)
		lc, lcErr := tsServer.LocalClient()
		if lcErr != nil {
			logger.Warn("could not get tailscale local client", "err", lcErr)
		}
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
					fmt.Fprintf(os.Stderr, "    https://%s\n", net.JoinHostPort(ip.String(), strconv.Itoa(*port)))
				}
			} else {
				logger.Warn("could not get tailscale status", "err", err)
				fmt.Fprintf(os.Stderr, "    https://kojo:<tailnet>.ts.net:%d  (getting status...)\n", *port)
			}
		}

		// tsnet.ListenTLS returns a tls.Listener, serve directly
		go func() {
			// ServeTLS with empty cert/key since TLS is already handled by the listener
			srv.SetTLSConfig(&tls.Config{})
			if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
				logger.Error("server error", "err", err)
				stop()
			}
		}()

		// Additional auth-required listener bound to loopback. Agents
		// running in PTY sessions reach this via $KOJO_API_BASE; the
		// Tailscale listener above stays open for the user UI without
		// any token requirement, preserving the original UX.
		authLn, err := listenWithFallback("127.0.0.1", *port+1, 10, logger)
		if err != nil {
			logger.Error("failed to listen on auth port", "err", err)
			os.Exit(1)
		}
		authAddr := authLn.Addr().String()
		agentAPIBase := "http://" + authAddr
		agent.SetKojoAPIBase(agentAPIBase)
		// Group DM / PreCompact / system-prompt curl examples must hit
		// the auth listener so the agent's Bearer is honored. The
		// Tailscale listener is for the user UI only.
		groupDMMgr.SetAPIBase(agentAPIBase)
		fmt.Fprintf(os.Stderr, "    agent API: %s  (Bearer required)\n\n", agentAPIBase)
		go func() {
			if err := srv.ServeAuth(authLn, resolver); err != nil && err != http.ErrServerClosed {
				logger.Error("auth listener error", "err", err)
				stop()
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

// tsAddrForURL formats a Tailscale IP + port as a URL host string,
// wrapping IPv6 addresses in brackets.
func tsAddrForURL(ip netip.Addr, port int) string {
	return net.JoinHostPort(ip.String(), strconv.Itoa(port))
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
