package usdt

import (
	"testing"

	"github.com/shopspring/decimal"
)

func TestFormatCoin(t *testing.T) {
	cases := map[string]string{
		"0":          "0.00",
		"2":         "2.00",
		"2.005":     "2.01",
		"0.004":     "<0.01",
		"0.999":     "1.00",
		"1234.5678": "1234.57",
		"-0.004":    ">-0.01",
	}
	for in, want := range cases {
		got := FormatCoin(decimal.RequireFromString(in))
		if got != want {
			t.Fatalf("FormatCoin(%s) = %s, want %s", in, got, want)
		}
	}
}
