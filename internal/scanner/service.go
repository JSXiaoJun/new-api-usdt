// Package scanner watches the USDT contract on each chain for transfers into
// issued deposit wallets, reports them to new-api once final, and sweeps
// wallets whose balance reaches the network's threshold.
package scanner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/shopspring/decimal"

	"new-api-usdt/internal/config"
	"new-api-usdt/internal/newapi"
	"new-api-usdt/internal/notify"
	"new-api-usdt/internal/rpc"
	"new-api-usdt/internal/store"
	"new-api-usdt/internal/sweep"
	"new-api-usdt/internal/usdt"
)

const Version = "0.1.6"

// addressBatch keeps the topic filter inside every provider's limit (TRON
// accepts at most 1000 options per topic).
const addressBatch = 500

type chainState struct {
	mu         sync.Mutex
	head       uint64
	cursor     uint64
	lastError  string
	sweepFails int
}

func (c *chainState) setError(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err == nil {
		c.lastError = ""
		return
	}
	c.lastError = err.Error()
}

type Service struct {
	cfg     *config.Config
	store   *store.Store
	api     *newapi.Client
	clients map[string]*rpc.Client
	senders map[string]sweep.Sender
	alerts  *notify.Manager

	mu      sync.RWMutex
	remote  map[string]newapi.Chain // chains new-api asks us to watch
	catchup map[string][]store.Address
	stats   map[string]*chainState
}

func New(cfg *config.Config, st *store.Store) (*Service, error) {
	s := &Service{
		cfg: cfg, store: st, api: newapi.New(cfg.NewAPIURL, cfg.ScannerSecret),
		clients: map[string]*rpc.Client{}, senders: map[string]sweep.Sender{},
		remote: map[string]newapi.Chain{}, catchup: map[string][]store.Address{}, stats: map[string]*chainState{},
	}
	s.alerts = newAlertManager(cfg, st)
	for id, c := range cfg.Chains {
		headers := map[string]string{}
		if id == usdt.ChainTron && cfg.TronAPIKey != "" {
			headers["TRON-PRO-API-KEY"] = cfg.TronAPIKey
		}
		client, err := rpc.New(c.RPC, headers)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", id, err)
		}
		s.clients[id] = client
		s.stats[id] = &chainState{}
		if cfg.SweepEnabled {
			chain, _ := usdt.Lookup(id)
			if chain.Kind == usdt.KindTron {
				s.senders[id] = sweep.NewTron(client, cfg.TronAPI, cfg.TronAPIKey, cfg.GasKey, cfg.TronFeeLimit)
			} else {
				s.senders[id] = sweep.NewEVM(client, chain.ChainID, cfg.GasKey, c.MaxGasPriceWei)
			}
		}
	}
	return s, nil
}

// GasWallets lists the fee payer per chain so the operator can fund it.
func (s *Service) GasWallets() map[string]string {
	out := map[string]string{}
	for id, sender := range s.senders {
		if t, ok := sender.(*sweep.Tron); ok {
			out[id] = t.TronAddress()
			continue
		}
		out[id] = sender.Address()
	}
	return out
}

// CheckChains refuses to start against an RPC that serves another network.
func (s *Service) CheckChains(ctx context.Context) error {
	for id, client := range s.clients {
		chain, _ := usdt.Lookup(id)
		got, err := client.ChainID(ctx)
		if err != nil {
			return fmt.Errorf("%s chain id: %w", id, err)
		}
		if got != chain.ChainID {
			return fmt.Errorf("%s RPC serves chain id %d, expected %d", id, got, chain.ChainID)
		}
	}
	return nil
}

func (s *Service) Run(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Go(func() { s.every(ctx, 30*time.Second, "sync", s.syncState) })
	wg.Go(func() { s.every(ctx, 5*time.Second, "notify", s.notify) })
	wg.Go(func() { s.every(ctx, time.Minute, "heartbeat", s.heartbeat) })
	wg.Go(func() { s.every(ctx, time.Minute, "gas-alerts", s.checkGasAlerts) })
	wg.Go(func() { s.every(ctx, time.Minute, "factory-events", s.watchFactoryEvents) })
	wg.Go(func() { s.every(ctx, 5*time.Minute, "destination-watch", s.checkDestination) })
	wg.Go(func() { s.every(ctx, 30*time.Second, "daily-summary", s.dailySummary) })
	for id := range s.clients {
		wg.Go(func() {
			s.every(ctx, s.cfg.PollInterval, "scan "+id, func(ctx context.Context) error { return s.scanChain(ctx, id) })
		})
		if s.senders[id] != nil {
			wg.Go(func() {
				s.every(ctx, 30*time.Second, "sweep "+id, func(ctx context.Context) error {
					err := s.sweepChain(ctx, id)
					s.noteSweepResult(ctx, id, err)
					return err
				})
			})
		}
	}
	wg.Wait()
}

func (s *Service) every(ctx context.Context, interval time.Duration, name string, fn func(context.Context) error) {
	for {
		runCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		if err := fn(runCtx); err != nil && ctx.Err() == nil {
			slog.Warn("loop failed", "loop", name, "error", err)
		}
		cancel()
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

// syncState pulls the watched networks and newly issued addresses.
func (s *Service) syncState(ctx context.Context) error {
	since, err := s.store.MaxAddressID(ctx)
	if err != nil {
		return err
	}
	for {
		state, err := s.api.State(ctx, since, 1000)
		if err != nil {
			return err
		}
		remote := map[string]newapi.Chain{}
		for _, c := range state.Chains {
			local, ok := usdt.Lookup(c.ID)
			// Never trust a contract or chain id that disagrees with our own table.
			if !ok || c.ChainID != local.ChainID || !strings.EqualFold(c.USDT, local.USDT) || c.Decimals != local.Decimals {
				slog.Error("new-api chain description does not match the built-in table; ignoring", "chain", c.ID)
				continue
			}
			remote[c.ID] = c
		}
		s.mu.Lock()
		s.remote = remote
		s.mu.Unlock()

		for _, a := range state.Addresses {
			row := store.Address{ID: a.ID, Chain: a.Chain, Address: strings.ToLower(a.Address), Salt: strings.ToLower(a.Salt)}
			added, err := s.store.AddAddress(ctx, row)
			if err != nil {
				return err
			}
			if added {
				s.mu.Lock()
				s.catchup[row.Chain] = append(s.catchup[row.Chain], row)
				s.mu.Unlock()
			}
		}
		if len(state.Addresses) == 0 || state.NextSince <= since {
			return nil
		}
		since = state.NextSince
	}
}

func (s *Service) remoteChain(id string) (newapi.Chain, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.remote[id]
	return c, ok
}

// remoteChains snapshots the chains new-api currently asks us to watch.
func (s *Service) remoteChains() map[string]newapi.Chain {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]newapi.Chain, len(s.remote))
	for id, c := range s.remote {
		out[id] = c
	}
	return out
}

// safeHead is the newest block new-api will accept as final.
func safeHead(ctx context.Context, client *rpc.Client, chain newapi.Chain) (uint64, uint64, error) {
	head, err := client.BlockNumber(ctx)
	if err != nil {
		return 0, 0, err
	}
	switch chain.Finality {
	case "finalized", "safe":
		safe, err := client.TaggedBlockNumber(ctx, chain.Finality)
		return head, safe, err
	default:
		required := uint64(max(chain.Confirmations, 1))
		if head < required {
			return head, 0, nil
		}
		return head, head - required, nil
	}
}

func (s *Service) scanChain(ctx context.Context, id string) error {
	chain, ok := s.remoteChain(id)
	stats := s.stats[id]
	if !ok {
		return nil
	}
	client := s.clients[id]
	local := s.cfg.Chains[id]
	head, safe, err := safeHead(ctx, client, chain)
	stats.setError(err)
	if err != nil {
		return err
	}
	cursor, found, err := s.store.Cursor(ctx, id)
	if err != nil {
		return err
	}
	if !found {
		start := local.StartBlock
		if start == 0 {
			start = safe - min(safe, local.LookbackBlocks)
		}
		cursor = max(start, 1) - 1
		if err := s.store.SetCursor(ctx, id, cursor); err != nil {
			return err
		}
		slog.Info("cursor initialized", "chain", id, "block", cursor)
	}
	stats.mu.Lock()
	stats.head, stats.cursor = head, cursor
	stats.mu.Unlock()

	// Rescan recent blocks for addresses issued since the last pass: a user can
	// pay before this scanner has synced their new address.
	s.mu.Lock()
	fresh := s.catchup[id]
	delete(s.catchup, id)
	s.mu.Unlock()
	if len(fresh) > 0 {
		from := cursor - min(cursor, local.CatchupBlocks) + 1
		if err := s.scanRanges(ctx, id, chain, from, cursor, fresh); err != nil {
			s.mu.Lock()
			s.catchup[id] = append(s.catchup[id], fresh...)
			s.mu.Unlock()
			stats.setError(err)
			return err
		}
	}

	if cursor >= safe {
		return nil
	}
	addrs, err := s.store.Addresses(ctx, id)
	if err != nil {
		return err
	}
	// Bounded work per pass; the next pass continues from the saved cursor.
	for range 20 {
		if cursor >= safe {
			break
		}
		to := min(safe, cursor+local.MaxRange)
		if err := s.scanRanges(ctx, id, chain, cursor+1, to, addrs); err != nil {
			stats.setError(err)
			return err
		}
		if err := s.store.SetCursor(ctx, id, to); err != nil {
			return err
		}
		cursor = to
		stats.mu.Lock()
		stats.cursor = cursor
		stats.mu.Unlock()
	}
	stats.setError(nil)
	return nil
}

func padTopic(address string) string {
	return "0x000000000000000000000000" + strings.TrimPrefix(strings.ToLower(address), "0x")
}

// scanRanges records every USDT transfer into addrs within [from, to].
func (s *Service) scanRanges(ctx context.Context, id string, chain newapi.Chain, from, to uint64, addrs []store.Address) error {
	if len(addrs) == 0 || from > to {
		return nil
	}
	watched := make(map[string]bool, len(addrs))
	for _, a := range addrs {
		watched[a.Address] = true
	}
	client := s.clients[id]
	for start := 0; start < len(addrs); start += addressBatch {
		batch := addrs[start:min(start+addressBatch, len(addrs))]
		topics := make([]string, 0, len(batch))
		for _, a := range batch {
			topics = append(topics, padTopic(a.Address))
		}
		logs, err := client.GetLogs(ctx, from, to, chain.USDT, [][]string{{usdt.TransferTopic}, nil, topics})
		if err != nil {
			return fmt.Errorf("getLogs %d-%d: %w", from, to, err)
		}
		for _, l := range logs {
			if l.Removed || len(l.Topics) != 3 || !strings.EqualFold(l.Address, chain.USDT) || !strings.EqualFold(l.Topics[0], usdt.TransferTopic) {
				continue
			}
			recipient := "0x" + strings.ToLower(l.Topics[2][len(l.Topics[2])-40:])
			if !watched[recipient] {
				continue
			}
			amount, err := rpc.ParseBig(l.Data)
			if err != nil || amount.Sign() <= 0 {
				continue
			}
			block, errBlock := rpc.ParseQuantity(l.BlockNumber)
			index, errIndex := rpc.ParseQuantity(l.LogIndex)
			if errBlock != nil || errIndex != nil {
				continue
			}
			d := store.Deposit{
				Chain: id, TxHash: strings.ToLower(l.TxHash), LogIndex: int(index), Address: recipient,
				Amount: decimal.NewFromBigInt(amount, -chain.Decimals).String(), Block: block,
			}
			if err := s.store.AddDeposit(ctx, d); err != nil {
				return err
			}
			slog.Info("deposit seen", "chain", id, "tx", d.TxHash, "log", d.LogIndex, "to", recipient, "amount", d.Amount)
		}
	}
	return nil
}

// notify reports queued deposits; new-api verifies each on chain itself.
func (s *Service) notify(ctx context.Context) error {
	due, err := s.store.DueDeposits(ctx, 50)
	if err != nil {
		return err
	}
	for _, d := range due {
		result, err := s.api.ReportDeposit(ctx, d.Chain, d.TxHash, d.LogIndex)
		switch {
		case err != nil:
			_ = s.store.RetryDeposit(ctx, d, err.Error())
		case !result.Settled:
			_ = s.store.RetryDeposit(ctx, d, result.Detail)
		default:
			status := "done"
			if result.Status == "rejected" {
				status = "rejected"
				slog.Error("new-api rejected a deposit", "chain", d.Chain, "tx", d.TxHash, "detail", result.Detail)
			}
			if err := s.store.SettleDeposit(ctx, d, status, result.Detail); err != nil {
				return err
			}
			slog.Info("deposit reported", "chain", d.Chain, "tx", d.TxHash, "log", d.LogIndex, "status", status)
		}
	}
	return nil
}

var balanceOfSelector = []byte{0x70, 0xa0, 0x82, 0x31}

func tokenBalance(ctx context.Context, client *rpc.Client, token, holder string) (*big.Int, error) {
	data := append(append([]byte{}, balanceOfSelector...), make([]byte, 12)...)
	raw, err := usdt.ParseHex20(holder)
	if err != nil {
		return nil, err
	}
	data = append(data, raw[:]...)
	out, err := client.EthCall(ctx, "", token, data)
	if err != nil {
		return nil, err
	}
	if len(out) != 32 {
		return nil, errors.New("unexpected balanceOf result")
	}
	return new(big.Int).SetBytes(out), nil
}

// sweepChain settles earlier sweeps, then batches wallets at or above the
// threshold into one sweepMany transaction.
func (s *Service) sweepChain(ctx context.Context, id string) error {
	chain, ok := s.remoteChain(id)
	if !ok {
		return nil
	}
	sender := s.senders[id]
	client := s.clients[id]
	pending, err := s.store.PendingSweeps(ctx, id)
	if err != nil {
		return err
	}
	stillPending := 0
	for _, sw := range pending {
		done, success, err := sender.Status(ctx, sw.TxHash)
		if err != nil {
			stillPending++
			continue
		}
		if !done {
			if time.Since(time.Unix(sw.CreatedAt, 0)) > 30*time.Minute {
				_ = s.store.FinishSweep(ctx, sw.ID, "dropped", "not mined after 30 minutes")
				s.recheckSweptWallets(ctx, id, sw.Address)
				continue
			}
			stillPending++
			continue
		}
		status := "success"
		if !success {
			status = "failed"
			slog.Error("sweep reverted", "chain", id, "tx", sw.TxHash)
		}
		_ = s.store.FinishSweep(ctx, sw.ID, status, "")
		s.recheckSweptWallets(ctx, id, sw.Address)
	}
	if stillPending > 0 {
		return nil // one in-flight sweep per chain keeps nonces simple
	}

	threshold, err := decimal.NewFromString(chain.SweepThreshold)
	if err != nil {
		return fmt.Errorf("invalid sweep threshold %q", chain.SweepThreshold)
	}
	thresholdRaw := threshold.Shift(chain.Decimals).BigInt()
	factory, err := usdt.ParseHex20(chain.Factory)
	if err != nil {
		return err
	}
	token, _ := usdt.ParseHex20(chain.USDT)
	wallets, err := s.store.WalletsToCheck(ctx, id, 20)
	if err != nil {
		return err
	}
	var salts [][32]byte
	var swept []string
	total := new(big.Int)
	for _, w := range wallets {
		balance, err := tokenBalance(ctx, client, chain.USDT, w)
		if err != nil {
			return err
		}
		if balance.Sign() == 0 || balance.Cmp(thresholdRaw) < 0 {
			// Below the threshold: the next deposit to this wallet flags it again.
			_ = s.store.MarkWalletChecked(ctx, id, w, false)
			continue
		}
		addr, err := s.store.AddressByWallet(ctx, id, w)
		if err != nil || addr == nil {
			continue
		}
		salt, err := usdt.ParseHex32(addr.Salt)
		if err != nil {
			continue
		}
		salts = append(salts, salt)
		swept = append(swept, w)
		total.Add(total, balance)
	}
	if len(salts) == 0 {
		return nil
	}
	txHash, err := sender.Send(ctx, factory, salts, token)
	if err != nil {
		return fmt.Errorf("sweep %d wallets: %w", len(salts), err)
	}
	if _, err := s.store.AddSweep(ctx, store.Sweep{
		Chain: id, Address: strings.Join(swept, ","), Amount: decimal.NewFromBigInt(total, -chain.Decimals).String(), TxHash: txHash,
	}); err != nil {
		return err
	}
	for _, w := range swept {
		_ = s.store.MarkWalletChecked(ctx, id, w, false)
	}
	slog.Info("sweep sent", "chain", id, "tx", txHash, "wallets", len(salts), "amount", decimal.NewFromBigInt(total, -chain.Decimals).String())
	return nil
}

// recheckSweptWallets flags wallets of a finished sweep so any balance left
// (a deposit racing the sweep, or a failed sweep) is checked again.
func (s *Service) recheckSweptWallets(ctx context.Context, id, joined string) {
	for _, w := range strings.Split(joined, ",") {
		if w != "" {
			_ = s.store.MarkWalletChecked(ctx, id, w, true)
		}
	}
}

func (s *Service) heartbeat(ctx context.Context) error {
	report := map[string]newapi.ChainHeartbeat{}
	for id, stats := range s.stats {
		stats.mu.Lock()
		hb := newapi.ChainHeartbeat{Head: stats.head, Cursor: stats.cursor, LastError: stats.lastError}
		stats.mu.Unlock()
		if sender := s.senders[id]; sender != nil {
			if bal, err := s.clients[id].Balance(ctx, sender.Address()); err == nil {
				decimals := int32(18)
				if id == usdt.ChainTron {
					decimals = 6
				}
				chain, _ := usdt.Lookup(id)
				hb.GasBalance = decimal.NewFromBigInt(bal, -decimals).String() + " " + chain.NativeSymbol
			}
			if pending, err := s.store.PendingSweeps(ctx, id); err == nil {
				hb.PendingSweeps = len(pending)
			}
		}
		report[id] = hb
	}
	return s.api.Heartbeat(ctx, Version, report)
}
