package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.uber.org/zap"
	"slackbot.arpa/bot"
	"slackbot.arpa/bot/config"
)

var (
	buildVersion     string
	buildTime        string
	buildEnvironment string

	// https://victoriametrics.com/blog/go-graceful-shutdown/
	terminationGracePeriod = 12 * time.Second
	terminationDrainPeriod = 5 * time.Second
)

func init() {
	if buildEnvironment != config.EnvironmentProduction.String() {
		terminationGracePeriod = 4 * time.Second
		terminationDrainPeriod = 1 * time.Second
	}
}

func main() {
	if err := run(context.Background(), os.Args); err != nil {
		panic(err)
	}
}

func run(rootCtx context.Context, args []string) error {
	rootCtx, stop := signal.NotifyContext(rootCtx, os.Interrupt, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()

	opts := config.BuildOpts{
		BuildVersion:     config.Default(buildVersion, "dev"),
		BuildTime:        buildTime,
		BuildEnvironment: buildEnvironment,
	}

	b := bot.NewBot(opts)
	// Flags/commands are parsed after Run
	start, cmd := bot.NewCommandRoot(b)
	if err := cmd.Run(rootCtx, args); err != nil {
		return errors.Join(err, shutdownBot(b))
	}

	if start == nil || !*start {
		return shutdownBot(b)
	}

	runCtx, runCancel := context.WithCancel(context.Background())
	svcErr := make(chan error, 1)
	go func() {
		err := b.Run(runCtx)
		svcErr <- err
	}()

	log := b.Logger()
	log.Info("Server started.")
	var runErr error
	serviceResultRead := false
	select {
	case <-rootCtx.Done():
	case err := <-svcErr:
		serviceResultRead = true
		if err != nil {
			runErr = fmt.Errorf("run server: %w", err)
			log.Error("Error while running server.", zap.Error(err))
		}
	}
	stop()
	log.Info("Received shutdown signal, beginning graceful shutdown.")
	if err := b.BeginShutdown(runCtx); err != nil {
		runErr = errors.Join(runErr, fmt.Errorf("begin shutdown: %w", err))
		log.Error("Error during begin shutdown.", zap.Error(err))
	}
	if err := sleepContext(runCtx, terminationDrainPeriod); err != nil { // Give time for readiness check to propagate
		runErr = errors.Join(runErr, fmt.Errorf("drain wait: %w", err))
		log.Error("Error during drain wait.", zap.Error(err))
	}
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), terminationGracePeriod)
	defer shutdownCancel()
	log.Info("Shutting down.")
	if err := b.Shutdown(shutdownCtx); err != nil {
		runErr = errors.Join(runErr, fmt.Errorf("shutdown: %w", err))
		log.Error("Error during server shutdown.", zap.Error(err))
	}
	runCancel()
	if !serviceResultRead {
		select {
		case err := <-svcErr:
			serviceResultRead = true
			if err != nil {
				runErr = errors.Join(runErr, fmt.Errorf("run server: %w", err))
			}
		default:
		}
	}
	if !serviceResultRead {
		select {
		case err := <-svcErr:
			if err != nil {
				runErr = errors.Join(runErr, fmt.Errorf("run server: %w", err))
			}
		case <-shutdownCtx.Done():
			runErr = errors.Join(runErr, fmt.Errorf("wait for server exit: %w", shutdownCtx.Err()))
		}
	}
	log.Info("Shutdown complete.")
	return runErr
}

func shutdownBot(b *bot.Bot) error {
	ctx, cancel := context.WithTimeout(context.Background(), terminationGracePeriod)
	defer cancel()
	if err := b.Shutdown(ctx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}

func sleepContext(ctx context.Context, duration time.Duration) error {
	select {
	case <-time.After(duration):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
