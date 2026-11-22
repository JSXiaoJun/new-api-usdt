package scanner

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/shopspring/decimal"

	"new-api-usdt/internal/config"
	"new-api-usdt/internal/notify"
	"new-api-usdt/internal/store"
	"new-api-usdt/internal/usdt"
)

// newAlertManager builds the mailer; without SMTP_HOST/MAIL_TO every alert is
// a no-op so the service behaves exactly as before.
func newAlertManager(cfg *config.Config, st *store.Store) *notify.Manager {
	var sender notify.Sender
	if cfg.SMTPHost != "" && len(cfg.MailTo) > 0 {
		sender = notify.NewSMTP(notify.Config{
			Host: cfg.SMTPHost,
			Port: cfg.SMTPPort,
			User: cfg.SMTPUser,
			Pass: cfg.SMTPPass,
			From: cfg.MailFrom,
			To:   cfg.MailTo,
		})
		slog.Info("email alerts enabled", "host", cfg.SMTPHost, "to", strings.Join(cfg.MailTo, ", "))
	}
	return notify.NewManager(sender, st, cfg.AlertCooldown)
}

// destinationChangedTopic is keccak("DestinationChanged(address,address)"),
// the only event that can redirect a factory's funds.
var destinationChangedTopic = crypto.Keccak256Hash(
	[]byte("DestinationChanged(address,address)")).Hex()

// alertExplorerTx maps a chain to a block explorer transaction URL prefix.
func alertExplorerTx(chain string) string {
	switch chain {
	case usdt.ChainBSC:
		return "https://bscscan.com/tx/"
	case usdt.ChainPolygon:
		return "https://polygonscan.com/tx/"
	case usdt.ChainArbitrum:
		return "https://arbiscan.io/tx/"
	case usdt.ChainTron:
		return "https://tronscan.org/#/transaction/"
	default:
		return ""
	}
}

// nativeBalance returns the gas wallet balance as a decimal in the chain's
// native coin.
func (s *Service) nativeBalance(ctx context.Context, chain string) (decimal.Decimal, bool) {
	sender := s.senders[chain]
	client := s.clients[chain]
	if sender == nil || client == nil {
		return decimal.Zero, false
	}
	bal, err := client.Balance(ctx, sender.Address())
	if err != nil {
		return decimal.Zero, false
	}
	decimals := int32(18)
	if chain == usdt.ChainTron {
		decimals = 6
	}
	return decimal.NewFromBigInt(bal, -decimals), true
}

// checkGasAlerts warns once per cooldown when a gas wallet drops below its
// floor, and sends a recovery note after a refill.
func (s *Service) checkGasAlerts(ctx context.Context) error {
	if !s.alerts.Enabled() {
		return nil
	}
	for chainID := range s.senders {
		local, ok := usdt.Lookup(chainID)
		if !ok {
			continue
		}
		floor, ok := s.cfg.GasMin[chainID]
		if !ok {
			continue
		}
		bal, ok := s.nativeBalance(ctx, chainID)
		if !ok {
			continue
		}
		lowKey := "gas-low:" + chainID
		if bal.LessThan(decimal.NewFromFloat(floor)) {
			addr := ""
			if sender := s.senders[chainID]; sender != nil {
				addr = sender.Address()
			}
			subject := fmt.Sprintf("[USDT 告警] %s Gas 钱包余额不足：%s %s", local.Name, bal.StringFixed(4), local.NativeSymbol)
			body := strings.Join([]string{
				"归集会暂停，但用户资金不受影响（仍在各自合约地址里）。",
				"",
				"网络：" + local.Name,
				"Gas 钱包：" + addr,
				"当前余额：" + bal.StringFixed(6) + " " + local.NativeSymbol,
				"告警阈值：" + decimal.NewFromFloat(floor).String() + " " + local.NativeSymbol,
				"",
				"充值网络请选择 " + local.Name + "，充到上面的 Gas 钱包地址。",
			}, "\n")
			if err := s.alerts.Alert(ctx, lowKey, subject, body); err != nil {
				slog.Warn("gas alert failed", "chain", chainID, "error", err)
			}
			continue
		}
		// Recovered? Only notify when the last low alert is newer than the last
		// recovery note, so a healthy wallet never mails on startup.
		lastLow, hasLow, err := s.store.LastAlert(ctx, lowKey)
		if err != nil || !hasLow {
			continue
		}
		lastOK, hasOK, err := s.store.LastAlert(ctx, "gas-ok:"+chainID)
		if err != nil {
			continue
		}
		if hasOK && !lastOK.Before(lastLow) {
			continue
		}
		subject := fmt.Sprintf("[USDT 恢复] %s Gas 钱包已充值：%s %s", local.Name, bal.StringFixed(4), local.NativeSymbol)
		body := fmt.Sprintf("网络：%s\n当前余额：%s %s\n归集已恢复正常。", local.Name, bal.StringFixed(6), local.NativeSymbol)
		if err := s.alerts.Alert(ctx, "gas-ok:"+chainID, subject, body); err != nil {
			slog.Warn("gas recovery alert failed", "chain", chainID, "error", err)
		}
	}
	return nil
}

// watchFactoryEvents reports every setDestination call on the watched factories.
func (s *Service) watchFactoryEvents(ctx context.Context) error {
	if !s.alerts.Enabled() {
		return nil
	}
	for chainID, chain := range s.remoteChains() {
		if strings.TrimSpace(chain.Factory) == "" {
			continue
		}
		client := s.clients[chainID]
		if client == nil {
			continue
		}
		head, err := client.BlockNumber(ctx)
		if err != nil {
			slog.Warn("factory events: head", "chain", chainID, "error", err)
			continue
		}
		key := "events:" + chainID
		cursor, found, err := s.store.Cursor(ctx, key)
		if err != nil {
			return err
		}
		if !found {
			// Start from the current head: historical changes are not news.
			if err := s.store.SetCursor(ctx, key, head); err != nil {
				return err
			}
			continue
		}
		if head <= cursor {
			continue
		}
		from := cursor + 1
		for from <= head {
			to := from + 999
			if to > head {
				to = head
			}
			logs, err := client.GetLogs(ctx, from, to, chain.Factory, [][]string{{destinationChangedTopic}})
			if err != nil {
				return fmt.Errorf("%s factory events %d-%d: %w", chainID, from, to, err)
			}
			for _, l := range logs {
				if l.Removed || len(l.Topics) < 3 {
					continue
				}
				oldAddr := topicAddress(l.Topics[1])
				newAddr := topicAddress(l.Topics[2])
				s.alertDestinationChange(ctx, chainID, chain.Factory, parseHexUint(l.BlockNumber), l.TxHash, oldAddr, newAddr)
			}
			if err := s.store.SetCursor(ctx, key, to); err != nil {
				return err
			}
			from = to + 1
		}
	}
	return nil
}

func (s *Service) alertDestinationChange(ctx context.Context, chainID, factory string, block uint64, txHash, oldAddr, newAddr string) {
	local, _ := usdt.Lookup(chainID)
	name := chainID
	if local.Name != "" {
		name = local.Name
	}
	link := ""
	if prefix := alertExplorerTx(chainID); prefix != "" {
		link = prefix + txHash
	}
	suspicious := ""
	if want, ok := s.cfg.ExpectedDestination[chainID]; ok && want != "" && !strings.EqualFold(want, newAddr) {
		suspicious = "\n\n⚠️ 新收款地址与配置的期望值不一致，请立刻核实！\n期望：" + want
	}
	subject := fmt.Sprintf("[USDT 告警] %s 收款地址被修改：%s → %s", name, shortAddr(oldAddr), shortAddr(newAddr))
	body := strings.Join([]string{
		"工厂合约的 setDestination 被调用，之后归集的钱会进入新地址。",
		"",
		"网络：" + name,
		"工厂合约：" + factory,
		"原收款地址：" + oldAddr,
		"新收款地址：" + newAddr,
		"交易：" + txHash,
		fmt.Sprintf("区块：%d", block),
		link,
		suspicious,
	}, "\n")
	if err := s.alerts.Alert(ctx, "dest-change:"+chainID, subject, body); err != nil {
		slog.Warn("destination change alert failed", "chain", chainID, "error", err)
	}
}

// noteSweepResult tracks consecutive sweep failures per chain and alerts on the
// third one, so a stuck gas wallet does not go unnoticed.
func (s *Service) noteSweepResult(ctx context.Context, chainID string, err error) {
	state := s.stats[chainID]
	if state == nil {
		return
	}
	state.mu.Lock()
	if err == nil {
		state.sweepFails = 0
		state.mu.Unlock()
		return
	}
	state.sweepFails++
	fails := state.sweepFails
	state.mu.Unlock()

	if fails < 3 {
		return
	}
	local, _ := usdt.Lookup(chainID)
	name := chainID
	if local.Name != "" {
		name = local.Name
	}
	subject := fmt.Sprintf("[USDT 告警] %s 归集连续失败 %d 次", name, fails)
	body := strings.Join([]string{
		"归集任务连续失败，用户资金仍在各自合约地址里，不会丢失。",
		"",
		"网络：" + name,
		"最近错误：" + err.Error(),
		"",
		"常见原因：Gas 钱包余额不足、RPC 异常、gas 价超过上限。",
	}, "\n")
	if aerr := s.alerts.Alert(ctx, "sweep-fail:"+chainID, subject, body); aerr != nil {
		slog.Warn("sweep failure alert failed", "chain", chainID, "error", aerr)
	}
}

// dailySummary sends one status mail per day at the configured local time.
func (s *Service) dailySummary(ctx context.Context) error {
	if !s.alerts.Enabled() {
		return nil
	}
	now := time.Now()
	if now.Hour() != s.cfg.DailyHour || now.Minute() != s.cfg.DailyMinute {
		return nil
	}
	key := "daily:" + now.Format("2006-01-02")
	if _, sent, err := s.store.LastAlert(ctx, key); err != nil || sent {
		return err
	}
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	count, total, err := s.store.SweepStatsSince(ctx, midnight)
	if err != nil {
		return err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "统计日期：%s\n\n", now.Format("2006-01-02"))
	for chainID := range s.cfg.Chains {
		local, _ := usdt.Lookup(chainID)
		name := chainID
		if local.Name != "" {
			name = local.Name
		}
		if bal, ok := s.nativeBalance(ctx, chainID); ok {
			fmt.Fprintf(&b, "%s\n  Gas 余额：%s %s\n", name, bal.StringFixed(4), local.NativeSymbol)
		} else {
			fmt.Fprintf(&b, "%s\n  Gas 余额：未配置\n", name)
		}
		if state := s.stats[chainID]; state != nil {
			state.mu.Lock()
			fmt.Fprintf(&b, "  最新区块：%d，已扫描：%d\n", state.head, state.cursor)
			state.mu.Unlock()
		}
		if pending, err := s.store.PendingSweeps(ctx, chainID); err == nil {
			fmt.Fprintf(&b, "  待完成归集：%d\n", len(pending))
		}
		if remote, ok := s.remoteChain(chainID); ok && remote.Factory != "" {
			if client := s.clients[chainID]; client != nil {
				if dest, err := factoryDestination(ctx, client, remote.Factory); err == nil {
					if raw, err := tokenBalance(ctx, client, remote.USDT, dest); err == nil {
						amt := decimal.NewFromBigInt(raw, -remote.Decimals)
						fmt.Fprintf(&b, "  收款地址 %s：%s USDT\n", shortAddr(dest), amt.StringFixed(2))
					}
				}
			}
		}
	}
	fmt.Fprintf(&b, "\n今日归集：%d 笔，合计 %s USDT", count, decimal.NewFromFloat(total).StringFixed(2))
	subject := fmt.Sprintf("[USDT 日报] %s 归集 %d 笔", now.Format("2006-01-02"), count)
	if err := s.alerts.Alert(ctx, key, subject, b.String()); err != nil {
		return err
	}
	return nil
}

func topicAddress(topic string) string {
	t := strings.TrimPrefix(topic, "0x")
	if len(t) < 40 {
		return "0x" + t
	}
	return "0x" + strings.ToLower(t[len(t)-40:])
}

func shortAddr(addr string) string {
	if len(addr) <= 12 {
		return addr
	}
	return addr[:8] + "…" + addr[len(addr)-6:]
}

func parseHexUint(s string) uint64 {
	v, err := strconv.ParseUint(strings.TrimPrefix(strings.TrimSpace(s), "0x"), 16, 64)
	if err != nil {
		return 0
	}
	return v
}
