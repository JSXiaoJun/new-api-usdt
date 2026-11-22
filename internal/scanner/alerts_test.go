package scanner

import (
	"context"
	"strings"
	"testing"
	"time"

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
