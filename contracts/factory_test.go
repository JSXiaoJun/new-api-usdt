package contracts

import (
	"context"
	"crypto/ecdsa"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient/simulated"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"new-api-usdt/internal/usdt"
)

type chainHarness struct {
	t      *testing.T
	sim    *simulated.Backend
	client simulated.Client
	keys   map[common.Address]*ecdsa.PrivateKey
}

func newHarness(t *testing.T, accounts int) (*chainHarness, []common.Address) {
	t.Helper()
	alloc := types.GenesisAlloc{}
	keys := map[common.Address]*ecdsa.PrivateKey{}
	var addrs []common.Address
	for range accounts {
		key, err := crypto.GenerateKey()
		require.NoError(t, err)
		addr := crypto.PubkeyToAddress(key.PublicKey)
		alloc[addr] = types.Account{Balance: new(big.Int).Mul(big.NewInt(1000), big.NewInt(1e18))}
		keys[addr] = key
		addrs = append(addrs, addr)
	}
	sim := simulated.NewBackend(alloc)
	t.Cleanup(func() { _ = sim.Close() })
	return &chainHarness{t: t, sim: sim, client: sim.Client(), keys: keys}, addrs
}

// send signs and mines one transaction and returns its receipt.
func (h *chainHarness) send(from common.Address, to *common.Address, value *big.Int, data []byte) *types.Receipt {
	h.t.Helper()
	ctx := context.Background()
	chainID, err := h.client.ChainID(ctx)
	require.NoError(h.t, err)
	nonce, err := h.client.PendingNonceAt(ctx, from)
	require.NoError(h.t, err)
	if value == nil {
		value = new(big.Int)
	}
	head, err := h.client.HeaderByNumber(ctx, nil)
	require.NoError(h.t, err)
	tip := big.NewInt(1e9)
	feeCap := new(big.Int).Add(new(big.Int).Mul(head.BaseFee, big.NewInt(2)), tip)
	tx := types.NewTx(&types.DynamicFeeTx{
		ChainID: chainID, Nonce: nonce, GasTipCap: tip, GasFeeCap: feeCap,
		Gas: 3_000_000, To: to, Value: value, Data: data,
	})
	signed, err := types.SignTx(tx, types.LatestSignerForChainID(chainID), h.keys[from])
	require.NoError(h.t, err)
	require.NoError(h.t, h.client.SendTransaction(ctx, signed))
	h.sim.Commit()
	receipt, err := h.client.TransactionReceipt(ctx, signed.Hash())
	require.NoError(h.t, err)
	return receipt
}

func (h *chainHarness) deploy(from common.Address, art Artifact, args ...any) common.Address {
	h.t.Helper()
	ctor, err := art.ABI.Pack("", args...)
	require.NoError(h.t, err)
	receipt := h.send(from, nil, nil, append(append([]byte{}, art.Bytecode...), ctor...))
	require.Equal(h.t, types.ReceiptStatusSuccessful, receipt.Status, "deploy %s", art.Name)
	return receipt.ContractAddress
}

func (h *chainHarness) call(to common.Address, a abi.ABI, method string, args ...any) []any {
	h.t.Helper()
	data, err := a.Pack(method, args...)
	require.NoError(h.t, err)
	out, err := h.client.CallContract(context.Background(), ethereum.CallMsg{To: &to, Data: data}, nil)
	require.NoError(h.t, err)
	values, err := a.Unpack(method, out)
	require.NoError(h.t, err)
	return values
}

func (h *chainHarness) tryCall(from, to common.Address, a abi.ABI, method string, args ...any) error {
	h.t.Helper()
	data, err := a.Pack(method, args...)
	require.NoError(h.t, err)
	_, err = h.client.CallContract(context.Background(), ethereum.CallMsg{From: from, To: &to, Data: data}, nil)
	return err
}

func (h *chainHarness) tx(from, to common.Address, a abi.ABI, method string, args ...any) *types.Receipt {
	h.t.Helper()
	data, err := a.Pack(method, args...)
	require.NoError(h.t, err)
	return h.send(from, &to, nil, data)
}

func balanceOf(h *chainHarness, token common.Address, who common.Address) *big.Int {
	return h.call(token, MustLoad("MockToken").ABI, "balanceOf", who)[0].(*big.Int)
}

func TestFactorySweepsEveryTransferConvention(t *testing.T) {
	h, accts := newHarness(t, 3)
	deployer, owner, dest := accts[0], accts[1], accts[2]
	factoryAddr := h.deploy(deployer, Factory, owner, dest, [1]byte{0xff})
	impl := h.call(factoryAddr, Factory.ABI, "implementation")[0].(common.Address)
	token := MustLoad("MockToken")

	// 0 = returns true, 1 = returns nothing (Ethereum USDT), 2 = returns false (TRON USDT)
	for mode := uint8(0); mode <= 2; mode++ {
		tokenAddr := h.deploy(deployer, token, mode)
		secret := [32]byte{1, 2, 3}
		salt := usdt.Salt(secret, uint64(100+mode))

		onChain := h.call(factoryAddr, Factory.ABI, "walletAddress", salt)[0].(common.Address)
		local := usdt.WalletAddress(0xff, factoryAddr, impl, salt)
		require.Equal(t, common.Address(local), onChain, "local CREATE2 must match the contract")

		// Deposit before the wallet exists, exactly like a real user.
		amount := big.NewInt(12_345_678)
		h.tx(deployer, tokenAddr, token.ABI, "mint", onChain, amount)
		require.Equal(t, amount, balanceOf(h, tokenAddr, onChain))

		// A stranger triggers the sweep: funds can only go to the destination.
		receipt := h.tx(deployer, factoryAddr, Factory.ABI, "sweep", salt, tokenAddr)
		require.Equal(t, types.ReceiptStatusSuccessful, receipt.Status, "mode %d", mode)
		assert.Equal(t, amount, balanceOf(h, tokenAddr, dest), "mode %d", mode)
		assert.Zero(t, balanceOf(h, tokenAddr, onChain).Sign(), "mode %d", mode)

		// A second deposit to the already deployed wallet sweeps too.
		h.tx(deployer, tokenAddr, token.ABI, "mint", onChain, big.NewInt(5))
		receipt = h.tx(deployer, factoryAddr, Factory.ABI, "sweep", salt, tokenAddr)
		require.Equal(t, types.ReceiptStatusSuccessful, receipt.Status)
		assert.Equal(t, new(big.Int).Add(amount, big.NewInt(5)), balanceOf(h, tokenAddr, dest))
	}
}

func TestOnlyFactoryCanEmptyAWallet(t *testing.T) {
	h, accts := newHarness(t, 3)
	deployer, owner, dest := accts[0], accts[1], accts[2]
	factoryAddr := h.deploy(deployer, Factory, owner, dest, [1]byte{0xff})
	token := MustLoad("MockToken")
	tokenAddr := h.deploy(deployer, token, uint8(0))
	salt := usdt.Salt([32]byte{9}, 1)

	receipt := h.tx(deployer, factoryAddr, Factory.ABI, "deployWallet", salt)
	require.Equal(t, types.ReceiptStatusSuccessful, receipt.Status)
	wallet := h.call(factoryAddr, Factory.ABI, "walletAddress", salt)[0].(common.Address)
	h.tx(deployer, tokenAddr, token.ABI, "mint", wallet, big.NewInt(100))

	forwarder := MustLoad("DepositForwarder")
	err := h.tryCall(deployer, wallet, forwarder.ABI, "flushToken", tokenAddr, deployer)
	require.Error(t, err, "a direct flush to an attacker must revert")
	assert.Equal(t, big.NewInt(100), balanceOf(h, tokenAddr, wallet))
}

func TestWrongCreate2PrefixIsRejectedBeforeFundsMove(t *testing.T) {
	h, accts := newHarness(t, 3)
	deployer, owner, dest := accts[0], accts[1], accts[2]
	// 0x41 is the TRON prefix; on an EVM chain the predicted address is wrong,
	// and deployWallet must refuse instead of publishing an unreachable address.
	factoryAddr := h.deploy(deployer, Factory, owner, dest, [1]byte{0x41})
	salt := usdt.Salt([32]byte{7}, 42)
	err := h.tryCall(deployer, factoryAddr, Factory.ABI, "deployWallet", salt)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "create2 mismatch")
}

func TestOnlyOwnerChangesDestination(t *testing.T) {
	h, accts := newHarness(t, 4)
	deployer, owner, dest, other := accts[0], accts[1], accts[2], accts[3]
	factoryAddr := h.deploy(deployer, Factory, owner, dest, [1]byte{0xff})

	require.Error(t, h.tryCall(other, factoryAddr, Factory.ABI, "setDestination", other))
	receipt := h.tx(owner, factoryAddr, Factory.ABI, "setDestination", other)
	require.Equal(t, types.ReceiptStatusSuccessful, receipt.Status)
	assert.Equal(t, other, h.call(factoryAddr, Factory.ABI, "destination")[0].(common.Address))

	// Ownership moves only when the new owner accepts.
	h.tx(owner, factoryAddr, Factory.ABI, "transferOwnership", other)
	assert.Equal(t, owner, h.call(factoryAddr, Factory.ABI, "owner")[0].(common.Address))
	require.Error(t, h.tryCall(deployer, factoryAddr, Factory.ABI, "acceptOwnership"))
	h.tx(other, factoryAddr, Factory.ABI, "acceptOwnership")
	assert.Equal(t, other, h.call(factoryAddr, Factory.ABI, "owner")[0].(common.Address))
}

func TestSweepNativeGoesToDestination(t *testing.T) {
	h, accts := newHarness(t, 3)
	deployer, owner, dest := accts[0], accts[1], accts[2]
	factoryAddr := h.deploy(deployer, Factory, owner, dest, [1]byte{0xff})
	salt := usdt.Salt([32]byte{5}, 5)
	wallet := h.call(factoryAddr, Factory.ABI, "walletAddress", salt)[0].(common.Address)

	value := big.NewInt(1e15)
	h.send(deployer, &wallet, value, nil)
	before, err := h.client.BalanceAt(context.Background(), dest, nil)
	require.NoError(t, err)
	receipt := h.tx(deployer, factoryAddr, Factory.ABI, "sweepNative", salt)
	require.Equal(t, types.ReceiptStatusSuccessful, receipt.Status)
	after, err := h.client.BalanceAt(context.Background(), dest, nil)
	require.NoError(t, err)
	assert.Equal(t, value, new(big.Int).Sub(after, before))
}
