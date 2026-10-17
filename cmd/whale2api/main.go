package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"whale2api/internal/config"
	"whale2api/internal/poolui"
	"whale2api/internal/server"
)

func main() {
	if err := config.LoadDotEnv(); err != nil {
		config.Logger.Warn("[dotenv] load failed", "error", err)
	}
	config.RefreshLogger()
	app, err := server.NewApp()
	if err != nil {
		config.Logger.Error("server initialization failed", "error", err)
		os.Exit(1)
	}
	defer app.Close()

	// Render's free tier is easiest to run as one Web Service. When enabled,
	// mount the pool management UI on the same listener and share app.PoolDB
	// with the API. The normal Docker Compose deployment keeps its separate
	// /usr/local/bin/poolui process because this is opt-in.
	handler := app.Router
	if embeddedPoolUIEnabled() {
		poolServer, err := poolui.NewServer(app.PoolDB)
		if err != nil {
			config.Logger.Error("embedded pool ui initialization failed", "error", err)
			os.Exit(1)
		}
		handler = newEmbeddedHandler(app.Router, poolServer.Router)
		config.Logger.Info("embedded pool ui enabled", "paths", "/ and /api/*")
	}
	port := strings.TrimSpace(os.Getenv("PORT"))
	if port == "" {
		port = "5001"
	}

	srv := &http.Server{
		Addr:              "0.0.0.0:" + port,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}
	localURL := fmt.Sprintf("http://127.0.0.1:%s", port)
	lanIP := detectLANIPv4()
	lanURL := ""
	if lanIP != "" {
		lanURL = fmt.Sprintf("http://%s:%s", lanIP, port)
	}

	// Start server in a goroutine so we can listen for shutdown signals.
	go func() {
		if lanURL != "" {
			config.Logger.Info("starting whale2api", "bind", srv.Addr, "port", port, "local_url", localURL, "lan_url", lanURL, "lan_ip", lanIP)
		} else {
			config.Logger.Info("starting whale2api", "bind", srv.Addr, "port", port, "local_url", localURL)
			config.Logger.Warn("lan ip not detected; check active network interfaces")
		}
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			config.Logger.Error("server stopped unexpectedly", "error", err)
			os.Exit(1)
		}
	}()

	// Wait for interrupt signal (Ctrl+C / SIGTERM).
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)
	sig := <-quit
	config.Logger.Info("shutdown signal received", "signal", sig.String())

	// Graceful shutdown: allow up to 10 seconds for in-flight requests to complete.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		config.Logger.Error("graceful shutdown failed, forcing exit", "error", err)
		os.Exit(1)
	}
	config.Logger.Info("server gracefully stopped")
}

func embeddedPoolUIEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("WHALE2API_EMBED_POOL_UI"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// newEmbeddedHandler dispatches the pool UI's browser and management API
// routes while leaving all OpenAI-compatible routes on the gateway router.
// Keeping this as a small handler wrapper avoids changing the existing router
// and therefore preserves the standalone Compose services and their tests.
func newEmbeddedHandler(apiHandler, poolUIHandler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isPoolUIPath(r.URL.Path) {
			poolUIHandler.ServeHTTP(w, r)
			return
		}
		apiHandler.ServeHTTP(w, r)
	})
}

func isPoolUIPath(path string) bool {
	switch path {
	case "/", "/api", "/app.js", "/app.css", "/favicon.ico":
		return true
	default:
		return strings.HasPrefix(path, "/api/")
	}
}

func detectLANIPv4() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			default:
				continue
			}
			ip = ip.To4()
			if ip == nil || !ip.IsPrivate() {
				continue
			}
			return ip.String()
		}
	}
	return ""
}
