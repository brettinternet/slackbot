package main

import (
	"context"
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
	terminationHardPeriod  = 3 * time.Second
)

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
		return err
	}

	if start == nil || !*start {
		return nil
	}

	runCtx, runCancel := context.WithCancel(context.Background())
	svcErr := make(chan error, 1)
	go func() {
		err := b.Run(runCtx)
		svcErr <- err
	}()

	log := b.Logger()
	log.Info("Server started.")
	select {
	case <-rootCtx.Done():
	case err := <-svcErr:
		if err != nil {
			log.Error("Error during server startup.", zap.Error(err))
		}
	}
	stop()
	log.Info("Received shutdown signal, beginning graceful shutdown.")
	if err := b.BeginShutdown(runCtx); err != nil {
		log.Error("Error during begin shutdown.", zap.Error(err))
	}
	if err := sleepContext(runCtx, terminationDrainPeriod); err != nil { // Give time for readiness check to propagate
		log.Error("Error during drain wait.", zap.Error(err))
	}
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), terminationGracePeriod)
	defer shutdownCancel()
	log.Info("Shutting down.")
	err := b.Shutdown(shutdownCtx)
	runCancel()
	if err != nil {
		log.Error("Error during server shutdown.", zap.Error(err))
		if err := sleepContext(shutdownCtx, terminationHardPeriod); err != nil { // Give time for shutdown to complete
			log.Error("Error during shutdown wait.", zap.Error(err))
		}
	}
	log.Info("Force shutting down server if still running.")
	if err := b.ForceShutdown(shutdownCtx); err != nil {
		log.Error("Error during server force shutdown.", zap.Error(err))
	}
	log.Info("Shutdown complete.")
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
