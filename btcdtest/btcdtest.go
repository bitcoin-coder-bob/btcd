package btcdtest

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/rpcclient"
)

// Create a new random address.
func newRandAddress(chain *chaincfg.Params) (btcutil.Address, error) {
	// Generate a new private key.
	prv, err := btcec.NewPrivateKey()
	if err != nil {
		return nil, err
	}

	// Derive a public key, then serialize it.
	pk := prv.PubKey().SerializeUncompressed()

	// Create a new pay-to-pubkey address.
	return btcutil.NewAddressPubKey(pk, chain)
}

// Create the default configuration for testing. Ranomized ports for RPC & P2P, RPC user is "user", pass is "pass".
// Random mining address.
func defaultConfig(dir string) (*Config, error) {
	// Create a new random address.
	addr, err := newRandAddress(&chaincfg.SimNetParams)
	if err != nil {
		return nil, err
	}

	cfg := &Config{
		Chain: &chaincfg.SimNetParams,
		Dir:   dir,

		// Listen on 127.0.0.1 on an OS assigned port.
		RPCListeners: []string{"127.0.0.1:0"},
		Listeners:    []string{"127.0.0.1:0"},

		// Use the random address as the mining address, required for generate.
		MiningAddr: addr.EncodeAddress(),
	}

	cfg.Args = append(cfg.Args,
		// Remove the configuration file, as everything is supplied in flags.
		`--configfile=""`,

		// Set the default credentials to user:pass.
		"--rpcuser", "user",
		"--rpcpass", "pass",
	)

	return cfg, nil
}

type Config struct {
	Stderr io.Writer
	Stdout io.Writer

	Path string
	Dir  string

	Args []string

	Chain *chaincfg.Params

	RPCListeners []string
	Listeners    []string

	MiningAddr string
	DebugLevel string
}

type Harness struct {
	*rpcclient.Client

	cmd *exec.Cmd

	rpc string
	p2p string

	cfg *Config

	running atomic.Bool
	stopped atomic.Bool
}

// Gracefully shutdown btcd instance using SIGINT and wait for it to exit.
func (h *Harness) Stop() {
	if !h.stopped.CompareAndSwap(false, true) {
		h.running.Store(false)
		return
	}

	// Signal SIGINT, for graceful shutdown.
	err := h.cmd.Process.Signal(os.Interrupt)
	if err != nil {
		panic(err)
	}

	// Wait for the program to exit.
	for {
		exit, err := h.cmd.Process.Wait()
		if err != nil {
			break
		}

		// Check if the program exited.
		if exit.Exited() {
			break
		}
	}
}

func (c *Harness) RPCAddress() string {
	return c.rpc
}

func (c *Harness) P2PAddress() string {
	return c.p2p
}

func (c *Harness) RPCConnConfig() (*rpcclient.ConnConfig, error) {
	cert, err := os.ReadFile(filepath.Join(c.cfg.Dir, "rpc.cert"))
	if err != nil {
		return nil, err
	}

	return &rpcclient.ConnConfig{
		Host: c.rpc,

		User: "user",
		Pass: "pass",

		Certificates: cert,

		HTTPPostMode: true,
	}, nil
}

// Start btcd and wait for the RPC to be ready.
func (h *Harness) Start() {
	if !h.running.CompareAndSwap(false, true) {
		return
	}

	name := "btcd"
	if h.cfg.Path != "" {
		name = h.cfg.Path
	}

	// Create the command.
	h.cmd = exec.Command(name, h.cfg.Args...)

	// Create a pipe of stdout.
	pr, pw, err := os.Pipe()
	if err != nil {
		panic(err)
	}

	// Match the output configuration.
	h.cmd.Stderr = h.cfg.Stderr
	h.cmd.Stdout = pw

	// Execute the command.
	err = h.cmd.Start()
	if err != nil {
		panic(err)
	}

	var r io.Reader = pr

	// Pipe the early output to stdout if configured.
	if h.cfg.Stdout != nil {
		r = io.TeeReader(pr, h.cfg.Stdout)
	}

	// Scan the stdout line by line.
	scan := bufio.NewScanner(r)

	rpc := false
	p2p := false

	// Scan each line until both RPC (if enabled) and P2P addresses are found.
	for !rpc || !p2p {
		line := scan.Text()

		_, addr, ok := strings.Cut(line, "RPC server listening on ")
		if ok {
			h.rpc = addr
			rpc = true
		}

		_, addr, ok = strings.Cut(line, "Server listening on ")
		if ok {
			h.p2p = addr
			p2p = true
		}

		// Ensure we've not found the RPC and P2P addresses, so we can continue scanning.
		if (!rpc || !p2p) && !scan.Scan() {
			break
		}
	}

	// Return early if RPC is disabled.
	if !rpc {
		return
	}

	// Discard as a fallback.
	stdout := io.Discard

	// Use the configured stdout by default.
	if h.cfg.Stdout != nil {
		stdout = h.cfg.Stdout
	}

	// The pipe needs to continuously be read, otherwise `btcd` will hang.
	go io.Copy(stdout, pr)

	deadline := time.Now().Add(30 * time.Second)

	// Try to connect via RPC for 30 seconds.
	for deadline.After(time.Now()) {
		var cfg *rpcclient.ConnConfig
		// Create the RPC config.
		cfg, err = h.RPCConnConfig()
		if err != nil {
			continue
		}

		// Create the RPC client.
		h.Client, err = rpcclient.New(cfg, nil)
		if err != nil {
			continue
		}

		// Ping the RPC client.
		err = h.Client.Ping()
		if err != nil {
			continue
		}

		err = nil
		break
	}

	// Check if the connection loop exited with an error.
	if err != nil {
		panic(fmt.Sprintf("timeout: %v", err))
	}

	// Check if the client was created.
	if h.Client == nil {
		panic("timeout")
	}

	// Enable block generation on regtest & simnet.
	if devnet(h.cfg.Chain) {
		err := h.SetGenerate(true, 0)
		if err != nil {
			panic(err)
		}
	}
}

func devnet(c *chaincfg.Params) bool {
	return c.Name == chaincfg.SimNetParams.Name || c.Name == chaincfg.RegressionNetParams.Name
}

// Update the directory.
func WithDir(dir string) func(*Config) {
	return func(cfg *Config) {
		cfg.Dir = dir
	}
}

// Update the output.
func WithOutput(stderr io.Writer, stdout io.Writer) func(*Config) {
	return func(cfg *Config) {
		cfg.Stderr = stderr
		cfg.Stdout = stdout
	}
}

// Set custom debug log level.
func WithDebugLevel(level string) func(*Config) {
	return func(cfg *Config) {
		cfg.DebugLevel = level
	}
}

// Set custom binary path.
func WithBinary(path string) func(*Config) {
	return func(cfg *Config) {
		cfg.Path = path
	}
}

func WithChainParams(chain *chaincfg.Params) func(*Config) {
	return func(cfg *Config) {
		cfg.Chain = chain

		if devnet(chain) {
			addr, err := newRandAddress(chain)
			if err != nil {
				panic(err)
			}

			cfg.MiningAddr = addr.EncodeAddress()
		}
	}
}

func WithArgs(args ...string) func(*Config) {
	return func(cfg *Config) {
		cfg.Args = append(cfg.Args, args...)
	}
}

// Create and start a harness.
func New(opts ...func(*Config)) *Harness {
	h := NewUnstarted(opts...)
	h.Start()

	return h
}

func NewUnstarted(opts ...func(*Config)) *Harness {
	tmp, err := os.MkdirTemp("", "btcdtest-*")
	if err != nil {
		panic(err)
	}

	cfg, err := defaultConfig(tmp)
	if err != nil {
		panic(err)
	}

	for _, opt := range opts {
		opt(cfg)
	}

	// Set the RPC listeners.
	for _, l := range cfg.RPCListeners {
		cfg.Args = append(cfg.Args, "--rpclisten", l)
	}

	// Set the P2P listeners.
	for _, l := range cfg.Listeners {
		cfg.Args = append(cfg.Args, "--listen", l)
	}

	// Enable block generation on devnets.
	if devnet(cfg.Chain) {
		cfg.Args = append(cfg.Args, "--generate")
	}

	// Set the mining address.
	if cfg.MiningAddr != "" {
		cfg.Args = append(cfg.Args, "--miningaddr", cfg.MiningAddr)
	}

	// Set the debug level.
	if cfg.DebugLevel != "" {
		cfg.Args = append(cfg.Args, "--debuglevel", cfg.DebugLevel)
	}

	// Set the chain params flag.
	cfg.Args = append(cfg.Args, "--"+nameParams(cfg.Chain))

	// Set the directory (unlike lnd, litd and tapd, there is not "btcddir").
	cfg.Args = append(cfg.Args,
		"--logdir", filepath.Join(cfg.Dir, "logs"),
		"--datadir", filepath.Join(cfg.Dir, "data"),
		"--rpckey", filepath.Join(cfg.Dir, "rpc.key"),
		"--rpccert", filepath.Join(cfg.Dir, "rpc.cert"),
	)

	h := &Harness{
		cfg: cfg,
	}

	return h
}

// `btcd` refers to testnet3 as "testnet", match behaviour here.
// https://github.com/btcsuite/btcd/blob/cd05d9ad3d0597368adf95c54bdc530700393aed/params.go#L73-L89
func nameParams(chain *chaincfg.Params) string {
	var name string

	switch chain.Name {
	case chaincfg.TestNet3Params.Name:
		name = "testnet"

	default:
		name = chain.Name
	}

	return name
}
