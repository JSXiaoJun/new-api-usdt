package scanner

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/eth/ethconfig"
	"github.com/ethereum/go-ethereum/ethclient/simulated"
	"github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/params"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"new-api-usdt/contracts"
	"new-api-usdt/internal/config"
	"new-api-usdt/internal/newapi"
	"new-api-usdt/internal/store"
	"new-api-usdt/internal/usdt"
)

func TestSignatureMatchesNewAPI(t *testing.T) {
	// The same vector is asserted in new-api's service/usdt tests.
	got := newapi.Signature("scanner-secret", "1700000000", "POST", "/api/usdt/scanner/deposit", []byte(`{"chain":"bsc"}`))
	assert.Equal(t, "d2109300514e5e27d648b9f9b2d357a664b836ae1c485ebe683be48906667b6e", got)
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// simulatedBSC runs a chain with BSC's chain id behind a real JSON-RPC
// endpoint, with a USDT-like token placed at the official BSC USDT address.
func simulatedBSC(t *testing.T, funded ...common.Address) (*simulated.Backend, string) {
	t.Helper()
	bsc, _ := usdt.Lookup(usdt.ChainBSC)
	// MockToken's immutable mode is zero in its runtime code: transfer returns true.
	runtimeCode, err := contracts.DeployedBytecode("MockToken")
	require.NoError(t, err)

	alloc := types.GenesisAlloc{common.HexToAddress(bsc.USDT): {Code: runtimeCode, Balance: new(big.Int)}}
	for _, a := range funded {
		alloc[a] = types.Account{Balance: new(big.Int).Mul(big.NewInt(100), big.NewInt(1e18))}
	}
	port := freePort(t)
	sim := simulated.NewBackend(alloc, func(n *node.Config, e *ethconfig.Config) {
		cfg := *params.AllDevChainProtocolChanges
		cfg.ChainID = big.NewInt(int64(bsc.ChainID))
		e.Genesis.Config = &cfg
		n.HTTPHost = "127.0.0.1"
		n.HTTPPort = port
		n.HTTPModules = []string{"eth", "net", "web3"}
	})
	t.Cleanup(func() { _ = sim.Close() })
	return sim, "http://127.0.0.1:" + itoa(port)
}

func itoa(n int) string { return big.NewInt(int64(n)).String() }

func sendTx(t *testing.T, sim *simulated.Backend, key *ecdsa.PrivateKey, to *common.Address, data []byte) *types.Receipt {
	t.Helper()
	ctx := context.Background()
	client := sim.Client()
	from := crypto.PubkeyToAddress(key.PublicKey)
	chainID, err := client.ChainID(ctx)
	require.NoError(t, err)
	nonce, err := client.PendingNonceAt(ctx, from)
	require.NoError(t, err)
	price, err := client.SuggestGasPrice(ctx)
	require.NoError(t, err)
	tx := types.NewTx(&types.LegacyTx{Nonce: nonce, GasPrice: new(big.Int).Mul(price, big.NewInt(2)), Gas: 3_000_000, To: to, Data: data})
	signed, err := types.SignTx(tx, types.NewEIP155Signer(chainID), key)
	require.NoError(t, err)
	require.NoError(t, client.SendTransaction(ctx, signed))
	sim.Commit()
	receipt, err := client.TransactionReceipt(ctx, signed.Hash())
	require.NoError(t, err)
	require.Equal(t, types.ReceiptStatusSuccessful, receipt.Status)
	return receipt
}

type fakeNewAPI struct {
	mu        sync.Mutex
	t         *testing.T
	secret    string
	state     newapi.State
	reports   []map[string]any
	heartbeat int
}

func (f *fakeNewAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	want := newapi.Signature(f.secret, r.Header.Get("X-Usdt-Timestamp"), r.Method, r.URL.RequestURI(), body)
	if r.Header.Get("X-Usdt-Signature") != want {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.URL.Path {
	case "/api/usdt/scanner/state":
		_ = json.NewEncoder(w).Encode(f.state)
	case "/api/usdt/scanner/deposit":
		var report map[string]any
		_ = json.Unmarshal(body, &report)
		f.reports = append(f.reports, report)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	case "/api/usdt/scanner/heartbeat":
		f.heartbeat++
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func TestScannerReportsAndSweepsADepositOnARealChain(t *testing.T) {
	gasKey, _ := crypto.GenerateKey()
	deployerKey, _ := crypto.GenerateKey()
	owner, dest := common.HexToAddress("0x00000000000000000000000000000000000000a1"), common.HexToAddress("0x00000000000000000000000000000000000000d5")
	sim, rpcURL := simulatedBSC(t, crypto.PubkeyToAddress(gasKey.PublicKey), crypto.PubkeyToAddress(deployerKey.PublicKey))
	bsc, _ := usdt.Lookup(usdt.ChainBSC)
	usdtAddr := common.HexToAddress(bsc.USDT)
	tokenABI := contracts.MustLoad("MockToken").ABI

	ctor, err := contracts.Factory.ABI.Pack("", owner, dest, [1]byte{0xff})
	require.NoError(t, err)
	factoryAddr := sendTx(t, sim, deployerKey, nil, append(append([]byte{}, contracts.Factory.Bytecode...), ctor...)).ContractAddress
	implOut, err := sim.Client().CallContract(context.Background(), ethereum.CallMsg{To: &factoryAddr, Data: contracts.Factory.ABI.Methods["implementation"].ID}, nil)
	require.NoError(t, err)
	impl := common.BytesToAddress(implOut)
	salt := usdt.Salt([32]byte{4, 2}, 7)
	wallet := usdt.WalletAddress(0xff, factoryAddr, impl, salt)

	api := &fakeNewAPI{t: t, secret: "scanner-secret", state: newapi.State{
		Chains: []newapi.Chain{{
			ID: usdt.ChainBSC, ChainID: bsc.ChainID, Factory: usdt.Hex20(factoryAddr), Implementation: usdt.Hex20(impl),
			USDT: bsc.USDT, Decimals: bsc.Decimals, Finality: "confirmations", Confirmations: 2, SweepThreshold: "1",
		}},
		Addresses: []newapi.Address{{ID: 1, Chain: usdt.ChainBSC, Address: usdt.Hex20(wallet), Salt: usdt.Hex32(salt)}},
		NextSince: 1,
	}}
	server := httptest.NewServer(api)
	t.Cleanup(server.Close)

	st, err := store.Open(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	svc, err := New(&config.Config{
		NewAPIURL: server.URL, ScannerSecret: "scanner-secret", GasKey: gasKey, SweepEnabled: true, PollInterval: time.Second,
		Chains: map[string]config.Chain{usdt.ChainBSC: {RPC: rpcURL, MaxRange: 100, CatchupBlocks: 50, LookbackBlocks: 50}},
	}, st)
	require.NoError(t, err)
	ctx := context.Background()
	require.NoError(t, svc.CheckChains(ctx))

	// The user pays 5 USDT before the scanner has ever seen their address.
	amount := new(big.Int).Mul(big.NewInt(5), big.NewInt(1e18))
	mint, err := tokenABI.Pack("mint", common.Address(wallet), amount)
	require.NoError(t, err)
	depositTx := sendTx(t, sim, deployerKey, &usdtAddr, mint).TxHash
	for range 3 {
		sim.Commit()
	}

	require.NoError(t, svc.syncState(ctx))
	require.NoError(t, svc.scanChain(ctx, usdt.ChainBSC))
	require.NoError(t, svc.scanChain(ctx, usdt.ChainBSC), "a second pass must not duplicate the deposit")
	require.NoError(t, svc.notify(ctx))
	require.NoError(t, svc.notify(ctx))
	api.mu.Lock()
	require.Len(t, api.reports, 1)
	assert.Equal(t, "bsc", api.reports[0]["chain"])
	assert.Equal(t, depositTx.Hex(), api.reports[0]["tx_hash"])
	api.mu.Unlock()

	// The sweep sends everything to the destination; the gas wallet only pays.
	require.NoError(t, svc.sweepChain(ctx, usdt.ChainBSC))
	sim.Commit()
	require.NoError(t, svc.sweepChain(ctx, usdt.ChainBSC))
	balance := func(who common.Address) *big.Int {
		data, err := tokenABI.Pack("balanceOf", who)
		require.NoError(t, err)
		out, err := sim.Client().CallContract(ctx, ethereum.CallMsg{To: &usdtAddr, Data: data}, nil)
		require.NoError(t, err)
		return new(big.Int).SetBytes(out)
	}
	assert.Equal(t, amount, balance(dest))
	assert.Zero(t, balance(common.Address(wallet)).Sign())
	pending, err := st.PendingSweeps(ctx, usdt.ChainBSC)
	require.NoError(t, err)
	assert.Empty(t, pending)

	require.NoError(t, svc.heartbeat(ctx))
	assert.Equal(t, 1, api.heartbeat)
}
