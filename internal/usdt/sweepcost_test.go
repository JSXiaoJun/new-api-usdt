package usdt

import (
	"testing"

	"github.com/shopspring/decimal"
)

func TestEstimateSweeps(t *testing.T) {
	cases := []struct {
		balance, cost string
		want          int
	}{
		{"0.93", "0.03", 31},
		{"0.029", "0.03", 0},
		{"0.03", "0.03", 1},
		{"0", "0.03", 0},
		{"1", "0", 0},
	}
	for _, c := range cases {
		got := EstimateSweeps(decimal.RequireFromString(c.balance), decimal.RequireFromString(c.cost))
		if got != c.want {
			t.Fatalf("EstimateSweeps(%s,%s) = %d, want %d", c.balance, c.cost, got, c.want)
		}
	}
}

func TestDefaultSweepCostKnownChains(t *testing.T) {
	for _, chain := range []string{ChainBSC, ChainPolygon, ChainArbitrum, ChainTron} {
		if DefaultSweepCost(chain).Sign() <= 0 {
			t.Fatalf("%s has no positive default cost", chain)
		}
	}
}
