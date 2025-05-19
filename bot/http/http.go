package http

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
)

type Config struct {
	ServerURL string
}

type Server struct {
	log            *zap.Logger
	config         Config
	server         *http.Server
	serveMux       *http.ServeMux
	isShuttingDown atomic.Bool
	isReady        atomic.Bool
}

func NewServer(log *zap.Logger, config Config) *Server {
	h := &Server{
		log:      log,
		serveMux: http.NewServeMux(),
		config:   config,
	}
	h.registerEndpoints()
	return h
}

func (h *Server) Run(ctx context.Context) error {

	// Set the service as ready after a short delay
	go func() {
		time.Sleep(2 * time.Second)
		h.isReady.Store(true)
	}()

	su, err := url.ParseRequestURI(h.config.ServerURL)
	if err != nil || su == nil {
		return fmt.Errorf("invalid server URL: %w", err)
	}

	h.server = &http.Server{
		Addr:    su.Host,
		Handler: h.serveMux,
		BaseContext: func(_ net.Listener) context.Context {
			return ctx
		},
	}

	h.log.Info("Starting http server", zap.String("addr", su.Host))
	if err := h.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func (h *Server) BeginShutdown(ctx context.Context) error {
	h.isShuttingDown.Store(true)
	return nil
}

func (h *Server) Shutdown(ctx context.Context) error {
	return h.server.Shutdown(ctx)
}

func (h *Server) registerEndpoints() {
	h.serveMux.HandleFunc("/health", h.health)
	h.serveMux.HandleFunc("/healthz", h.healthz)
	h.serveMux.HandleFunc("/ready", h.ready)
}

func (h *Server) health(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if h.isShuttingDown.Load() { // allow draining by degrading readiness probe
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]string{
			"status": "shutting down",
			"time":   time.Now().Format(time.RFC3339),
		})
		return
	}
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{
		"status": "ok",
		"time":   time.Now().Format(time.RFC3339),
	})
}

func (h *Server) healthz(w http.ResponseWriter, r *http.Request) {
	if h.isShuttingDown.Load() { // allow draining by degrading readiness probe
		h.log.Error("Health check failed", zap.String("remoteAddr", r.RemoteAddr))
		http.Error(w, "Service is shutting down.", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Server) ready(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if !h.isReady.Load() {
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]string{
			"status": "not ready",
			"time":   time.Now().Format(time.RFC3339),
		})
		return
	}

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{
		"status": "ready",
		"time":   time.Now().Format(time.RFC3339),
	})
}
