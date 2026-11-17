// Package newapi talks to new-api's scanner endpoints with HMAC-signed requests.
package newapi

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type Client struct {
	base   string
	secret string
	http   *http.Client
}

func New(base, secret string) *Client {
	return &Client{base: strings.TrimRight(base, "/"), secret: secret, http: &http.Client{Timeout: 60 * time.Second}}
}

// Signature must match new-api's usdt.ScannerSignature.
func Signature(secret, timestamp, method, path string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp + "\n" + strings.ToUpper(method) + "\n" + path + "\n"))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func (c *Client) do(ctx context.Context, method, path string, body []byte) (int, []byte, error) {
	full, err := url.Parse(c.base + path)
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, full.String(), bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	// new-api signs the request URI it sees: the path plus the query string,
	// prefixed by any sub-path new-api is mounted under.
	req.Header.Set("X-Usdt-Timestamp", ts)
	req.Header.Set("X-Usdt-Signature", Signature(c.secret, ts, method, full.RequestURI(), body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	return resp.StatusCode, raw, err
}

type Chain struct {
	ID             string `json:"id"`
	ChainID        uint64 `json:"chain_id"`
	Factory        string `json:"factory"`
	Implementation string `json:"implementation"`
	USDT           string `json:"usdt"`
	Decimals       int32  `json:"decimals"`
	Finality       string `json:"finality"`
	Confirmations  int    `json:"confirmations"`
	SweepThreshold string `json:"sweep_threshold"`
}

type Address struct {
	ID      int    `json:"id"`
	Chain   string `json:"chain"`
	Address string `json:"address"`
	Salt    string `json:"salt"`
}

type State struct {
	Chains    []Chain   `json:"chains"`
	Addresses []Address `json:"addresses"`
	NextSince int       `json:"next_since"`
}

func (c *Client) State(ctx context.Context, since, limit int) (State, error) {
	var state State
	status, raw, err := c.do(ctx, http.MethodGet, fmt.Sprintf("/api/usdt/scanner/state?since=%d&limit=%d", since, limit), nil)
	if err != nil {
		return state, err
	}
	if status != http.StatusOK {
		return state, fmt.Errorf("state: HTTP %d", status)
	}
	err = json.Unmarshal(raw, &state)
	return state, err
}

// DepositResult reports whether new-api settled the report or wants a retry.
type DepositResult struct {
	Settled bool
	Status  string
	Detail  string
}

func (c *Client) ReportDeposit(ctx context.Context, chain, txHash string, logIndex int) (DepositResult, error) {
	body, _ := json.Marshal(map[string]any{"chain": chain, "tx_hash": txHash, "log_index": logIndex})
	status, raw, err := c.do(ctx, http.MethodPost, "/api/usdt/scanner/deposit", body)
	if err != nil {
		return DepositResult{}, err
	}
	var resp struct {
		Status string `json:"status"`
		Error  string `json:"error"`
	}
	_ = json.Unmarshal(raw, &resp)
	switch status {
	case http.StatusOK:
		return DepositResult{Settled: true, Status: resp.Status, Detail: resp.Error}, nil
	case http.StatusConflict:
		return DepositResult{Status: "retry", Detail: resp.Error}, nil
	default:
		return DepositResult{}, fmt.Errorf("deposit report: HTTP %d %s", status, resp.Error)
	}
}

type ChainHeartbeat struct {
	Head          uint64 `json:"head"`
	Cursor        uint64 `json:"cursor"`
	GasBalance    string `json:"gas_balance"`
	PendingSweeps int    `json:"pending_sweeps"`
	LastError     string `json:"last_error"`
}

func (c *Client) Heartbeat(ctx context.Context, version string, chains map[string]ChainHeartbeat) error {
	body, _ := json.Marshal(map[string]any{"version": version, "chains": chains})
	status, _, err := c.do(ctx, http.MethodPost, "/api/usdt/scanner/heartbeat", body)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("heartbeat: HTTP %d", status)
	}
	return nil
}
