// usdt-scanner watches USDT deposits for new-api and sweeps deposit wallets.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"new-api-usdt/internal/config"
	"new-api-usdt/internal/scanner"
	"new-api-usdt/internal/store"
)

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))
	if err := run(); err != nil {
		slog.Error("usdt-scanner stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return err
	}
	st, err := store.Open(cfg.DataDir)
	if err != nil {
		return err
	}
	defer st.Close()

	svc, err := scanner.New(cfg, st)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	checkCtx, cancel := context.WithTimeout(ctx, time.Minute)
	err = svc.CheckChains(checkCtx)
	cancel()
	if err != nil {
		return err
	}
	for chain, addr := range svc.GasWallets() {
		slog.Info("gas wallet (fund it with the chain's native coin)", "chain", chain, "address", addr)
	}

	server := &http.Server{Addr: cfg.ListenAddr, ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, "ok %s\n", scanner.Version)
	})}
	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Warn("health endpoint stopped", "error", err)
		}
	}()
	defer server.Close()

	slog.Info("usdt-scanner started", "version", scanner.Version, "chains", len(cfg.Chains), "sweep", cfg.SweepEnabled)
	svc.Run(ctx)
	return nil
}
