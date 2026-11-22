package usdt

import "github.com/shopspring/decimal"

// defaultSweepCost is the native coin one sweep transaction is expected to
// burn on each chain. It is deliberately a little pessimistic: a cost that is
// too high only makes the alert fire earlier, never later.
var defaultSweepCost = map[string]string{
	ChainBSC:      "0.0002", // BNB
	ChainPolygon:  "0.03",   // POL, measured 0.028~0.041 on mainnet
	ChainArbitrum: "0.0002", // ETH
	ChainTron:     "2",      // TRX
}

// DefaultSweepCost returns the assumed native-coin cost of one sweep. Adding a
// new chain only needs an entry here (or ALERT_SWEEP_COST_<CHAIN> in .env).
func DefaultSweepCost(chain string) decimal.Decimal {
	if s, ok := defaultSweepCost[chain]; ok {
		if d, err := decimal.NewFromString(s); err == nil {
			return d
		}
	}
	return decimal.NewFromFloat(0.001)
}

// EstimateSweeps reports how many more sweeps a balance can pay for.
func EstimateSweeps(balance, cost decimal.Decimal) int {
	if cost.Sign() <= 0 {
		return 0
	}
	if balance.Sign() <= 0 {
		return 0
	}
	return int(balance.Div(cost).Floor().IntPart())
}
