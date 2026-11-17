// Package usdt holds the facts every component must agree on: which chains
// are supported, the official USDT contract on each, and how a user's deposit
// address is derived. A wrong value here moves or credits real money, so the
// table lives in code, not in configuration.
package usdt

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/crypto"
)

type Kind string

const (
	KindEVM  Kind = "evm"
	KindTron Kind = "tron"
)

type Chain struct {
	ID       string
	Name     string
	Kind     Kind
	ChainID  uint64
	USDT     string // 0x-prefixed lowercase 20-byte hex, also for TRON
	Decimals int32
	// Create2Prefix is 0xff on EVM chains and 0x41 on TRON.
	Create2Prefix byte
	// NativeSymbol pays the sweep fee on this chain.
	NativeSymbol string
}

const (
	ChainBSC      = "bsc"
	ChainArbitrum = "arbitrum"
	ChainPolygon  = "polygon"
	ChainTron     = "tron"
)

var chains = map[string]Chain{
	ChainBSC: {
		ID: ChainBSC, Name: "BSC (BEP20)", Kind: KindEVM, ChainID: 56,
		USDT: "0x55d398326f99059ff775485246999027b3197955", Decimals: 18,
		Create2Prefix: 0xff, NativeSymbol: "BNB",
	},
	ChainArbitrum: {
		ID: ChainArbitrum, Name: "Arbitrum One (USDT0)", Kind: KindEVM, ChainID: 42161,
		USDT: "0xfd086bc7cd5c481dcc9c85ebe478a1c0b69fcbb9", Decimals: 6,
		Create2Prefix: 0xff, NativeSymbol: "ETH",
	},
	ChainPolygon: {
		ID: ChainPolygon, Name: "Polygon (PoS)", Kind: KindEVM, ChainID: 137,
		USDT: "0xc2132d05d31c914a87c6611c10748aeb04b58e8f", Decimals: 6,
		Create2Prefix: 0xff, NativeSymbol: "POL",
	},
	ChainTron: {
		// TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t
		ID: ChainTron, Name: "TRON (TRC20)", Kind: KindTron, ChainID: 728126428,
		USDT: "0xa614f803b6fd780986a42c78ec9c7f77e6ded13c", Decimals: 6,
		Create2Prefix: 0x41, NativeSymbol: "TRX",
	},
}

// ChainOrder is the display order.
var ChainOrder = []string{ChainBSC, ChainArbitrum, ChainPolygon, ChainTron}

func Lookup(id string) (Chain, bool) {
	c, ok := chains[strings.ToLower(strings.TrimSpace(id))]
	return c, ok
}

// TransferTopic is keccak256("Transfer(address,address,uint256)").
const TransferTopic = "0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef"

// cloneCode returns the EIP-1167 creation code that DepositFactory deploys.
func cloneCode(implementation [20]byte) []byte {
	prefix, _ := hex.DecodeString("3d602d80600a3d3981f3363d3d373d3d3d363d73")
	suffix, _ := hex.DecodeString("5af43d82803e903d91602b57fd5bf3")
	code := make([]byte, 0, len(prefix)+20+len(suffix))
	code = append(code, prefix...)
	code = append(code, implementation[:]...)
	return append(code, suffix...)
}

// WalletAddress mirrors DepositFactory.walletAddress.
func WalletAddress(prefix byte, factory, implementation [20]byte, salt [32]byte) [20]byte {
	codeHash := crypto.Keccak256(cloneCode(implementation))
	buf := make([]byte, 0, 1+20+32+32)
	buf = append(buf, prefix)
	buf = append(buf, factory[:]...)
	buf = append(buf, salt[:]...)
	buf = append(buf, codeHash...)
	var out [20]byte
	copy(out[:], crypto.Keccak256(buf)[12:])
	return out
}

// Salt is keccak256(secret ‖ uint256(userID)).
func Salt(secret [32]byte, userID uint64) [32]byte {
	id := make([]byte, 32)
	new(big.Int).SetUint64(userID).FillBytes(id)
	var out [32]byte
	copy(out[:], crypto.Keccak256(secret[:], id))
	return out
}

// ParseHex20 accepts a 0x-prefixed 20-byte hex address.
func ParseHex20(s string) ([20]byte, error) {
	var out [20]byte
	raw, ok := strings.CutPrefix(strings.TrimSpace(s), "0x")
	if !ok || len(raw) != 40 {
		return out, fmt.Errorf("invalid 20-byte hex address %q", s)
	}
	b, err := hex.DecodeString(raw)
	if err != nil {
		return out, fmt.Errorf("invalid 20-byte hex address %q", s)
	}
	copy(out[:], b)
	return out, nil
}

func ParseHex32(s string) ([32]byte, error) {
	var out [32]byte
	raw, ok := strings.CutPrefix(strings.TrimSpace(s), "0x")
	if !ok || len(raw) != 64 {
		return out, fmt.Errorf("invalid 32-byte hex %q", s)
	}
	b, err := hex.DecodeString(raw)
	if err != nil {
		return out, fmt.Errorf("invalid 32-byte hex %q", s)
	}
	copy(out[:], b)
	return out, nil
}

func Hex20(a [20]byte) string { return "0x" + hex.EncodeToString(a[:]) }
func Hex32(a [32]byte) string { return "0x" + hex.EncodeToString(a[:]) }

const base58Alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

// TronAddress renders a 20-byte address as a TRON base58check address (T...).
func TronAddress(a [20]byte) string {
	payload := append([]byte{0x41}, a[:]...)
	first := sha256.Sum256(payload)
	second := sha256.Sum256(first[:])
	full := append(payload, second[:4]...)

	n := new(big.Int).SetBytes(full)
	base := big.NewInt(58)
	mod := new(big.Int)
	var out []byte
	for n.Sign() > 0 {
		n.DivMod(n, base, mod)
		out = append(out, base58Alphabet[mod.Int64()])
	}
	for _, b := range full {
		if b != 0 {
			break
		}
		out = append(out, base58Alphabet[0])
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return string(out)
}

// ParseTronAddress decodes a T... base58check address to its 20-byte form.
func ParseTronAddress(s string) ([20]byte, error) {
	var out [20]byte
	n := new(big.Int)
	for _, r := range strings.TrimSpace(s) {
		idx := strings.IndexRune(base58Alphabet, r)
		if idx < 0 {
			return out, errors.New("invalid base58 character")
		}
		n.Mul(n, big.NewInt(58))
		n.Add(n, big.NewInt(int64(idx)))
	}
	full := n.Bytes()
	if len(full) != 25 || full[0] != 0x41 {
		return out, errors.New("not a TRON mainnet address")
	}
	first := sha256.Sum256(full[:21])
	second := sha256.Sum256(first[:])
	if string(second[:4]) != string(full[21:]) {
		return out, errors.New("bad TRON address checksum")
	}
	copy(out[:], full[1:21])
	return out, nil
}

// DisplayAddress renders an address the way a wallet on that chain expects.
func DisplayAddress(c Chain, a [20]byte) string {
	if c.Kind == KindTron {
		return TronAddress(a)
	}
	return checksumHex(a)
}

func checksumHex(a [20]byte) string {
	lower := hex.EncodeToString(a[:])
	hash := crypto.Keccak256([]byte(lower))
	out := []byte(lower)
	for i := range out {
		if out[i] >= 'a' && out[i] <= 'f' {
			nibble := hash[i/2]
			if i%2 == 0 {
				nibble >>= 4
			}
			if nibble&0x0f >= 8 {
				out[i] -= 'a' - 'A'
			}
		}
	}
	return "0x" + string(out)
}
