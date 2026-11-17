// Package rpc is a small Ethereum JSON-RPC client with endpoint failover. TRON
// nodes serve the same read methods on /jsonrpc, so it covers all chains.
package rpc

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

type Client struct {
	endpoints []string
	next      atomic.Uint32
	http      *http.Client
	headers   map[string]string
}

func New(urls string, headers map[string]string) (*Client, error) {
	var endpoints []string
	for raw := range strings.SplitSeq(urls, ",") {
		if u := strings.TrimSpace(raw); u != "" {
			endpoints = append(endpoints, u)
		}
	}
	if len(endpoints) == 0 {
		return nil, errors.New("no RPC endpoint")
	}
	return &Client{endpoints: endpoints, http: &http.Client{Timeout: 20 * time.Second}, headers: headers}, nil
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("rpc error %d: %s", e.Code, e.Message) }

// Call tries endpoints round-robin. Errors never include endpoint URLs,
// which may embed API keys.
func (c *Client) Call(ctx context.Context, method string, params []any, out any) error {
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	if err != nil {
		return err
	}
	start := int(c.next.Add(1))
	var lastErr error
	for i := range c.endpoints {
		idx := (start + i) % len(c.endpoints)
		raw, err := c.post(ctx, c.endpoints[idx], body)
		if err != nil {
			lastErr = fmt.Errorf("%s endpoint#%d: %w", method, idx+1, err)
			continue
		}
		var resp struct {
			Result json.RawMessage `json:"result"`
			Error  *rpcError       `json:"error"`
		}
		if err := json.Unmarshal(raw, &resp); err != nil {
			lastErr = fmt.Errorf("%s endpoint#%d: invalid response", method, idx+1)
			continue
		}
		if resp.Error != nil {
			lastErr = fmt.Errorf("%s endpoint#%d: %w", method, idx+1, resp.Error)
			continue
		}
		if out != nil {
			if err := json.Unmarshal(resp.Result, out); err != nil {
				lastErr = fmt.Errorf("%s endpoint#%d: decode: %w", method, idx+1, err)
				continue
			}
		}
		return nil
	}
	return lastErr
}

func (c *Client) post(ctx context.Context, endpoint string, body []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("invalid endpoint")
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("request failed")
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, errors.New("read failed")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return raw, nil
}

func ParseQuantity(s string) (uint64, error) {
	n, err := ParseBig(s)
	if err != nil || !n.IsUint64() {
		return 0, fmt.Errorf("invalid quantity %q", s)
	}
	return n.Uint64(), nil
}

func ParseBig(s string) (*big.Int, error) {
	raw, ok := strings.CutPrefix(strings.TrimSpace(s), "0x")
	if !ok {
		return nil, fmt.Errorf("invalid hex number %q", s)
	}
	if raw == "" {
		return new(big.Int), nil
	}
	n, ok := new(big.Int).SetString(raw, 16)
	if !ok {
		return nil, fmt.Errorf("invalid hex number %q", s)
	}
	return n, nil
}

func (c *Client) quantity(ctx context.Context, method string, params ...any) (uint64, error) {
	var out string
	if params == nil {
		params = []any{}
	}
	if err := c.Call(ctx, method, params, &out); err != nil {
		return 0, err
	}
	return ParseQuantity(out)
}

func (c *Client) ChainID(ctx context.Context) (uint64, error) {
	return c.quantity(ctx, "eth_chainId")
}

func (c *Client) BlockNumber(ctx context.Context) (uint64, error) {
	return c.quantity(ctx, "eth_blockNumber")
}

// TaggedBlockNumber resolves "finalized" or "safe" to a height.
func (c *Client) TaggedBlockNumber(ctx context.Context, tag string) (uint64, error) {
	var block *struct {
		Number string `json:"number"`
	}
	if err := c.Call(ctx, "eth_getBlockByNumber", []any{tag, false}, &block); err != nil {
		return 0, err
	}
	if block == nil {
		return 0, fmt.Errorf("%s block unavailable", tag)
	}
	return ParseQuantity(block.Number)
}

type Log struct {
	Address     string   `json:"address"`
	Topics      []string `json:"topics"`
	Data        string   `json:"data"`
	BlockNumber string   `json:"blockNumber"`
	TxHash      string   `json:"transactionHash"`
	LogIndex    string   `json:"logIndex"`
	Removed     bool     `json:"removed"`
}

func (c *Client) GetLogs(ctx context.Context, from, to uint64, contract string, topics [][]string) ([]Log, error) {
	filterTopics := make([]any, len(topics))
	for i, t := range topics {
		switch len(t) {
		case 0:
			filterTopics[i] = nil
		case 1:
			filterTopics[i] = t[0]
		default:
			filterTopics[i] = t
		}
	}
	var logs []Log
	err := c.Call(ctx, "eth_getLogs", []any{map[string]any{
		"fromBlock": fmt.Sprintf("0x%x", from),
		"toBlock":   fmt.Sprintf("0x%x", to),
		"address":   contract,
		"topics":    filterTopics,
	}}, &logs)
	return logs, err
}

func (c *Client) EthCall(ctx context.Context, from, to string, data []byte) ([]byte, error) {
	msg := map[string]string{"to": to, "data": "0x" + hex.EncodeToString(data)}
	if from != "" {
		msg["from"] = from
	}
	var out string
	if err := c.Call(ctx, "eth_call", []any{msg, "latest"}, &out); err != nil {
		return nil, err
	}
	return hex.DecodeString(strings.TrimPrefix(out, "0x"))
}

func (c *Client) Balance(ctx context.Context, address string) (*big.Int, error) {
	var out string
	if err := c.Call(ctx, "eth_getBalance", []any{address, "latest"}, &out); err != nil {
		return nil, err
	}
	return ParseBig(out)
}

func (c *Client) GasPrice(ctx context.Context) (*big.Int, error) {
	var out string
	if err := c.Call(ctx, "eth_gasPrice", []any{}, &out); err != nil {
		return nil, err
	}
	return ParseBig(out)
}

func (c *Client) PendingNonce(ctx context.Context, address string) (uint64, error) {
	return c.quantity(ctx, "eth_getTransactionCount", address, "pending")
}

func (c *Client) EstimateGas(ctx context.Context, from, to string, data []byte) (uint64, error) {
	return c.quantity(ctx, "eth_estimateGas", map[string]string{
		"from": from, "to": to, "data": "0x" + hex.EncodeToString(data),
	})
}

func (c *Client) SendRawTransaction(ctx context.Context, raw []byte) (string, error) {
	var hash string
	err := c.Call(ctx, "eth_sendRawTransaction", []any{"0x" + hex.EncodeToString(raw)}, &hash)
	return hash, err
}

type Receipt struct {
	Status      string `json:"status"`
	BlockNumber string `json:"blockNumber"`
	BlockHash   string `json:"blockHash"`
}

func (c *Client) Receipt(ctx context.Context, txHash string) (*Receipt, error) {
	var out *Receipt
	err := c.Call(ctx, "eth_getTransactionReceipt", []any{txHash}, &out)
	return out, err
}
