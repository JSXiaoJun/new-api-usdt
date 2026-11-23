// usdt-deploy deploys DepositFactory and proves its address formula on chain.
//
//	usdt-deploy new-key
//	usdt-deploy wallet                      (prints the address of DEPLOYER_PRIVATE_KEY or GAS_PRIVATE_KEY)
//	usdt-deploy evm  -chain bsc -rpc URL -owner 0x.. -destination 0x..
//	usdt-deploy tron -rpc https://api.trongrid.io/jsonrpc -api https://api.trongrid.io -owner T.. -destination T..
//	usdt-deploy set-destination -chain polygon -rpc URL -factory 0x.. -destination 0x..   (OWNER_PRIVATE_KEY)
//	usdt-deploy transfer-owner  -chain polygon -rpc URL -factory 0x.. -new-owner 0x..     (OWNER_PRIVATE_KEY)
//	usdt-deploy accept-ownership -chain polygon -rpc URL -factory 0x..                    (NEW owner key)
//	usdt-deploy send -chain bsc -rpc URL -to 0x.. -amount 0.0002                          (DEPLOYER_PRIVATE_KEY)
//
// The deployer key only pays deployment fees; it never controls funds. The
// owner (your cold or hardware wallet) is the only account that can change
// the destination later.
package main

import (
	"context"
	"crypto/ecdsa"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"math/big"
	"os"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/shopspring/decimal"

	"new-api-usdt/contracts"
	"new-api-usdt/internal/rpc"
	"new-api-usdt/internal/tron"
	"new-api-usdt/internal/usdt"
)

// Arachnid's deterministic deployment proxy, present at this address on BSC,
// Polygon and Arbitrum. Deploying through it gives the factory, and therefore
// every user's deposit address, the same 0x address on all EVM chains.
const create2Proxy = "0x4e59b44847b379578588920cA78FbF26c0B4956C"

var (
	factorySalt = crypto.Keccak256Hash([]byte("new-api-usdt:factory:v1"))
	// probeSalt must match new-api's usdt.ProbeSalt.
	probeSalt = crypto.Keccak256Hash([]byte("new-api-usdt:probe"))
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: usdt-deploy new-key | wallet | evm | tron (see source header)")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "new-key":
		err = newKey()
	case "wallet":
		err = wallet()
	case "evm":
		err = deployEVM(os.Args[2:])
	case "tron":
		err = deployTron(os.Args[2:])
	case "send":
		err = sendNative(os.Args[2:])
	case "set-destination", "transfer-owner", "accept-ownership", "balance":
		err = factoryAdmin(os.Args[1], os.Args[2:])
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func printAccount(key *ecdsa.PrivateKey) {
	addr := crypto.PubkeyToAddress(key.PublicKey)
	fmt.Println("EVM address  :", addr.Hex())
	fmt.Println("TRON address :", usdt.TronAddress(addr))
}

func newKey() error {
	key, err := crypto.GenerateKey()
	if err != nil {
		return err
	}
	fmt.Println("private key  :", hex.EncodeToString(crypto.FromECDSA(key)))
	printAccount(key)
	fmt.Println("Store the private key only in the scanner's .env (GAS_PRIVATE_KEY) and fund each chain with a few dollars of fees.")
	return nil
}

func loadKey(names ...string) (*ecdsa.PrivateKey, error) {
	for _, name := range names {
		if raw := strings.TrimSpace(os.Getenv(name)); raw != "" {
			key, err := crypto.HexToECDSA(strings.TrimPrefix(raw, "0x"))
			if err != nil {
				return nil, fmt.Errorf("%s is not a valid private key", name)
			}
			return key, nil
		}
	}
	return nil, fmt.Errorf("set %s", strings.Join(names, " or "))
}

func wallet() error {
	key, err := loadKey("DEPLOYER_PRIVATE_KEY", "GAS_PRIVATE_KEY")
	if err != nil {
		return err
	}
	printAccount(key)
	return nil
}

func waitReceipt(ctx context.Context, client *rpc.Client, hash string) (*rpc.Receipt, error) {
	for {
		receipt, err := client.Receipt(ctx, hash)
		if err == nil && receipt != nil {
			if receipt.Status != "0x1" {
				return receipt, fmt.Errorf("transaction %s reverted", hash)
			}
			return receipt, nil
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("transaction %s not mined: %w", hash, ctx.Err())
		case <-time.After(3 * time.Second):
		}
	}
}

func hasCode(ctx context.Context, client *rpc.Client, address string) (bool, error) {
	var code string
	if err := client.Call(ctx, "eth_getCode", []any{address, "latest"}, &code); err != nil {
		return false, err
	}
	return strings.TrimPrefix(code, "0x") != "", nil
}

func sendEVM(ctx context.Context, client *rpc.Client, chainID uint64, key *ecdsa.PrivateKey, to common.Address, data []byte) (string, error) {
	return sendValue(ctx, client, chainID, key, to, nil, data)
}

// sendValue signs and broadcasts a transaction, optionally moving native coin.
func sendValue(ctx context.Context, client *rpc.Client, chainID uint64, key *ecdsa.PrivateKey, to common.Address, value *big.Int, data []byte) (string, error) {
	if value == nil {
		value = big.NewInt(0)
	}
	from := strings.ToLower(crypto.PubkeyToAddress(key.PublicKey).Hex())
	price, err := client.GasPrice(ctx)
	if err != nil {
		return "", err
	}
	price = new(big.Int).Div(new(big.Int).Mul(price, big.NewInt(110)), big.NewInt(100))
	gas, err := client.EstimateGas(ctx, from, strings.ToLower(to.Hex()), data)
	if err != nil {
		return "", fmt.Errorf("estimate gas: %w", err)
	}
	nonce, err := client.PendingNonce(ctx, from)
	if err != nil {
		return "", err
	}
	tx := types.NewTx(&types.LegacyTx{Nonce: nonce, GasPrice: price, Gas: gas * 5 / 4, To: &to, Value: value, Data: data})
	signed, err := types.SignTx(tx, types.NewEIP155Signer(new(big.Int).SetUint64(chainID)), key)
	if err != nil {
		return "", err
	}
	raw, err := signed.MarshalBinary()
	if err != nil {
		return "", err
	}
	if _, err := client.SendRawTransaction(ctx, raw); err != nil {
		return "", err
	}
	return strings.ToLower(signed.Hash().Hex()), nil
}

func deployEVM(args []string) error {
	fs := flag.NewFlagSet("evm", flag.ExitOnError)
	chainID := fs.String("chain", "", "bsc | arbitrum | polygon")
	rpcURL := fs.String("rpc", "", "RPC URL of the chain")
	ownerFlag := fs.String("owner", "", "owner (cold/hardware wallet) 0x address")
	destFlag := fs.String("destination", "", "where swept USDT goes, 0x address")
	_ = fs.Parse(args)
	chain, ok := usdt.Lookup(*chainID)
	if !ok || chain.Kind != usdt.KindEVM {
		return errors.New("-chain must be bsc, arbitrum or polygon")
	}
	if !common.IsHexAddress(*ownerFlag) || !common.IsHexAddress(*destFlag) {
		return errors.New("-owner and -destination must be 0x addresses")
	}
	key, err := loadKey("DEPLOYER_PRIVATE_KEY")
	if err != nil {
		return err
	}
	client, err := rpc.New(*rpcURL, nil)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if got, err := client.ChainID(ctx); err != nil || got != chain.ChainID {
		return fmt.Errorf("RPC chain id %d (err %v) does not match %s (%d)", got, err, chain.ID, chain.ChainID)
	}
	if ok, err := hasCode(ctx, client, create2Proxy); err != nil || !ok {
		return fmt.Errorf("deterministic deployment proxy not found on %s (err %v)", chain.ID, err)
	}

	ctor, err := contracts.Factory.ABI.Pack("", common.HexToAddress(*ownerFlag), common.HexToAddress(*destFlag), [1]byte{0xff})
	if err != nil {
		return err
	}
	initCode := append(append([]byte{}, contracts.Factory.Bytecode...), ctor...)
	factory := crypto.CreateAddress2(common.HexToAddress(create2Proxy), factorySalt, crypto.Keccak256(initCode))
	fmt.Println("factory address:", factory.Hex())

	deployed, err := hasCode(ctx, client, factory.Hex())
	if err != nil {
		return err
	}
	if deployed {
		fmt.Println("factory already deployed on", chain.ID)
	} else {
		hash, err := sendEVM(ctx, client, chain.ChainID, key, common.HexToAddress(create2Proxy), append(factorySalt.Bytes(), initCode...))
		if err != nil {
			return fmt.Errorf("deploy factory: %w", err)
		}
		fmt.Println("deploy tx:", hash)
		if _, err := waitReceipt(ctx, client, hash); err != nil {
			return err
		}
		if ok, err := hasCode(ctx, client, factory.Hex()); err != nil || !ok {
			return fmt.Errorf("factory has no code after deployment (err %v)", err)
		}
	}
	return probe(ctx, client, chain, factory, func(data []byte) (string, error) {
		return sendEVM(ctx, client, chain.ChainID, key, factory, data)
	})
}

// probe deploys the wallet for probeSalt for real. DepositFactory reverts
// unless the deployed address equals its prediction, so success proves the
// address formula on this chain; new-api's verification accepts it.
func probe(ctx context.Context, client *rpc.Client, chain usdt.Chain, factory common.Address, send func([]byte) (string, error)) error {
	implRaw, err := client.EthCall(ctx, "", strings.ToLower(factory.Hex()), contracts.Factory.ABI.Methods["implementation"].ID)
	if err != nil || len(implRaw) != 32 {
		return fmt.Errorf("read implementation: %v", err)
	}
	impl := common.BytesToAddress(implRaw)
	probeWallet := usdt.WalletAddress(chain.Create2Prefix, factory, impl, probeSalt)
	fmt.Println("implementation :", usdt.DisplayAddress(chain, impl))
	exists, err := hasCode(ctx, client, usdt.Hex20(probeWallet))
	if err != nil {
		return err
	}
	if !exists {
		data, err := contracts.Factory.ABI.Pack("deployWallet", [32]byte(probeSalt))
		if err != nil {
			return err
		}
		hash, err := send(data)
		if err != nil {
			return fmt.Errorf("probe deployWallet: %w", err)
		}
		fmt.Println("probe tx:", hash)
		if _, err := waitReceipt(ctx, client, hash); err != nil {
			return err
		}
		if exists, err = hasCode(ctx, client, usdt.Hex20(probeWallet)); err != nil || !exists {
			return fmt.Errorf("probe wallet missing after deployment (err %v): do NOT enable this chain", err)
		}
	}
	destRaw, err := client.EthCall(ctx, "", strings.ToLower(factory.Hex()), contracts.Factory.ABI.Methods["destination"].ID)
	if err == nil && len(destRaw) == 32 {
		fmt.Println("destination    :", usdt.DisplayAddress(chain, common.BytesToAddress(destRaw)))
	}
	fmt.Println("probe wallet   :", usdt.DisplayAddress(chain, probeWallet), "(address formula verified)")
	fmt.Println()
	fmt.Println("Next: in new-api, USDT settings →", chain.Name, "→ factory address", usdt.DisplayAddress(chain, factory), "→ Verify.")
	return nil
}

func deployTron(args []string) error {
	fs := flag.NewFlagSet("tron", flag.ExitOnError)
	rpcURL := fs.String("rpc", "https://api.trongrid.io/jsonrpc", "TRON JSON-RPC URL")
	api := fs.String("api", "https://api.trongrid.io", "TRON HTTP API URL")
	ownerFlag := fs.String("owner", "", "owner (cold/hardware wallet) T address")
	destFlag := fs.String("destination", "", "where swept USDT goes, T address")
	feeLimit := fs.Int64("fee-limit-trx", 1000, "maximum TRX burned for deployment")
	_ = fs.Parse(args)
	chain, _ := usdt.Lookup(usdt.ChainTron)
	owner, err := usdt.ParseTronAddress(*ownerFlag)
	if err != nil {
		return fmt.Errorf("-owner: %w", err)
	}
	dest, err := usdt.ParseTronAddress(*destFlag)
	if err != nil {
		return fmt.Errorf("-destination: %w", err)
	}
	key, err := loadKey("DEPLOYER_PRIVATE_KEY")
	if err != nil {
		return err
	}
	headers := map[string]string{}
	if apiKey := os.Getenv("TRON_API_KEY"); apiKey != "" {
		headers["TRON-PRO-API-KEY"] = apiKey
	}
	client, err := rpc.New(*rpcURL, headers)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if got, err := client.ChainID(ctx); err != nil || got != chain.ChainID {
		return fmt.Errorf("RPC chain id %d (err %v) is not TRON mainnet (%d)", got, err, chain.ChainID)
	}
	tc := tron.New(*api, os.Getenv("TRON_API_KEY"), key)
	ctor, err := contracts.Factory.ABI.Pack("", common.Address(owner), common.Address(dest), [1]byte{0x41})
	if err != nil {
		return err
	}
	abiJSON, err := contracts.ABIJSON("DepositFactory")
	if err != nil {
		return err
	}
	fee := *feeLimit * 1_000_000
	hash, factory, err := tc.Deploy(ctx, "DepositFactory", abiJSON, contracts.Factory.Bytecode, ctor, fee)
	if err != nil {
		return err
	}
	fmt.Println("deploy tx:", hash)
	fmt.Println("factory address:", usdt.TronAddress(factory))
	if _, err := waitReceipt(ctx, client, hash); err != nil {
		return err
	}
	return probe(ctx, client, chain, factory, func(data []byte) (string, error) {
		return tc.Trigger(ctx, factory, "deployWallet(bytes32)", data, 100_000_000)
	})
}

// sendNative moves native coin (gas money) from the deployer wallet to another
// address, so a single exchange withdrawal can fund the owner keys too.
func sendNative(args []string) error {
	fs := flag.NewFlagSet("send", flag.ExitOnError)
	chainID := fs.String("chain", "", "bsc | arbitrum | polygon")
	rpcURL := fs.String("rpc", "", "RPC URL of the chain")
	toFlag := fs.String("to", "", "recipient address")
	amountFlag := fs.String("amount", "", "amount of native coin, e.g. 0.0002")
	_ = fs.Parse(args)
	chain, ok := usdt.Lookup(*chainID)
	if !ok || chain.Kind != usdt.KindEVM {
		return errors.New("-chain must be bsc, arbitrum or polygon")
	}
	if *rpcURL == "" {
		return errors.New("-rpc is required")
	}
	to, err := usdt.ParseHex20(*toFlag)
	if err != nil {
		return fmt.Errorf("-to: %w", err)
	}
	amount, err := decimal.NewFromString(*amountFlag)
	if err != nil || !amount.IsPositive() {
		return errors.New("-amount must be a positive number")
	}
	key, err := loadKey("DEPLOYER_PRIVATE_KEY")
	if err != nil {
		return err
	}
	client, err := rpc.New(*rpcURL, nil)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	from := crypto.PubkeyToAddress(key.PublicKey)
	hash, err := sendValue(ctx, client, chain.ChainID, key, to, amount.Shift(18).BigInt(), nil)
	if err != nil {
		return err
	}
	fmt.Println("from        :", strings.ToLower(from.Hex()))
	fmt.Println("to          :", usdt.DisplayAddress(chain, to))
	fmt.Printf("amount      : %s %s\n", amount.String(), chain.NativeSymbol)
	fmt.Println("tx          :", hash)
	return nil
}