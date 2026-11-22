package notify

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type fakeSender struct {
	subjects []string
	bodies   []string
	err      error
}

func (f *fakeSender) Send(_ context.Context, subject, body string) error {
	if f.err != nil {
		return f.err
	}
	f.subjects = append(f.subjects, subject)
	f.bodies = append(f.bodies, body)
	return nil
}

type fakeStore struct {
	times map[string]time.Time
}

func (f *fakeStore) LastAlert(_ context.Context, key string) (time.Time, bool, error) {
	t, ok := f.times[key]
	return t, ok, nil
}

func (f *fakeStore) MarkAlert(_ context.Context, key string, at time.Time) error {
	if f.times == nil {
		f.times = map[string]time.Time{}
	}
	f.times[key] = at
	return nil
}

func TestBuildMessageEncodesSubjectAndBody(t *testing.T) {
	raw := string(buildMessage("a@qq.com", []string{"b@qq.com"}, "Gas 余额不足", "Polygon 0.18 POL"))
	if !strings.Contains(raw, "Subject: =?UTF-8?") {
		t.Fatalf("subject is not encoded: %q", raw)
	}
	if !strings.Contains(raw, "Content-Transfer-Encoding: base64") {
		t.Fatalf("body encoding header missing")
	}
	if strings.Contains(raw, "余额不足") {
		// The body must be base64 so providers cannot mangle UTF-8.
		t.Fatalf("body leaked as raw UTF-8")
	}
}

func TestManagerCooldown(t *testing.T) {
	sender := &fakeSender{}
	store := &fakeStore{}
	m := NewManager(sender, store, time.Hour)
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return now }

	ctx := context.Background()
	if err := m.Alert(ctx, "gas-low:polygon", "s1", "b1"); err != nil {
		t.Fatal(err)
	}
	if len(sender.subjects) != 1 {
		t.Fatalf("first alert not sent")
	}
	// Inside the window: skipped.
	if err := m.Alert(ctx, "gas-low:polygon", "s2", "b2"); err != nil {
		t.Fatal(err)
	}
	if len(sender.subjects) != 1 {
		t.Fatalf("cooldown not applied")
	}
	// After the window: sent again.
	now = now.Add(2 * time.Hour)
	if err := m.Alert(ctx, "gas-low:polygon", "s3", "b3"); err != nil {
		t.Fatal(err)
	}
	if len(sender.subjects) != 2 {
		t.Fatalf("alert after cooldown not sent")
	}
}

func TestManagerDisabledIsNoop(t *testing.T) {
	m := NewManager(nil, nil, time.Hour)
	if m.Enabled() {
		t.Fatal("expected disabled")
	}
	if err := m.Alert(context.Background(), "k", "s", "b"); err != nil {
		t.Fatalf("disabled manager must be a no-op, got %v", err)
	}
}

func TestManagerPropagatesSendError(t *testing.T) {
	sender := &fakeSender{err: errors.New("smtp down")}
	m := NewManager(sender, &fakeStore{}, time.Hour)
	if err := m.Alert(context.Background(), "k", "s", "b"); err == nil {
		t.Fatal("expected send error")
	}
}
