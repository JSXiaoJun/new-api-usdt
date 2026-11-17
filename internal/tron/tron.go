// Package tron builds, checks, signs and broadcasts TRON transactions through
// the TronGrid HTTP API. Nodes return unsigned transactions; every one is
// checked against the intended call before the local key signs it.
package tron

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/crypto"

	"new-api-usdt/internal/usdt"
)

type Client struct {
	api    string
	apiKey string
	key    *ecdsa.PrivateKey
	owner  [20]byte
	http   *http.Client
}

func New(api, apiKey string, key *ecdsa.PrivateKey) *Client {
	return &Client{
		api: strings.TrimRight(api, "/"), apiKey: apiKey, key: key,
		owner: crypto.PubkeyToAddress(key.PublicKey), http: &http.Client{Timeout: 30 * time.Second},
	}
}

// Owner is the signing account as a 20-byte address.
func (c *Client) Owner() [20]byte { return c.owner }

func hex41(a [20]byte) string { return "41" + hex.EncodeToString(a[:]) }

func (c *Client) post(ctx context.Context, path string, body any, out any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.api+path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("TRON-PRO-API-KEY", c.apiKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return errors.New("TRON API request failed")
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("TRON API %s: HTTP %d", path, resp.StatusCode)
	}
	return json.Unmarshal(data, out)
}

func decodeMessage(s string) string {
	if b, err := hex.DecodeString(s); err == nil {
		return string(b)
	}
	return s
}

type unsigned map[string]json.RawMessage

// checkedTxID verifies txID == sha256(raw_data_hex), that every expected byte
// string appears in the bytes being signed, and the contract type.
func checkedTxID(tx unsigned, contractType string, maxFee int64, expected ...[]byte) (string, []byte, error) {
	var txID, rawHex string
	if err := json.Unmarshal(tx["txID"], &txID); err != nil {
		return "", nil, errors.New("unsigned transaction has no txID")
	}
	if err := json.Unmarshal(tx["raw_data_hex"], &rawHex); err != nil {
		return "", nil, errors.New("unsigned transaction has no raw_data_hex")
	}
	rawBytes, err := hex.DecodeString(rawHex)
	if err != nil {
		return "", nil, errors.New("invalid raw_data_hex")
	}
	sum := sha256.Sum256(rawBytes)
	if hex.EncodeToString(sum[:]) != strings.ToLower(txID) {
		return "", nil, errors.New("txID does not match raw_data_hex")
	}
	for _, want := range expected {
		if !bytes.Contains(rawBytes, want) {
			return "", nil, errors.New("unsigned transaction does not contain the requested call")
		}
	}
	var raw struct {
		Contract []struct {
			Type      string `json:"type"`
			Parameter struct {
				Value struct {
					CallValue int64 `json:"call_value"`
				} `json:"value"`
			} `json:"parameter"`
		} `json:"contract"`
		FeeLimit int64 `json:"fee_limit"`
	}
	if err := json.Unmarshal(tx["raw_data"], &raw); err != nil {
		return "", nil, errors.New("invalid raw_data")
	}
	if len(raw.Contract) != 1 || raw.Contract[0].Type != contractType || raw.Contract[0].Parameter.Value.CallValue != 0 || raw.FeeLimit > maxFee {
		return "", nil, errors.New("unsigned transaction is not the requested call")
	}
	return strings.ToLower(txID), sum[:], nil
}

func (c *Client) signAndBroadcast(ctx context.Context, tx unsigned, digest []byte) error {
	sig, err := crypto.Sign(digest, c.key)
	if err != nil {
		return err
	}
	sig[64] += 27
	signed := map[string]any{}
	for k, v := range tx {
		signed[k] = v
	}
	signed["signature"] = []string{hex.EncodeToString(sig)}
	var out struct {
		Result  bool   `json:"result"`
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := c.post(ctx, "/wallet/broadcasttransaction", signed, &out); err != nil {
		return err
	}
	if !out.Result {
		return fmt.Errorf("broadcast refused: %s %s", out.Code, decodeMessage(out.Message))
	}
	return nil
}

// Trigger calls contract with calldata (selector included) and returns the
// 0x-prefixed txid.
func (c *Client) Trigger(ctx context.Context, contract [20]byte, signature string, calldata []byte, feeLimit int64) (string, error) {
	var built struct {
		Result struct {
			Result  bool   `json:"result"`
			Message string `json:"message"`
		} `json:"result"`
		Transaction unsigned `json:"transaction"`
	}
	err := c.post(ctx, "/wallet/triggersmartcontract", map[string]any{
		"owner_address":     hex41(c.owner),
		"contract_address":  hex41(contract),
		"function_selector": signature,
		"parameter":         hex.EncodeToString(calldata[4:]),
		"fee_limit":         feeLimit,
		"call_value":        0,
		"visible":           false,
	}, &built)
	if err != nil {
		return "", err
	}
	if !built.Result.Result {
		return "", fmt.Errorf("triggersmartcontract refused: %s", decodeMessage(built.Result.Message))
	}
	txID, digest, err := checkedTxID(built.Transaction, "TriggerSmartContract", feeLimit, c.owner[:], contract[:], calldata)
	if err != nil {
		return "", err
	}
	if err := c.signAndBroadcast(ctx, built.Transaction, digest); err != nil {
		return "", err
	}
	return "0x" + txID, nil
}

// Deploy creates a contract and returns the txid and the contract address.
func (c *Client) Deploy(ctx context.Context, name, abiJSON string, bytecode, ctorArgs []byte, feeLimit int64) (string, [20]byte, error) {
	var contract [20]byte
	var raw unsigned
	err := c.post(ctx, "/wallet/deploycontract", map[string]any{
		"owner_address":                 hex41(c.owner),
		"abi":                           abiJSON,
		"bytecode":                      hex.EncodeToString(bytecode),
		"parameter":                     hex.EncodeToString(ctorArgs),
		"name":                          name,
		"fee_limit":                     feeLimit,
		"call_value":                    0,
		"consume_user_resource_percent": 100,
		"origin_energy_limit":           10_000_000,
		"visible":                       false,
	}, &raw)
	if err != nil {
		return "", contract, err
	}
	var contractHex string
	if err := json.Unmarshal(raw["contract_address"], &contractHex); err != nil {
		var refusal string
		_ = json.Unmarshal(raw["Error"], &refusal)
		return "", contract, fmt.Errorf("deploycontract refused: %s", refusal)
	}
	addrBytes, err := hex.DecodeString(strings.TrimPrefix(contractHex, "41"))
	if err != nil || len(addrBytes) != 20 {
		return "", contract, errors.New("invalid contract address in deploy response")
	}
	copy(contract[:], addrBytes)
	txID, digest, err := checkedTxID(raw, "CreateSmartContract", feeLimit, c.owner[:], bytecode, ctorArgs)
	if err != nil {
		return "", contract, err
	}
	if err := c.signAndBroadcast(ctx, raw, digest); err != nil {
		return "", contract, err
	}
	return "0x" + txID, contract, nil
}

// Base58 renders the signing account for funding.
func (c *Client) Base58() string { return usdt.TronAddress(c.owner) }
