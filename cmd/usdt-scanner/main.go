// usdt-scanner watches USDT deposits for new-api and sweeps deposit wallets.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/shopspring/decimal"

	"new-api-usdt/internal/config"
	"new-api-usdt/internal/scanner"
	"new-api-usdt/internal/store"
)

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))
	var err error
	switch {
	case len(os.Args) > 1 && os.Args[1] == "sweep-once":
		err = runForceSweep(os.Args[2:])
	default:
		err = run()
	}
	if err != nil {
		slog.Error("usdt-scanner stopped", "error", err)
		os.Exit(1)
	}
}

// runForceSweep ignores the dashboard threshold and collects every address
// holding at least -min USDT. Useful after the threshold was set high.
func runForceSweep(args []string) error {
	fs := flag.NewFlagSet("sweep-once", flag.ExitOnError)
	minFlag := fs.String("min", "0.01", "only sweep addresses holding at least this much USDT")
	_ = fs.Parse(args)
	min, err := decimal.NewFromString(*minFlag)
	if err != nil {
		return fmt.Errorf("-min must be a number")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	st, err := store.Open(cfg.DataDir)
	if err != nil {
		return err
	}
	defer st.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	return scanner.ForceSweep(ctx, cfg, st, min)
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
