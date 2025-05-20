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

const DefaultServerPort = 4200

type Config struct {
	ServerHost     string
	ServerPort     uint32
	SlackEventPath string // Path for the Slack events API endpoint
}

type Server struct {
	log                  *zap.Logger
	config               Config
	server               *http.Server
	serveMux             *http.ServeMux
	isShuttingDown       atomic.Bool
	isReady              atomic.Bool
	slackVerifier        slackVerifier
	slackEventProcessors []slackEventProcessor
}

func NewServer(log *zap.Logger, config Config, slack slackVerifier) *Server {
	h := &Server{
		log:           log,
		serveMux:      http.NewServeMux(),
		config:        config,
		slackVerifier: slack,
	}
	h.registerHealthEndpoints()
	h.registerSlackEndpoints()
	return h
}

func (h *Server) Run(ctx context.Context) error {
	host := h.config.ServerHost
	su, err := url.ParseRequestURI(h.config.ServerHost)
	if err == nil {
		host = su.Hostname()
	}
	port := h.config.ServerPort
	if port == 0 {
		port = DefaultServerPort
	}
	addr := fmt.Sprintf("%s:%d", host, port)
	h.server = &http.Server{
		Addr:    addr,
		Handler: h.serveMux,
		BaseContext: func(_ net.Listener) context.Context {
			return ctx
		},
	}

	// Set the service as ready after a short delay
	go func() {
		time.Sleep(2 * time.Second)
		h.isReady.Store(true)
		h.log.Debug("Service is ready", zap.String("addr", addr))
	}()

	h.log.Info("Starting http server", zap.String("addr", addr))
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

func (h *Server) registerHealthEndpoints() {
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
