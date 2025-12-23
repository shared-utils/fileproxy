package fileproxy

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// Server HTTP 伺服器
type Server struct {
	config     *Config
	proxy      *Proxy
	httpServer *http.Server
}

// NewServer 建立伺服器實例
func NewServer(cfg *Config) (*Server, error) {
	proxy, err := NewProxy(cfg)
	if err != nil {
		return nil, err
	}

	mux := http.NewServeMux()
	server := &Server{config: cfg, proxy: proxy}

	mux.HandleFunc("/health", server.handleHealth)
	mux.HandleFunc("/stats", server.handleStats)
	mux.Handle("/", proxy)

	server.httpServer = &http.Server{
		Addr:         cfg.ListenAddr,
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: cfg.UpstreamTimeout + 30*time.Second,
		IdleTimeout:  120 * time.Second,
	}

	return server, nil
}

// handleHealth 健康檢查端點
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// handleStats 統計資訊端點
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s.proxy.Stats())
}

// Start 啟動伺服器
func (s *Server) Start() error {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	errCh := make(chan error, 1)
	useTLS := s.config.TLSCertFile != "" && s.config.TLSKeyFile != ""

	go func() {
		slog.Info("server started",
			"addr", s.config.ListenAddr,
			"upstream", s.config.UpstreamURL,
			"cache_dir", s.config.CacheDir,
			"max_cache_gb", float64(s.config.MaxCacheSize)/(1<<30),
			"tls", useTLS,
		)

		var err error
		if useTLS {
			err = s.httpServer.ListenAndServeTLS(s.config.TLSCertFile, s.config.TLSKeyFile)
		} else {
			err = s.httpServer.ListenAndServe()
		}
		if err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case sig := <-sigCh:
		slog.Info("shutting down", "signal", sig)
		return s.Shutdown()
	}
}

// Shutdown 優雅關閉伺服器
func (s *Server) Shutdown() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := s.httpServer.Shutdown(ctx); err != nil {
		slog.Error("http shutdown error", "error", err)
	}

	if err := s.proxy.Close(); err != nil {
		slog.Error("proxy shutdown error", "error", err)
	}

	slog.Info("server stopped")
	return nil
}

// Run 便捷啟動方法
func Run(cfg *Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}

	server, err := NewServer(cfg)
	if err != nil {
		return err
	}

	return server.Start()
}
