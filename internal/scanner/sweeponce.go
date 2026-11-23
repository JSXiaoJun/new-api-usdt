package scanner

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"new-api-usdt/internal/config"
	"new-api-usdt/internal/newapi"
	"new-api-usdt/internal/rpc"
	"new-api-usdt/internal/store"
	"new-api-usdt/internal/sweep"
	"new-api-usdt/internal/usdt"
)

// ForceSweep collects every issued address whose balance is at least min,
// regardless of the per-chain sweep threshold configured in new-api. It exists
// because the normal sweeper only revisits wallets that a new deposit flagged:
// lowering the threshold in the dashboard does not rescue balances that have
// been sitting untouched.
func ForceSweep(ctx context.Context, cfg *config.Config, st *store.Store, min decimal.Decimal) error {
	if cfg.GasKey == nil {
		return fmt.Errorf("GAS_PRIVATE_KEY is required for a forced sweep")
	}
	api := newapi.New(cfg.NewAPIURL, cfg.ScannerSecret)
	state, err := api.State(ctx, 0, 1000)
	if err != nil {
		return fmt.Errorf("read chains from new-api: %w", err)
	}
	done := 0
	for _, chain := range state.Chains {
		local, ok := usdt.Lookup(chain.ID)
		if !ok || local.Kind != usdt.KindEVM {
			continue
		}
		localCfg, ok := cfg.Chains[chain.ID]
		if !ok {
			slog.Warn("force sweep: no RPC configured", "chain", chain.ID)
			continue
		}
		rpcClient, err := rpc.New(localCfg.RPC, nil)
		if err != nil {
			return fmt.Errorf("%s rpc: %w", chain.ID, err)
		}
		factory, err := usdt.ParseHex20(chain.Factory)
		if err != nil {
			slog.Warn("force sweep: bad factory", "chain", chain.ID, "error", err)
			continue
		}
		token, err := usdt.ParseHex20(chain.USDT)
		if err != nil {
			slog.Warn("force sweep: bad token", "chain", chain.ID, "error", err)
			continue
		}
		sender := sweep.NewEVM(rpcClient, local.ChainID, cfg.GasKey, localCfg.MaxGasPriceWei)

		rows, err := st.Addresses(ctx, chain.ID)
		if err != nil {
			return err
		}
		slog.Info("force sweep: scanning", "chain", chain.ID, "addresses", len(rows), "min", min.String())
		var (
			salts   [][32]byte
			wallets []string
			total   = decimal.Zero
		)
		flush := func() error {
			if len(salts) == 0 {
				return nil
			}
			hash, err := sender.Send(ctx, factory, salts, token)
			if err != nil {
				return fmt.Errorf("%s sweep %d wallets: %w", chain.ID, len(salts), err)
			}
			if err := waitMined(ctx, rpcClient, hash); err != nil {
				return fmt.Errorf("%s sweep %s: %w", chain.ID, hash, err)
			}
			if _, err := st.AddSweep(ctx, store.Sweep{
				Chain: chain.ID, Address: strings.Join(wallets, ","),
				Amount: total.String(), TxHash: hash,
			}); err != nil {
				return err
			}
			slog.Info("force sweep: sent", "chain", chain.ID, "tx", hash,
				"wallets", len(wallets), "amount", total.String())
			done += len(wallets)
			salts, wallets, total = nil, nil, decimal.Zero
			return nil
		}
		for _, row := range rows {
			bal, err := tokenBalance(ctx, rpcClient, chain.USDT, row.Address)
			if err != nil {
				slog.Warn("force sweep: balance", "chain", chain.ID, "wallet", row.Address, "error", err)
				continue
			}
			amount := decimal.NewFromBigInt(bal, -chain.Decimals)
			if amount.LessThan(min) {
				continue
			}
			salt, err := usdt.ParseHex32(row.Salt)
			if err != nil {
				continue
			}
			salts = append(salts, salt)
			wallets = append(wallets, row.Address)
			total = total.Add(amount)
			if len(salts) >= 25 {
				if err := flush(); err != nil {
					return err
				}
			}
		}
		if err := flush(); err != nil {
			return err
		}
	}
	slog.Info("force sweep: finished", "wallets", done)
	return nil
}

// waitMined blocks until a receipt is available or the context ends.
func waitMined(ctx context.Context, client *rpc.Client, hash string) error {
	for {
		receipt, err := client.Receipt(ctx, hash)
		if err == nil && receipt != nil {
			if receipt.Status != "0x1" {
				return fmt.Errorf("transaction reverted")
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("transaction %s not mined: %w", hash, ctx.Err())
		case <-time.After(3 * time.Second):
		}
	}
}
