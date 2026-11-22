package store

import (
	"context"
	"testing"
	"time"
)

func TestAlertStateRoundtrip(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	if _, ok, err := st.LastAlert(ctx, "gas-low:polygon"); err != nil || ok {
		t.Fatalf("empty store: ok=%v err=%v", ok, err)
	}
	first := time.Unix(1_700_000_000, 0)
	if err := st.MarkAlert(ctx, "gas-low:polygon", first); err != nil {
		t.Fatal(err)
	}
	got, ok, err := st.LastAlert(ctx, "gas-low:polygon")
	if err != nil || !ok || !got.Equal(first) {
		t.Fatalf("read back: %v %v %v", got, ok, err)
	}

	// Second mark must overwrite, not duplicate.
	later := first.Add(time.Hour)
	if err := st.MarkAlert(ctx, "gas-low:polygon", later); err != nil {
		t.Fatal(err)
	}
	got, _, err = st.LastAlert(ctx, "gas-low:polygon")
	if err != nil || !got.Equal(later) {
		t.Fatalf("upsert: %v %v", got, err)
	}
}

func TestSweepStatsSince(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	if _, err := st.AddSweep(ctx, Sweep{Chain: "polygon", Address: "0xa", Amount: "2", TxHash: "0x1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddSweep(ctx, Sweep{Chain: "polygon", Address: "0xb", Amount: "3.5", TxHash: "0x2"}); err != nil {
		t.Fatal(err)
	}
	count, total, err := st.SweepStatsSince(ctx, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 || total != 5.5 {
		t.Fatalf("got count=%d total=%v", count, total)
	}
	// A window that excludes both rows.
	count, _, err = st.SweepStatsSince(ctx, time.Now().Add(time.Hour))
	if err != nil || count != 0 {
		t.Fatalf("future window: count=%d err=%v", count, err)
	}
}
