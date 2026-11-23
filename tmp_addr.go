package main

import (
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	"new-api-usdt/contracts"
)

// Throwaway helper: recompute the deterministic factory address for a given
// (owner, destination) pair so we can explain why two chains differ.
func main() {
	proxy := common.HexToAddress("0x4e59b44847b379578588920cA78FbF26c0B4956C")
	salt := crypto.Keccak256Hash([]byte("new-api-usdt:factory:v1"))
	pairs := [][3]string{
		{"旧 owner/destination（Polygon 部署时用的）", "0x46b784f33b3FA28e66DfeF3E7ffDbd95Da193A77", "0x75fffA62590e434243066E0E68fcC451C53b99c4"},
		{"新 owner/destination（BSC 刚才用的）", "0xdD894E85204BDB71c81eb70A8df15e48b0394DB6", "0xd6138334c54E655d037f17a54E88F9cD468Ff0B0"},
	}
	for _, p := range pairs {
		ctor, err := contracts.Factory.ABI.Pack("", common.HexToAddress(p[1]), common.HexToAddress(p[2]), [1]byte{0xff})
		if err != nil {
			panic(err)
		}
		initCode := append(append([]byte{}, contracts.Factory.Bytecode...), ctor...)
		addr := crypto.CreateAddress2(proxy, salt, crypto.Keccak256(initCode))
		fmt.Printf("%-38s -> %s\n", p[0], addr.Hex())
	}
}
