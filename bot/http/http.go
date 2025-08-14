package http

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
)

const DefaultServerPort = 4200

type slackService interface {
	VerifyRequest(http.Header, []byte) error
}

type Config struct {
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
	slack                slackService
	slackEventProcessors []slackEventProcessor
}

func NewServer(log *zap.Logger, config Config, slack slackService) *Server {
	h := &Server{
		log:      log,
		serveMux: http.NewServeMux(),
		config:   config,
		slack:    slack,
	}
	h.registerHealthEndpoints()
	h.registerSlackEndpoints()
	return h
}

func (h *Server) Run(ctx context.Context) error {
	port := h.config.ServerPort
	if port == 0 {
		port = DefaultServerPort
	}
	addr := fmt.Sprintf(":%d", port)
	h.server = &http.Server{
		Addr:               addr,
		Handler:            h.serveMux,
		ReadHeaderTimeout:  time.Second * 10,
		ReadTimeout:        time.Second * 30,
		WriteTimeout:       time.Second * 30,
		IdleTimeout:        time.Second * 120,
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
	if h.server == nil {
		return nil
	}
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
		_ = json.NewEncoder(w).Encode(map[string]string{
			"status": "shutting down",
			"time":   time.Now().Format(time.RFC3339),
		})
		return
	}
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{
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
		_ = json.NewEncoder(w).Encode(map[string]string{
			"status": "not ready",
			"time":   time.Now().Format(time.RFC3339),
		})
		return
	}

	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"status": "ready",
		"time":   time.Now().Format(time.RFC3339),
	})
}
