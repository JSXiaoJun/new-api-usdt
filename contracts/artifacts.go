// Package contracts embeds the compiled DepositFactory artifacts. Rebuild them
// with `npm run build` in this directory after editing the Solidity sources.
package contracts

import (
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ethereum/go-ethereum/accounts/abi"
)

//go:embed build/*.json
var buildFS embed.FS

type Artifact struct {
	Name     string
	ABI      abi.ABI
	Bytecode []byte
}

func Load(name string) (Artifact, error) {
	raw, err := buildFS.ReadFile("build/" + name + ".json")
	if err != nil {
		return Artifact{}, err
	}
	var file struct {
		ABI      json.RawMessage `json:"abi"`
		Bytecode string          `json:"bytecode"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		return Artifact{}, fmt.Errorf("decode %s artifact: %w", name, err)
	}
	parsed, err := abi.JSON(strings.NewReader(string(file.ABI)))
	if err != nil {
		return Artifact{}, fmt.Errorf("parse %s abi: %w", name, err)
	}
	code, err := hex.DecodeString(strings.TrimPrefix(file.Bytecode, "0x"))
	if err != nil {
		return Artifact{}, fmt.Errorf("decode %s bytecode: %w", name, err)
	}
	return Artifact{Name: name, ABI: parsed, Bytecode: code}, nil
}

// DeployedBytecode returns a contract's runtime code (tests place it in genesis).
func DeployedBytecode(name string) ([]byte, error) {
	raw, err := buildFS.ReadFile("build/" + name + ".json")
	if err != nil {
		return nil, err
	}
	var file struct {
		DeployedBytecode string `json:"deployedBytecode"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		return nil, err
	}
	return hex.DecodeString(strings.TrimPrefix(file.DeployedBytecode, "0x"))
}

// ABIJSON returns a contract's ABI as JSON text (TRON deploycontract wants it).
func ABIJSON(name string) (string, error) {
	raw, err := buildFS.ReadFile("build/" + name + ".json")
	if err != nil {
		return "", err
	}
	var file struct {
		ABI json.RawMessage `json:"abi"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		return "", err
	}
	return string(file.ABI), nil
}

func MustLoad(name string) Artifact {
	a, err := Load(name)
	if err != nil {
		panic(err)
	}
	return a
}

// Factory is the DepositFactory ABI used by the scanner, the deploy tool and tests.
var Factory = MustLoad("DepositFactory")
