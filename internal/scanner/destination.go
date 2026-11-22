package scanner

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/shopspring/decimal"

	"new-api-usdt/contracts"
	"new-api-usdt/internal/rpc"
	"new-api-usdt/internal/store"
	"new-api-usdt/internal/usdt"
)

// factoryDestination reads the collection address straight from the factory, so
// the monitor follows a setDestination automatically.
func factoryDestination(ctx context.Context, client *rpc.Client, factory string) (string, error) {
	m, ok := contracts.Factory.ABI.Methods["destination"]
	if !ok {
		return "", fmt.Errorf("destination method missing from ABI")
	}
	raw, err := client.EthCall(ctx, "", factory, m.ID)
	if err != nil {
		return "", err
	}
	if len(raw) < 32 {
		return "", fmt.Errorf("short destination result")
	}
	return strings.ToLower(common.BytesToAddress(raw).Hex()), nil
}

// destinationVerdict decides what changed since the last observation.
// kind is one of: init, decrease, milestone, none.
func destinationVerdict(prev decimal.Decimal, hasPrev bool, prevAddr, curAddr string, cur, step decimal.Decimal) (string, decimal.Decimal) {
	if !hasPrev || !strings.EqualFold(prevAddr, curAddr) {
		return "init", decimal.Zero
	}
	if cur.LessThan(prev) {
		return "decrease", prev.Sub(cur)
	}
	if cur.GreaterThan(prev) && step.Sign() > 0 {
		prevMilestone := prev.Div(step).Floor().Mul(step)
		curMilestone := cur.Div(step).Floor().Mul(step)
		if curMilestone.GreaterThan(prevMilestone) {
			return "milestone", curMilestone
		}
	}
	return "none", decimal.Zero
}

// checkDestination warns when the collection wallet crosses a milestone or
// loses funds. A drop is always reported: it is either the operator's own
// withdrawal or someone else spending the money.
func (s *Service) checkDestination(ctx context.Context) error {
	if !s.alerts.Enabled() {
		return nil
	}
	for chainID, chain := range s.remoteChains() {
		client := s.clients[chainID]
		if client == nil || strings.TrimSpace(chain.Factory) == "" {
			continue
		}
		dest, err := factoryDestination(ctx, client, chain.Factory)
		if err != nil {
			slog.Debug("destination monitor: read destination", "chain", chainID, "error", err)
			continue
		}
		raw, err := tokenBalance(ctx, client, chain.USDT, dest)
		if err != nil {
			slog.Debug("destination monitor: read balance", "chain", chainID, "error", err)
			continue
		}
		cur := decimal.NewFromBigInt(raw, -chain.Decimals)
		step := decimal.NewFromFloat(s.cfg.DestinationStep[chainID])

		prevState, hasPrev, err := s.store.DestinationState(ctx, chainID)
		if err != nil {
			return err
		}
		prev := decimal.Zero
		prevAddr := ""
		if hasPrev {
			prevAddr = prevState.Address
			if parsed, err := decimal.NewFromString(prevState.Balance); err == nil {
				prev = parsed
			}
		}

		kind, amount := destinationVerdict(prev, hasPrev, prevAddr, dest, cur, step)
		local, _ := usdt.Lookup(chainID)
		name := chainID
		if local.Name != "" {
			name = local.Name
		}
		const symbol = "USDT"

		switch kind {
		case "init":
			subject := fmt.Sprintf("[USDT 监控] %s 收款地址余额：%s %s", name, cur.StringFixed(2), symbol)
			body := strings.Join([]string{
				"开始监控收款地址余额（首次记录，不发余额变动告警）。",
				"",
				"网络：" + name,
				"收款地址：" + dest,
				fmt.Sprintf("当前余额：%s %s", cur.StringFixed(6), symbol),
				fmt.Sprintf("每满 %s %s 提醒一次；余额减少会立刻提醒。", step.String(), symbol),
			}, "\n")
			if err := s.alerts.Alert(ctx, "dest-init:"+chainID+":"+dest, subject, body); err != nil {
				slog.Warn("destination init alert failed", "chain", chainID, "error", err)
			}
		case "milestone":
			subject := fmt.Sprintf("[USDT 提醒] %s 收款地址余额达到 %s %s", name, cur.StringFixed(2), symbol)
			body := strings.Join([]string{
				"收款地址余额跨过了一个提醒档位。",
				"",
				"网络：" + name,
				"收款地址：" + dest,
				fmt.Sprintf("当前余额：%s %s", cur.StringFixed(6), symbol),
				fmt.Sprintf("已跨过：%s %s", amount.StringFixed(0), symbol),
				"",
				"需要提到交易所时：先给该地址充一点原生币付 gas，再用对应私钥转出。",
			}, "\n")
			if err := s.alerts.Send(ctx, subject, body); err != nil {
				slog.Warn("destination milestone alert failed", "chain", chainID, "error", err)
			}
		case "decrease":
			subject := fmt.Sprintf("[USDT 告警] %s 收款地址减少 %s %s", name, amount.StringFixed(2), symbol)
			body := strings.Join([]string{
				"收款地址的 USDT 余额变少了。如果这是你自己转账到交易所，忽略即可；否则请立刻核实。",
				"",
				"网络：" + name,
				"收款地址：" + dest,
				fmt.Sprintf("减少金额：%s %s", amount.StringFixed(6), symbol),
				fmt.Sprintf("当前余额：%s %s", cur.StringFixed(6), symbol),
			}, "\n")
			if err := s.alerts.Send(ctx, subject, body); err != nil {
				slog.Warn("destination decrease alert failed", "chain", chainID, "error", err)
			}
		}

		if err := s.store.SetDestinationState(ctx, chainID, store.DestState{
			Address: dest,
			Balance: cur.String(),
		}); err != nil {
			return err
		}
	}
	return nil
}
