package main

import (
	"context"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"math/big"
	"os"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/shopspring/decimal"

	"new-api-usdt/contracts"
	"new-api-usdt/internal/rpc"
	"new-api-usdt/internal/usdt"
)

// factoryAdmin runs the owner-only maintenance calls against an already
// deployed factory, signed by OWNER_PRIVATE_KEY:
//
//	set-destination    -destination 0x..   (future sweeps go to the new address)
//	transfer-owner     -new-owner   0x..   (start the two-step ownership transfer)
//	accept-ownership                       (run with the NEW owner key)
//
// The factory address never changes, so user deposit addresses stay valid.
func factoryAdmin(command string, args []string) error {
	fs := flag.NewFlagSet(command, flag.ExitOnError)
	chainID := fs.String("chain", "", "bsc | arbitrum | polygon")
	rpcURL := fs.String("rpc", "", "RPC URL of the chain")
	factoryFlag := fs.String("factory", "", "factory contract address")
	destFlag := fs.String("destination", "", "new destination address (set-destination)")
	ownerFlag := fs.String("new-owner", "", "new owner address (transfer-owner)")
	_ = fs.Parse(args)

	chain, ok := usdt.Lookup(*chainID)
	if !ok || chain.Kind != usdt.KindEVM {
		return errors.New("-chain must be bsc, arbitrum or polygon")
	}
	if !common.IsHexAddress(*factoryFlag) {
		return errors.New("-factory must be a 0x address")
	}

	var (
		data      []byte
		err       error
		nextLabel string
	)
	switch command {
	case "set-destination":
		if !common.IsHexAddress(*destFlag) {
			return errors.New("-destination must be a 0x address")
		}
		data, err = contracts.Factory.ABI.Pack("setDestination", common.HexToAddress(*destFlag))
		nextLabel = "destination"
	case "transfer-owner":
		if !common.IsHexAddress(*ownerFlag) {
			return errors.New("-new-owner must be a 0x address")
		}
		data, err = contracts.Factory.ABI.Pack("transferOwnership", common.HexToAddress(*ownerFlag))
		nextLabel = "pendingOwner"
	case "accept-ownership":
		data, err = contracts.Factory.ABI.Pack("acceptOwnership")
		nextLabel = "owner"
	case "balance":
		// Read-only: printBalances() prints and returns before any key is read.
		nextLabel = "destination"
	default:
		return fmt.Errorf("unknown factory command %q", command)
	}
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

	factory := common.HexToAddress(*factoryFlag)
	if ok, err := hasCode(ctx, client, factory.Hex()); err != nil || !ok {
		return fmt.Errorf("no factory contract at %s (err %v)", factory.Hex(), err)
	}

	fmt.Println("factory     :", usdt.DisplayAddress(chain, factory))
	if addr, err := readFactoryAddress(ctx, client, factory, "owner"); err == nil {
		fmt.Println("owner       :", usdt.DisplayAddress(chain, addr))
	}
	if addr, err := readFactoryAddress(ctx, client, factory, "pendingOwner"); err == nil && addr != (common.Address{}) {
		fmt.Println("pendingOwner:", usdt.DisplayAddress(chain, addr))
	}
	if addr, err := readFactoryAddress(ctx, client, factory, "destination"); err == nil {
		fmt.Println("destination :", usdt.DisplayAddress(chain, addr))
	}

	// `balance` is read-only: report the collection wallet and what is in it.
	if command == "balance" {
		return printBalances(ctx, client, chain, factory)
	}

	key, err := loadKey("OWNER_PRIVATE_KEY")
	if err != nil {
		return fmt.Errorf("%w (the key that currently owns the factory; state above is read-only)", err)
	}

	hash, err := sendEVM(ctx, client, chain.ChainID, key, factory, data)
	if err != nil {
		return fmt.Errorf("%s: %w", command, err)
	}
	fmt.Println("tx          :", hash)
	if _, err := waitReceipt(ctx, client, hash); err != nil {
		return err
	}
	if addr, err := readFactoryAddress(ctx, client, factory, nextLabel); err == nil {
		fmt.Println("now "+nextLabel+" :", usdt.DisplayAddress(chain, addr))
	}
	fmt.Println("done        :", command)
	return nil
}

// readFactoryAddress eth_calls a public address getter on the factory.
func readFactoryAddress(ctx context.Context, client *rpc.Client, factory common.Address, method string) (common.Address, error) {
	m, ok := contracts.Factory.ABI.Methods[method]
	if !ok {
		return common.Address{}, fmt.Errorf("no method %q in ABI", method)
	}
	raw, err := client.EthCall(ctx, "", factory.Hex(), m.ID)
	if err != nil {
		return common.Address{}, err
	}
	if len(raw) < 32 {
		return common.Address{}, fmt.Errorf("short %s result", method)
	}
	return common.BytesToAddress(raw), nil
}

// printBalances reports how much USDT the collection wallet holds, plus the
// native gas balances, all rounded to two decimals for USDT.
func printBalances(ctx context.Context, client *rpc.Client, chain usdt.Chain, factory common.Address) error {
	dest, err := readFactoryAddress(ctx, client, factory, "destination")
	if err != nil {
		return err
	}
	amount, err := erc20Balance(ctx, client, common.HexToAddress(chain.USDT), dest, chain.Decimals)
	if err != nil {
		return err
	}
	fmt.Println("USDT        :", amount.StringFixed(2), "USDT", "("+chain.Name+")")

	if native, err := client.Balance(ctx, strings.ToLower(dest.Hex())); err == nil {
		nativeAmount := decimal.NewFromBigInt(native, -18)
		fmt.Println("收款地址 gas :", nativeAmount.StringFixed(4), chain.NativeSymbol)
	}
	if raw := strings.TrimSpace(os.Getenv("GAS_PRIVATE_KEY")); raw != "" {
		if key, err := crypto.HexToECDSA(strings.TrimPrefix(raw, "0x")); err == nil {
			gasAddr := crypto.PubkeyToAddress(key.PublicKey)
			if native, err := client.Balance(ctx, strings.ToLower(gasAddr.Hex())); err == nil {
				fmt.Println("Gas 钱包     :", usdt.DisplayAddress(chain, gasAddr),
					decimal.NewFromBigInt(native, -18).StringFixed(4), chain.NativeSymbol)
			}
		}
	}
	return nil
}

// erc20Balance calls balanceOf(holder) and scales by the token decimals.
func erc20Balance(ctx context.Context, client *rpc.Client, token, holder common.Address, decimals int32) (decimal.Decimal, error) {
	data := make([]byte, 4+32)
	copy(data[:4], mustDecodeHex("70a08231"))
	copy(data[4:], common.LeftPadBytes(holder.Bytes(), 32))
	raw, err := client.EthCall(ctx, "", token.Hex(), data)
	if err != nil {
		return decimal.Zero, err
	}
	if len(raw) < 32 {
		return decimal.Zero, fmt.Errorf("short balanceOf result")
	}
	return decimal.NewFromBigInt(new(big.Int).SetBytes(raw[:32]), -decimals), nil
}

func mustDecodeHex(s string) []byte {
	out, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return out
}
