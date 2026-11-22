package scanner

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"new-api-usdt/internal/config"
	"new-api-usdt/internal/notify"
	"new-api-usdt/internal/store"
)

type captureSender struct {
	subjects []string
	bodies   []string
}

func (c *captureSender) Send(_ context.Context, subject, body string) error {
	c.subjects = append(c.subjects, subject)
	c.bodies = append(c.bodies, body)
	return nil
}

func TestTopicAddress(t *testing.T) {
	topic := "0x00000000000000000000000075fffa62590e434243066e0e68fcc451c53b99c4"
	if got, want := topicAddress(topic), "0x75fffa62590e434243066e0e68fcc451c53b99c4"; got != want {
		t.Fatalf("topicAddress = %s, want %s", got, want)
	}
}

func TestParseHexUint(t *testing.T) {
	cases := map[string]uint64{
		"0x10":  16,
		"0x0":   0,
		"":      0,
		"0xZZZ": 0,
	}
	for in, want := range cases {
		if got := parseHexUint(in); got != want {
			t.Fatalf("parseHexUint(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestDestinationChangeAlertFlagsUnexpectedAddress(t *testing.T) {
	sender := &captureSender{}
	s := &Service{
		cfg: &config.Config{
			ExpectedDestination: map[string]string{"polygon": "0xd6138334c54e655d037f17a54e88f9cd468ff0b0"},
		},
		alerts: notify.NewManager(sender, nil, 0),
	}
	s.alertDestinationChange(context.Background(), "polygon", "0x2c22",
		12345, "0xabc", "0x75fffa62590e434243066e0e68fcc451c53b99c4",
		"0x1111111111111111111111111111111111111111")

	if len(sender.subjects) != 1 {
		t.Fatalf("expected one mail, got %d", len(sender.subjects))
	}
	if !strings.Contains(sender.subjects[0], "收款地址被修改") {
		t.Fatalf("subject: %s", sender.subjects[0])
	}
	if !strings.Contains(sender.bodies[0], "不一致") {
		t.Fatalf("body should flag the mismatch: %s", sender.bodies[0])
	}
	if !strings.Contains(sender.bodies[0], "polygonscan.com/tx/0xabc") {
		t.Fatalf("body should link the transaction: %s", sender.bodies[0])
	}
}

func TestSweepFailureStreakAlertsOnThirdError(t *testing.T) {
	sender := &captureSender{}
	s := &Service{
		cfg:    &config.Config{},
		stats:  map[string]*chainState{"polygon": {}},
		alerts: notify.NewManager(sender, nil, 0),
	}
	ctx := context.Background()
	err := context.DeadlineExceeded

	s.noteSweepResult(ctx, "polygon", err)
	s.noteSweepResult(ctx, "polygon", err)
	if len(sender.subjects) != 0 {
		t.Fatalf("must stay quiet before the third failure")
	}
	s.noteSweepResult(ctx, "polygon", err)
	if len(sender.subjects) != 1 {
		t.Fatalf("expected alert on third failure, got %d", len(sender.subjects))
	}
	// A success resets the streak.
	s.noteSweepResult(ctx, "polygon", nil)
	s.noteSweepResult(ctx, "polygon", err)
	s.noteSweepResult(ctx, "polygon", err)
	if len(sender.subjects) != 1 {
		t.Fatalf("streak was not reset")
	}
}

func TestDestinationVerdict(t *testing.T) {
	d := decimal.RequireFromString
	addr := "0xd6138334c54e655d037f17a54e88f9cd468ff0b0"
	step := d("100")

	cases := []struct {
		name     string
		prev     decimal.Decimal
		hasPrev  bool
		prevAddr string
		cur      decimal.Decimal
		want     string
	}{
		{"first observation", d("0"), false, "", d("2"), "init"},
		{"small increase below step", d("2"), true, addr, d("50"), "none"},
		{"crosses 100", d("95"), true, addr, d("105"), "milestone"},
		{"crosses two steps at once", d("95"), true, addr, d("250"), "milestone"},
		{"no movement", d("105"), true, addr, d("105"), "none"},
		{"decrease", d("105"), true, addr, d("5"), "decrease"},
		{"address changed", d("105"), true, "0xother", d("105"), "init"},
	}
	for _, c := range cases {
		got, _ := destinationVerdict(c.prev, c.hasPrev, c.prevAddr, addr, c.cur, step)
		if got != c.want {
			t.Fatalf("%s: got %s want %s", c.name, got, c.want)
		}
	}
}

func TestDestinationVerdictDecreaseAmount(t *testing.T) {
	addr := "0xd6138334c54e655d037f17a54e88f9cd468ff0b0"
	_, amount := destinationVerdict(decimal.RequireFromString("105"), true, addr, addr,
		decimal.RequireFromString("5"), decimal.RequireFromString("100"))
	if amount.String() != "100" {
		t.Fatalf("decrease amount = %s, want 100", amount)
	}
}

func TestDailySummaryHonoursConfiguredHour(t *testing.T) {
	sender := &captureSender{}
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	s := &Service{
		cfg:    &config.Config{DailyHour: time.Now().Hour(), DailyMinute: time.Now().Minute()},
		store:  st,
		alerts: notify.NewManager(sender, st, 0),
	}
	if err := s.dailySummary(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(sender.subjects) != 1 {
		t.Fatalf("expected daily mail, got %d", len(sender.subjects))
	}
	// The same day must not send twice.
	if err := s.dailySummary(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(sender.subjects) != 1 {
		t.Fatalf("daily summary repeated: %d", len(sender.subjects))
	}
}

func TestGasVerdict(t *testing.T) {
	cases := []struct {
		bal, cost  string
		floor      float64
		limit      int
		wantLow    bool
		wantLeft   int
	}{
		{"0.93", "0.03", 0.2, 30, false, 31},
		{"0.90", "0.03", 0.2, 30, true, 30},
		{"0.10", "0.03", 0.2, 30, true, 3},
		{"0.15", "0.03", 0.2, 30, true, 5},
		{"0", "0.03", 0.2, 30, true, 0},
		{"2", "0.03", 0.2, 30, false, 66},
	}
	for _, c := range cases {
		low, left := gasVerdict(decimal.RequireFromString(c.bal), decimal.RequireFromString(c.cost), c.floor, c.limit)
		if low != c.wantLow || left != c.wantLeft {
			t.Fatalf("gasVerdict(%s,%s) = low=%v left=%d, want low=%v left=%d",
				c.bal, c.cost, low, left, c.wantLow, c.wantLeft)
		}
	}
}
func TestGasOutflowVerdict(t *testing.T) {
	d := decimal.RequireFromString
	cost := d("0.03")
	cases := []struct {
		name    string
		prev    string
		cur     string
		sweeps  int
		want    bool
	}{
		{"no change", "1.0", "1.0", 0, false},
		{"balance grew", "1.0", "1.5", 0, false},
		{"explained by one sweep", "1.0", "0.97", 1, false},
		{"unexplained, no sweep", "1.0", "0.5", 0, true},
		{"drained despite a sweep", "1.0", "0.5", 1, true},
		{"two sweeps explained", "1.0", "0.94", 2, false},
	}
	for _, c := range cases {
		got, delta := gasOutflowVerdict(d(c.prev), d(c.cur), c.sweeps, cost)
		if got != c.want {
			t.Fatalf("%s: got %v (delta %s), want %v", c.name, got, delta, c.want)
		}
	}
}