// Package store keeps the scanner's durable state in SQLite: watched
// addresses, per-chain cursors, the deposit report queue and sweep history.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS addresses (
	id      INTEGER PRIMARY KEY,
	chain   TEXT NOT NULL,
	address TEXT NOT NULL,
	salt    TEXT NOT NULL,
	UNIQUE (chain, address)
);
CREATE TABLE IF NOT EXISTS cursors (
	chain TEXT PRIMARY KEY,
	block INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS deposits (
	chain      TEXT NOT NULL,
	tx_hash    TEXT NOT NULL,
	log_index  INTEGER NOT NULL,
	address    TEXT NOT NULL,
	amount     TEXT NOT NULL,
	block      INTEGER NOT NULL,
	status     TEXT NOT NULL DEFAULT 'pending',
	attempts   INTEGER NOT NULL DEFAULT 0,
	next_at    INTEGER NOT NULL DEFAULT 0,
	last_error TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	PRIMARY KEY (chain, tx_hash, log_index)
);
CREATE TABLE IF NOT EXISTS wallets (
	chain       TEXT NOT NULL,
	address     TEXT NOT NULL,
	needs_check INTEGER NOT NULL DEFAULT 0,
	checked_at  INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (chain, address)
);
CREATE TABLE IF NOT EXISTS sweeps (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	chain      TEXT NOT NULL,
	address    TEXT NOT NULL,
	amount     TEXT NOT NULL,
	tx_hash    TEXT NOT NULL,
	status     TEXT NOT NULL,
	error      TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_deposits_status ON deposits (status, next_at);
CREATE INDEX IF NOT EXISTS idx_sweeps_status ON sweeps (chain, status);
CREATE TABLE IF NOT EXISTS alerts (
	key     TEXT PRIMARY KEY,
	sent_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS chain_meta (
	chain           TEXT PRIMARY KEY,
	sweep_threshold TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS gas_state (
	chain      TEXT PRIMARY KEY,
	balance    TEXT NOT NULL,
	updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS dest_state (
	chain      TEXT PRIMARY KEY,
	address    TEXT NOT NULL,
	balance    TEXT NOT NULL,
	updated_at INTEGER NOT NULL
);
`

func Open(dir string) (*Store, error) {
	db, err := sql.Open("sqlite", filepath.Join(dir, "scanner.db")+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("init schema: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func now() int64 { return time.Now().Unix() }

type Address struct {
	ID      int
	Chain   string
	Address string
	Salt    string
}

func (s *Store) MaxAddressID(ctx context.Context) (int, error) {
	var id sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT MAX(id) FROM addresses`).Scan(&id)
	return int(id.Int64), err
}

// AddAddress returns true when the address was not known before.
func (s *Store) AddAddress(ctx context.Context, a Address) (bool, error) {
	res, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO addresses (id, chain, address, salt) VALUES (?, ?, ?, ?)`,
		a.ID, a.Chain, a.Address, a.Salt)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

func (s *Store) Addresses(ctx context.Context, chain string) ([]Address, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, chain, address, salt FROM addresses WHERE chain = ? ORDER BY id`, chain)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Address
	for rows.Next() {
		var a Address
		if err := rows.Scan(&a.ID, &a.Chain, &a.Address, &a.Salt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) AddressByWallet(ctx context.Context, chain, address string) (*Address, error) {
	var a Address
	err := s.db.QueryRowContext(ctx, `SELECT id, chain, address, salt FROM addresses WHERE chain = ? AND address = ?`, chain, address).
		Scan(&a.ID, &a.Chain, &a.Address, &a.Salt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &a, err
}

func (s *Store) Cursor(ctx context.Context, chain string) (uint64, bool, error) {
	var block int64
	err := s.db.QueryRowContext(ctx, `SELECT block FROM cursors WHERE chain = ?`, chain).Scan(&block)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return uint64(block), err == nil, err
}

func (s *Store) SetCursor(ctx context.Context, chain string, block uint64) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO cursors (chain, block) VALUES (?, ?)
		ON CONFLICT (chain) DO UPDATE SET block = excluded.block`, chain, int64(block))
	return err
}

type Deposit struct {
	Chain    string
	TxHash   string
	LogIndex int
	Address  string
	Amount   string
	Block    uint64
	Attempts int
}

// AddDeposit queues a report and flags the wallet for a sweep check.
func (s *Store) AddDeposit(ctx context.Context, d Deposit) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO deposits (chain, tx_hash, log_index, address, amount, block, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, d.Chain, d.TxHash, d.LogIndex, d.Address, d.Amount, int64(d.Block), now()); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO wallets (chain, address, needs_check) VALUES (?, ?, 1)
		ON CONFLICT (chain, address) DO UPDATE SET needs_check = 1`, d.Chain, d.Address); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) DueDeposits(ctx context.Context, limit int) ([]Deposit, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT chain, tx_hash, log_index, address, amount, block, attempts FROM deposits
		WHERE status = 'pending' AND next_at <= ? ORDER BY created_at LIMIT ?`, now(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Deposit
	for rows.Next() {
		var d Deposit
		var block int64
		if err := rows.Scan(&d.Chain, &d.TxHash, &d.LogIndex, &d.Address, &d.Amount, &block, &d.Attempts); err != nil {
			return nil, err
		}
		d.Block = uint64(block)
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Store) SettleDeposit(ctx context.Context, d Deposit, status, detail string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE deposits SET status = ?, last_error = ? WHERE chain = ? AND tx_hash = ? AND log_index = ?`,
		status, detail, d.Chain, d.TxHash, d.LogIndex)
	return err
}

// RetryDeposit backs off exponentially up to 10 minutes.
func (s *Store) RetryDeposit(ctx context.Context, d Deposit, detail string) error {
	delay := min(int64(15)<<min(d.Attempts, 6), 600)
	_, err := s.db.ExecContext(ctx, `UPDATE deposits SET attempts = attempts + 1, next_at = ?, last_error = ?
		WHERE chain = ? AND tx_hash = ? AND log_index = ?`, now()+delay, detail, d.Chain, d.TxHash, d.LogIndex)
	return err
}

func (s *Store) PendingDepositCount(ctx context.Context, chain string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM deposits WHERE chain = ? AND status = 'pending'`, chain).Scan(&n)
	return n, err
}

func (s *Store) WalletsToCheck(ctx context.Context, chain string, limit int) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT address FROM wallets WHERE chain = ? AND needs_check = 1 ORDER BY checked_at LIMIT ?`, chain, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) MarkWalletChecked(ctx context.Context, chain, address string, stillNeeded bool) error {
	flag := 0
	if stillNeeded {
		flag = 1
	}
	_, err := s.db.ExecContext(ctx, `UPDATE wallets SET needs_check = ?, checked_at = ? WHERE chain = ? AND address = ?`,
		flag, now(), chain, address)
	return err
}

type Sweep struct {
	ID        int64
	Chain     string
	Address   string
	Amount    string
	TxHash    string
	Status    string
	CreatedAt int64
}

func (s *Store) AddSweep(ctx context.Context, sw Sweep) (int64, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO sweeps (chain, address, amount, tx_hash, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, 'pending', ?, ?)`, sw.Chain, sw.Address, sw.Amount, sw.TxHash, now(), now())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) PendingSweeps(ctx context.Context, chain string) ([]Sweep, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, chain, address, amount, tx_hash, status, created_at FROM sweeps
		WHERE chain = ? AND status = 'pending' ORDER BY id`, chain)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Sweep
	for rows.Next() {
		var sw Sweep
		if err := rows.Scan(&sw.ID, &sw.Chain, &sw.Address, &sw.Amount, &sw.TxHash, &sw.Status, &sw.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, sw)
	}
	return out, rows.Err()
}

func (s *Store) FinishSweep(ctx context.Context, id int64, status, detail string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE sweeps SET status = ?, error = ?, updated_at = ? WHERE id = ?`, status, detail, now(), id)
	return err
}

// LastAlert returns when the given alert key last fired.
func (s *Store) LastAlert(ctx context.Context, key string) (time.Time, bool, error) {
	var at int64
	err := s.db.QueryRowContext(ctx, `SELECT sent_at FROM alerts WHERE key = ?`, key).Scan(&at)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	return time.Unix(at, 0), true, nil
}

// MarkAlert records that the alert key fired at the given time.
func (s *Store) MarkAlert(ctx context.Context, key string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO alerts (key, sent_at) VALUES (?, ?)
		ON CONFLICT (key) DO UPDATE SET sent_at = excluded.sent_at`, key, at.Unix())
	return err
}

// SweepStatsSince counts finished sweeps and sums their amount since a time.
func (s *Store) SweepStatsSince(ctx context.Context, since time.Time) (int, float64, error) {
	var (
		count int
		total sql.NullFloat64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*), SUM(CAST(amount AS REAL)) FROM sweeps WHERE created_at >= ?`,
		since.Unix()).Scan(&count, &total)
	if err != nil {
		return 0, 0, err
	}
	return count, total.Float64, nil
}

// DestState is the last observed balance of the collection address.
type DestState struct {
	Address   string
	Balance   string
	UpdatedAt int64
}

// DestinationState returns the stored observation for a chain.
func (s *Store) DestinationState(ctx context.Context, chain string) (DestState, bool, error) {
	var st DestState
	err := s.db.QueryRowContext(ctx,
		`SELECT address, balance, updated_at FROM dest_state WHERE chain = ?`, chain).
		Scan(&st.Address, &st.Balance, &st.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return DestState{}, false, nil
	}
	if err != nil {
		return DestState{}, false, err
	}
	return st, true, nil
}

// SetDestinationState upserts the last observation for a chain.
func (s *Store) SetDestinationState(ctx context.Context, chain string, st DestState) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO dest_state (chain, address, balance, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (chain) DO UPDATE SET address = excluded.address,
			balance = excluded.balance, updated_at = excluded.updated_at`,
		chain, st.Address, st.Balance, now())
	return err
}


// GasState is the last observed native balance of a chain's gas wallet.
type GasState struct {
	Balance   string
	UpdatedAt int64
}

// GasWalletState returns the stored gas-wallet observation for a chain.
func (s *Store) GasWalletState(ctx context.Context, chain string) (GasState, bool, error) {
	var st GasState
	err := s.db.QueryRowContext(ctx,
		`SELECT balance, updated_at FROM gas_state WHERE chain = ?`, chain).
		Scan(&st.Balance, &st.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return GasState{}, false, nil
	}
	if err != nil {
		return GasState{}, false, err
	}
	return st, true, nil
}

// SetGasWalletState upserts the last gas-wallet observation for a chain.
func (s *Store) SetGasWalletState(ctx context.Context, chain string, st GasState) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO gas_state (chain, balance, updated_at)
		VALUES (?, ?, ?)
		ON CONFLICT (chain) DO UPDATE SET balance = excluded.balance, updated_at = excluded.updated_at`,
		chain, st.Balance, now())
	return err
}

// SweepCountSince counts sweeps this scanner sent since a unix timestamp.
func (s *Store) SweepCountSince(ctx context.Context, chain string, since int64) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sweeps WHERE chain = ? AND created_at >= ?`, chain, since).Scan(&n)
	return n, err
}


// SweepThreshold returns the last sweep threshold this chain reported.
func (s *Store) SweepThreshold(ctx context.Context, chain string) (string, bool, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT sweep_threshold FROM chain_meta WHERE chain = ?`, chain).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return v, true, nil
}

// SetSweepThreshold records the threshold seen for a chain.
func (s *Store) SetSweepThreshold(ctx context.Context, chain, threshold string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO chain_meta (chain, sweep_threshold) VALUES (?, ?)
		ON CONFLICT (chain) DO UPDATE SET sweep_threshold = excluded.sweep_threshold`, chain, threshold)
	return err
}

// FlagAllWallets puts every wallet of a chain back into the check queue, so a
// changed sweep threshold re-examines balances that were already dismissed.
func (s *Store) FlagAllWallets(ctx context.Context, chain string) (int64, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE wallets SET needs_check = 1 WHERE chain = ? AND needs_check = 0`, chain)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
