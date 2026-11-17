// Package sweep sends DepositFactory.sweepMany transactions. The gas wallet
// only pays fees: the factory moves funds to its fixed destination no matter
// who calls it, so a stolen gas key can at worst waste the fee balance.
package sweep

import (
	"context"
	"crypto/ecdsa"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"

	"new-api-usdt/contracts"
	"new-api-usdt/internal/rpc"
	"new-api-usdt/internal/tron"
	"new-api-usdt/internal/usdt"
)

// Sender submits a sweep and reports its outcome.
type Sender interface {
	// Address is the gas wallet in the chain's 0x form.
	Address() string
	Send(ctx context.Context, factory [20]byte, salts [][32]byte, token [20]byte) (string, error)
	// Status returns done=false while the transaction is unknown or pending.
	Status(ctx context.Context, txHash string) (done bool, success bool, err error)
}

func packSweepMany(salts [][32]byte, token [20]byte) ([]byte, error) {
	return contracts.Factory.ABI.Pack("sweepMany", salts, common.Address(token))
}

type EVM struct {
	client      *rpc.Client
	chainID     *big.Int
	key         *ecdsa.PrivateKey
	from        common.Address
	maxGasPrice *big.Int
}

func NewEVM(client *rpc.Client, chainID uint64, key *ecdsa.PrivateKey, maxGasPriceWei uint64) *EVM {
	s := &EVM{client: client, chainID: new(big.Int).SetUint64(chainID), key: key, from: crypto.PubkeyToAddress(key.PublicKey)}
	if maxGasPriceWei > 0 {
		s.maxGasPrice = new(big.Int).SetUint64(maxGasPriceWei)
	}
	return s
}

func (s *EVM) Address() string { return strings.ToLower(s.from.Hex()) }

func (s *EVM) Send(ctx context.Context, factory [20]byte, salts [][32]byte, token [20]byte) (string, error) {
	data, err := packSweepMany(salts, token)
	if err != nil {
		return "", err
	}
	to := common.Address(factory)
	price, err := s.client.GasPrice(ctx)
	if err != nil {
		return "", err
	}
	// Pay 10% over the quote so the sweep is not stuck behind fee changes.
	price = new(big.Int).Div(new(big.Int).Mul(price, big.NewInt(110)), big.NewInt(100))
	if s.maxGasPrice != nil && price.Cmp(s.maxGasPrice) > 0 {
		return "", fmt.Errorf("gas price %s wei is above the configured cap", price)
	}
	gas, err := s.client.EstimateGas(ctx, s.Address(), usdt.Hex20(factory), data)
	if err != nil {
		return "", fmt.Errorf("estimate gas: %w", err)
	}
	nonce, err := s.client.PendingNonce(ctx, s.Address())
	if err != nil {
		return "", err
	}
	tx := types.NewTx(&types.LegacyTx{Nonce: nonce, GasPrice: price, Gas: gas * 5 / 4, To: &to, Data: data})
	signed, err := types.SignTx(tx, types.NewEIP155Signer(s.chainID), s.key)
	if err != nil {
		return "", err
	}
	raw, err := signed.MarshalBinary()
	if err != nil {
		return "", err
	}
	if _, err := s.client.SendRawTransaction(ctx, raw); err != nil {
		return "", err
	}
	return strings.ToLower(signed.Hash().Hex()), nil
}

func (s *EVM) Status(ctx context.Context, txHash string) (bool, bool, error) {
	receipt, err := s.client.Receipt(ctx, txHash)
	if err != nil || receipt == nil {
		return false, false, err
	}
	return true, receipt.Status == "0x1", nil
}

// Tron sends sweeps through the TRON HTTP API; see package tron for the
// checks applied before the local key signs.
type Tron struct {
	client   *rpc.Client // TRON /jsonrpc, for receipts
	api      *tron.Client
	feeLimit int64
}

func NewTron(client *rpc.Client, api, apiKey string, key *ecdsa.PrivateKey, feeLimitSun int64) *Tron {
	return &Tron{client: client, api: tron.New(api, apiKey, key), feeLimit: feeLimitSun}
}

func (s *Tron) Address() string { return usdt.Hex20(s.api.Owner()) }

// TronAddress is the gas wallet in T... form, to fund it with TRX.
func (s *Tron) TronAddress() string { return s.api.Base58() }

func (s *Tron) Send(ctx context.Context, factory [20]byte, salts [][32]byte, token [20]byte) (string, error) {
	data, err := packSweepMany(salts, token)
	if err != nil {
		return "", err
	}
	return s.api.Trigger(ctx, factory, "sweepMany(bytes32[],address)", data, s.feeLimit)
}

func (s *Tron) Status(ctx context.Context, txHash string) (bool, bool, error) {
	receipt, err := s.client.Receipt(ctx, txHash)
	if err != nil || receipt == nil {
		return false, false, err
	}
	return true, receipt.Status == "0x1", nil
}
