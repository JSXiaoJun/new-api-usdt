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

func TestFlagAllWalletsRequeuesChainOnly(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	if _, err := st.AddAddress(ctx, Address{ID: 1, Chain: "polygon", Address: "0xaa", Salt: "0x01"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddAddress(ctx, Address{ID: 2, Chain: "polygon", Address: "0xbb", Salt: "0x02"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddAddress(ctx, Address{ID: 3, Chain: "bsc", Address: "0xcc", Salt: "0x03"}); err != nil {
		t.Fatal(err)
	}
	// Simulate a deposit on each wallet, then the sweeper dismissing them.
	for _, w := range []struct{ chain, addr string }{{"polygon", "0xaa"}, {"polygon", "0xbb"}, {"bsc", "0xcc"}} {
		if err := st.AddDeposit(ctx, Deposit{Chain: w.chain, TxHash: "0x" + w.addr, LogIndex: 0, Address: w.addr, Amount: "10", Block: 1}); err != nil {
			t.Fatal(err)
		}
		if err := st.MarkWalletChecked(ctx, w.chain, w.addr, false); err != nil {
			t.Fatal(err)
		}
	}
	n, err := st.FlagAllWallets(ctx, "polygon")
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("re-queued %d polygon wallets, want 2", n)
	}
	got, err := st.WalletsToCheck(ctx, "polygon", 10)
	if err != nil || len(got) != 2 {
		t.Fatalf("wallets to check = %v err=%v", got, err)
	}
	if other, _ := st.WalletsToCheck(ctx, "bsc", 10); len(other) != 0 {
		t.Fatalf("bsc wallets should stay dismissed, got %v", other)
	}
}