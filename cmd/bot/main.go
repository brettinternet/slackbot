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
	buildVersion string
	buildTime    string

	// https://victoriametrics.com/blog/go-graceful-shutdown/
	terminationGracePeriod = 22 * time.Second
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
		BuildVersion: config.Default(buildVersion, "dev"),
		BuildTime:    buildTime,
	}

	b := bot.NewBot(opts)
	// Flags/commands are parsed after Run
	start, cmd := bot.NewCommandRoot(b)
	if err := cmd.Run(rootCtx, args); err != nil {
		return err
	}

	runCtx, runCancel := context.WithCancel(context.Background())
	svcErr := make(chan error, 1)
	if start != nil && *start {
		go func() {
			err := b.Run(runCtx)
			svcErr <- err
		}()
	}

	log := b.Logger()
	log.Info("Bot started.")
	select {
	case <-rootCtx.Done():
		stop()
		b.BeginShutdown(runCtx)
		sleepContext(runCtx, terminationDrainPeriod) // Give time for readiness check to propagate

		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), terminationGracePeriod)
		defer shutdownCancel()
		log.Info("Shutting down.")
		err := b.Shutdown(shutdownCtx)
		runCancel()
		if err != nil {
			log.Error("Error during bot shutdown.", zap.Error(err))
			sleepContext(shutdownCtx, terminationHardPeriod) // Give time for shutdown to complete
		}

		if err := b.ForceShutdown(shutdownCtx); err != nil {
			log.Error("Error during bot force shutdown.", zap.Error(err))
		}
	case err := <-svcErr:
		if err != nil {
			log.Error("Error during bot startup.", zap.Error(err))
		}
		log.Info("shutting down")
		if err := b.Shutdown(rootCtx); err != nil {
			log.Error("Error during bot shutdown.", zap.Error(err))
		}
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
