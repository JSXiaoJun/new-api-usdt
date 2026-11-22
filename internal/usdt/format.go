package usdt

import "github.com/shopspring/decimal"

// FormatCoin renders an amount with two decimals for operator-facing output.
// Non-zero dust is shown as "<0.01" instead of "0.00", so a small gas balance
// never looks like an empty wallet.
func FormatCoin(d decimal.Decimal) string {
	if d.IsZero() {
		return "0.00"
	}
	if d.Abs().LessThan(decimal.NewFromFloat(0.01)) {
		if d.Sign() < 0 {
			return ">-0.01"
		}
		return "<0.01"
	}
	return d.StringFixed(2)
}
