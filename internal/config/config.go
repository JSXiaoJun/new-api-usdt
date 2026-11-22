// Package config reads the scanner's settings from the environment. Secrets
// (the gas wallet key, the scanner secret, RPC keys) never leave this process.
package config

import (
	"crypto/ecdsa"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/crypto"

	"new-api-usdt/internal/usdt"
)

type Chain struct {
	RPC string
	// MaxRange caps one eth_getLogs block span.
	MaxRange uint64
	// CatchupBlocks is how far behind the cursor a newly issued address is
	// rescanned, covering deposits made before the scanner learned it.
	CatchupBlocks uint64
	// StartBlock is where an empty cursor starts; 0 = LookbackBlocks behind head.
	StartBlock     uint64
	LookbackBlocks uint64
	// MaxGasPriceWei refuses EVM sweeps above this price (0 = no cap).
	MaxGasPriceWei uint64
}

type Config struct {
	NewAPIURL     string
	ScannerSecret string
	GasKey        *ecdsa.PrivateKey
	DataDir       string
	PollInterval  time.Duration
	SweepEnabled  bool
	Chains        map[string]Chain
	// TRON HTTP API (triggersmartcontract / broadcasttransaction) for sweeps.
	TronAPI      string
	TronAPIKey   string
	TronFeeLimit int64 // sun
	ListenAddr   string

	// Operator alerts. An empty SMTPHost disables email entirely.
	SMTPHost            string
	SMTPPort            int
	SMTPUser            string
	SMTPPass            string
	MailFrom            string
	MailTo              []string
	AlertCooldown       time.Duration
	GasMin              map[string]float64 // native coin floor per chain
	ExpectedDestination map[string]string  // lower-case destination we expect
	DailyHour           int
	DailyMinute         int
}

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envUint(key string, def uint64) (uint64, error) {
	v := env(key, "")
	if v == "" {
		return def, nil
	}
	n, err := strconv.ParseUint(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s must be a non-negative integer", key)
	}
	return n, nil
}

// chain defaults: block spans every common provider accepts, and catch-up
// windows of roughly ten minutes of blocks.
var chainDefaults = map[string]Chain{
	usdt.ChainBSC:      {MaxRange: 1000, CatchupBlocks: 1000, LookbackBlocks: 2000, MaxGasPriceWei: 10_000_000_000},
	usdt.ChainArbitrum: {MaxRange: 2000, CatchupBlocks: 3000, LookbackBlocks: 5000, MaxGasPriceWei: 10_000_000_000},
	usdt.ChainPolygon:  {MaxRange: 1000, CatchupBlocks: 400, LookbackBlocks: 2000, MaxGasPriceWei: 2_000_000_000_000},
	usdt.ChainTron:     {MaxRange: 1000, CatchupBlocks: 300, LookbackBlocks: 2000},
}

func Load() (*Config, error) {
	cfg := &Config{
		NewAPIURL:     strings.TrimRight(env("NEWAPI_URL", ""), "/"),
		ScannerSecret: env("SCANNER_SECRET", ""),
		DataDir:       env("DATA_DIR", "./data"),
		SweepEnabled:  env("SWEEP_ENABLED", "true") == "true",
		TronAPI:       strings.TrimRight(env("TRON_API", "https://api.trongrid.io"), "/"),
		TronAPIKey:    env("TRON_API_KEY", ""),
		ListenAddr:    env("LISTEN_ADDR", "127.0.0.1:9180"),
		Chains:        map[string]Chain{},
	}
	if cfg.NewAPIURL == "" || cfg.ScannerSecret == "" {
		return nil, errors.New("NEWAPI_URL and SCANNER_SECRET are required")
	}
	poll, err := envUint("POLL_SECONDS", 10)
	if err != nil {
		return nil, err
	}
	cfg.PollInterval = time.Duration(max(poll, 3)) * time.Second
	feeLimit, err := envUint("TRON_FEE_LIMIT_TRX", 50)
	if err != nil {
		return nil, err
	}
	cfg.TronFeeLimit = int64(feeLimit) * 1_000_000

	// ---- operator alerts (email) ----
	cfg.SMTPHost = env("SMTP_HOST", "")
	port, err := envUint("SMTP_PORT", 465)
	if err != nil {
		return nil, err
	}
	cfg.SMTPPort = int(port)
	cfg.SMTPUser = env("SMTP_USER", "")
	cfg.SMTPPass = env("SMTP_PASS", "")
	cfg.MailFrom = env("MAIL_FROM", cfg.SMTPUser)
	for _, raw := range strings.Split(env("MAIL_TO", ""), ",") {
		if addr := strings.TrimSpace(raw); addr != "" {
			cfg.MailTo = append(cfg.MailTo, addr)
		}
	}
	cooldown, err := envUint("ALERT_COOLDOWN_SECONDS", 21600)
	if err != nil {
		return nil, err
	}
	cfg.AlertCooldown = time.Duration(cooldown) * time.Second
	hour, err := envUint("ALERT_DAILY_HOUR", 20)
	if err != nil {
		return nil, err
	}
	minute, err := envUint("ALERT_DAILY_MINUTE", 0)
	if err != nil {
		return nil, err
	}
	cfg.DailyHour, cfg.DailyMinute = int(hour), int(minute)
	// Defaults sized to "a couple of sweeps' worth": Polygon 0.2, BSC 0.01,
	// Arbitrum 0.0005, TRON 20.
	cfg.GasMin = map[string]float64{
		usdt.ChainBSC:      0.01,
		usdt.ChainPolygon:  0.2,
		usdt.ChainArbitrum: 0.0005,
		usdt.ChainTron:     20,
	}
	cfg.ExpectedDestination = map[string]string{}

	if raw := env("GAS_PRIVATE_KEY", ""); raw != "" {
		key, err := crypto.HexToECDSA(strings.TrimPrefix(raw, "0x"))
		if err != nil {
			return nil, errors.New("GAS_PRIVATE_KEY is not a valid secp256k1 private key")
		}
		cfg.GasKey = key
	} else if cfg.SweepEnabled {
		return nil, errors.New("GAS_PRIVATE_KEY is required when SWEEP_ENABLED=true")
	}

	for _, id := range usdt.ChainOrder {
		upper := strings.ToUpper(id)
		c := chainDefaults[id]
		c.RPC = env("RPC_"+upper, "")
		if c.RPC == "" {
			continue
		}
		var err error
		if c.MaxRange, err = envUint("MAX_RANGE_"+upper, c.MaxRange); err != nil {
			return nil, err
		}
		if c.CatchupBlocks, err = envUint("CATCHUP_BLOCKS_"+upper, c.CatchupBlocks); err != nil {
			return nil, err
		}
		if c.StartBlock, err = envUint("START_BLOCK_"+upper, 0); err != nil {
			return nil, err
		}
		if c.LookbackBlocks, err = envUint("LOOKBACK_BLOCKS_"+upper, c.LookbackBlocks); err != nil {
			return nil, err
		}
		if c.MaxGasPriceWei, err = envUint("MAX_GAS_PRICE_WEI_"+upper, c.MaxGasPriceWei); err != nil {
			return nil, err
		}
		if raw := env("ALERT_GAS_MIN_"+upper, ""); raw != "" {
			floor, err := strconv.ParseFloat(raw, 64)
			if err != nil || floor < 0 {
				return nil, fmt.Errorf("ALERT_GAS_MIN_%s must be a non-negative number", upper)
			}
			cfg.GasMin[id] = floor
		}
		if raw := env("ALERT_EXPECTED_DESTINATION_"+upper, ""); raw != "" {
			cfg.ExpectedDestination[id] = strings.ToLower(strings.TrimSpace(raw))
		}
		if c.MaxRange == 0 {
			c.MaxRange = 1
		}
		cfg.Chains[id] = c
	}
	if len(cfg.Chains) == 0 {
		return nil, errors.New("configure at least one RPC_<CHAIN> (RPC_BSC, RPC_ARBITRUM, RPC_POLYGON, RPC_TRON)")
	}
	return cfg, nil
}
