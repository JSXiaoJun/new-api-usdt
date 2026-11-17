package main

import (
	"context"
	"encoding/hex"
	"math/big"
	"net"
	"strconv"
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
	"new-api-usdt/internal/usdt"
)

// Runtime code of Arachnid's deterministic deployment proxy.
const create2ProxyRuntime = "7fffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffe03601600081602082378035828234f58015156039578182fd5b8082525050506014600cf3"

func TestDeployEVMProducesTheSameFactoryAddressAndProvesTheFormula(t *testing.T) {
	deployer, err := crypto.GenerateKey()
	require.NoError(t, err)
	proxyCode, err := hex.DecodeString(create2ProxyRuntime)
	require.NoError(t, err)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := l.Addr().(*net.TCPAddr).Port
	require.NoError(t, l.Close())

	sim := simulated.NewBackend(types.GenesisAlloc{
		crypto.PubkeyToAddress(deployer.PublicKey): {Balance: new(big.Int).Mul(big.NewInt(10), big.NewInt(1e18))},
		common.HexToAddress(create2Proxy):          {Code: proxyCode, Balance: new(big.Int)},
	}, func(n *node.Config, e *ethconfig.Config) {
		cfg := *params.AllDevChainProtocolChanges
		cfg.ChainID = big.NewInt(137) // pose as Polygon
		e.Genesis.Config = &cfg
		n.HTTPHost, n.HTTPPort, n.HTTPModules = "127.0.0.1", port, []string{"eth", "net", "web3"}
	})
	t.Cleanup(func() { _ = sim.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { // mine continuously, like a real chain
		for ctx.Err() == nil {
			sim.Commit()
			time.Sleep(200 * time.Millisecond)
		}
	}()

	owner, dest := "0x00000000000000000000000000000000000000a1", "0x00000000000000000000000000000000000000d5"
	t.Setenv("DEPLOYER_PRIVATE_KEY", hex.EncodeToString(crypto.FromECDSA(deployer)))
	args := []string{"-chain", "polygon", "-rpc", "http://127.0.0.1:" + strconv.Itoa(port), "-owner", owner, "-destination", dest}
	require.NoError(t, deployEVM(args))
	require.NoError(t, deployEVM(args), "re-running is idempotent")

	ctor, err := contracts.Factory.ABI.Pack("", common.HexToAddress(owner), common.HexToAddress(dest), [1]byte{0xff})
	require.NoError(t, err)
	factory := crypto.CreateAddress2(common.HexToAddress(create2Proxy), factorySalt,
		crypto.Keccak256(append(append([]byte{}, contracts.Factory.Bytecode...), ctor...)))
	code, err := sim.Client().CodeAt(context.Background(), factory, nil)
	require.NoError(t, err)
	require.NotEmpty(t, code, "factory lives at the predicted cross-chain address")

	implRaw, err := sim.Client().CallContract(context.Background(), ethereum.CallMsg{To: &factory, Data: contracts.Factory.ABI.Methods["implementation"].ID}, nil)
	require.NoError(t, err)
	polygon, _ := usdt.Lookup(usdt.ChainPolygon)
	probeWallet := usdt.WalletAddress(polygon.Create2Prefix, factory, common.BytesToAddress(implRaw), probeSalt)
	probeCode, err := sim.Client().CodeAt(context.Background(), common.Address(probeWallet), nil)
	require.NoError(t, err)
	assert.NotEmpty(t, probeCode, "the probe wallet that new-api checks exists")
}
