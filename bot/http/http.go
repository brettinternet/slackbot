package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
)

const DefaultServerPort = 4200

type slackService interface {
	VerifyRequest(http.Header, []byte) error
}

type Config struct {
	ServerPort                    uint32
	SlackEventPath                string // Path for the Slack events API endpoint
	SlackEventDeduplicationWindow time.Duration
}

type Server struct {
	log                  *zap.Logger
	config               Config
	server               *http.Server
	serveMux             *http.ServeMux
	isShuttingDown       atomic.Bool
	isReady              atomic.Bool
	slack                slackService
	slackEventProcessors []*slackEventDispatcher
	serverMu             sync.RWMutex // Protects server field
	listen               func(network, address string) (net.Listener, error)
	dispatchMu           sync.RWMutex
	dispatchAccepting    bool
	dispatchWG           sync.WaitGroup
	dispatchDone         chan struct{}
	eventDeduplicator    *eventDeduplicator
}

func NewServer(log *zap.Logger, config Config, slack slackService) *Server {
	h := &Server{
		log:               log,
		serveMux:          http.NewServeMux(),
		config:            config,
		slack:             slack,
		dispatchAccepting: true,
		eventDeduplicator: newEventDeduplicator(config.SlackEventDeduplicationWindow),
		listen:            net.Listen,
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

	server := &http.Server{
		Addr:              addr,
		Handler:           h.serveMux,
		ReadHeaderTimeout: time.Second * 10,
		ReadTimeout:       time.Second * 30,
		WriteTimeout:      time.Second * 30,
		IdleTimeout:       time.Second * 120,
		BaseContext: func(_ net.Listener) context.Context {
			return ctx
		},
	}

	h.serverMu.Lock()
	h.server = server
	h.serverMu.Unlock()

	listener, err := h.listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", addr, err)
	}

	// Bot.Run starts the HTTP server only after Slack and every configured feature
	// worker have started. A bound listener therefore represents completed startup,
	// rather than an arbitrary elapsed delay.
	h.isReady.Store(true)
	defer h.isReady.Store(false)
	h.log.Info("Starting http server", zap.String("addr", addr))
	h.log.Debug("Service is ready", zap.String("addr", addr))
	if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func (h *Server) BeginShutdown(ctx context.Context) error {
	h.isReady.Store(false)
	h.isShuttingDown.Store(true)
	return nil
}

func (h *Server) Shutdown(ctx context.Context) error {
	var shutdownErr error
	h.serverMu.RLock()
	server := h.server
	h.serverMu.RUnlock()
	if server != nil {
		shutdownErr = server.Shutdown(ctx)
	}

	done := h.closeDispatchQueues()
	select {
	case <-done:
		if err := h.waitForProcessorQueues(ctx); err != nil {
			return errors.Join(shutdownErr, err)
		}
		return shutdownErr
	case <-ctx.Done():
		h.abandonDispatchQueues()
		h.reportProcessorPending()
		return errors.Join(shutdownErr, fmt.Errorf("drain Slack event processors: %w", ctx.Err()))
	}
}

func (h *Server) closeDispatchQueues() <-chan struct{} {
	h.dispatchMu.Lock()
	defer h.dispatchMu.Unlock()
	h.dispatchAccepting = false
	if h.dispatchDone != nil {
		return h.dispatchDone
	}
	for _, dispatcher := range h.slackEventProcessors {
		close(dispatcher.queue)
	}
	h.dispatchDone = make(chan struct{})
	go func() {
		h.dispatchWG.Wait()
		close(h.dispatchDone)
	}()
	return h.dispatchDone
}

func (h *Server) abandonDispatchQueues() {
	h.dispatchMu.RLock()
	defer h.dispatchMu.RUnlock()
	for _, dispatcher := range h.slackEventProcessors {
		abandoned, inFlight := dispatcher.abandon()
		if abandoned > 0 {
			h.log.Error("Slack processor events abandoned at shutdown deadline",
				zap.String("processor", dispatcher.processor.ProcessorType()),
				zap.Int("count", abandoned))
		}
		if inFlight > 0 {
			h.log.Error("Slack processor events still in flight at shutdown deadline",
				zap.String("processor", dispatcher.processor.ProcessorType()),
				zap.Int("count", inFlight))
		}
	}
}

func (h *Server) waitForProcessorQueues(ctx context.Context) error {
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		if h.processorPendingCount() == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			h.reportProcessorPending()
			return fmt.Errorf("drain processor-internal event queues: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func (h *Server) processorPendingCount() int64 {
	var total int64
	for _, dispatcher := range h.slackEventProcessors {
		if reporter, ok := dispatcher.processor.(slackEventPendingReporter); ok {
			total += reporter.PendingEvents()
		}
	}
	return total
}

func (h *Server) reportProcessorPending() {
	for _, dispatcher := range h.slackEventProcessors {
		if reporter, ok := dispatcher.processor.(slackEventPendingReporter); ok {
			if count := reporter.PendingEvents(); count > 0 {
				h.log.Error("Slack processor events abandoned at shutdown deadline",
					zap.String("processor", dispatcher.processor.ProcessorType()),
					zap.Int64("count", count))
			}
		}
	}
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

	if h.isShuttingDown.Load() || !h.isReady.Load() {
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
