// vft-indexer scans the TON blockchain for Vote For Truth contracts and stores
// their history in Postgres.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/liteclient"
	"github.com/xssnick/tonutils-go/ton"

	"github.com/VoteForTruth/vft-indexer/internal/indexer"
	"github.com/VoteForTruth/vft-indexer/internal/scanner"
	"github.com/VoteForTruth/vft-indexer/internal/store"
	"github.com/VoteForTruth/vft-indexer/internal/vft"
)

var configURLs = map[string]string{
	"testnet": "https://ton-blockchain.github.io/testnet-global.config.json",
	"mainnet": "https://ton-blockchain.github.io/global.config.json",
}

type config struct {
	network    string
	dsn        string
	master     string
	startSeqno uint
	proof      string
	prefetch   int
	verbose    bool
}

func main() {
	var cfg config
	flag.StringVar(&cfg.network, "network", envOr("VFT_NETWORK", "testnet"), "testnet or mainnet")
	flag.StringVar(&cfg.dsn, "dsn", envOr("VFT_DSN", "postgres://localhost/vft_indexer?sslmode=disable"), "Postgres connection string")
	flag.StringVar(&cfg.master, "master", os.Getenv("VFT_MASTER"), "VftMaster address")
	flag.UintVar(&cfg.startSeqno, "start-seqno", 0, "masterchain seqno to start from on the first run (default: found from the master deploy)")
	flag.StringVar(&cfg.proof, "proof", "fast", "lite server proof checks: fast or unsafe")
	flag.IntVar(&cfg.prefetch, "prefetch", 16, "masterchain blocks fetched ahead in parallel")
	flag.BoolVar(&cfg.verbose, "v", false, "debug logging")
	flag.Parse()

	level := slog.LevelInfo
	if cfg.verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, cfg, log); err != nil && !errors.Is(err, context.Canceled) {
		log.Error("stopped", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, cfg config, log *slog.Logger) error {
	if cfg.master == "" {
		return errors.New("-master (or VFT_MASTER) is required")
	}
	master, err := address.ParseAddr(cfg.master)
	if err != nil {
		return fmt.Errorf("master address: %w", err)
	}
	configURL, ok := configURLs[cfg.network]
	if !ok {
		return fmt.Errorf("unknown network %q", cfg.network)
	}

	code, err := vft.LoadCode()
	if err != nil {
		return err
	}

	st, err := store.Open(ctx, cfg.dsn)
	if err != nil {
		return err
	}
	defer st.Close()
	if err = st.Migrate(ctx); err != nil {
		return err
	}

	api, err := connect(ctx, configURL, cfg.proof, log)
	if err != nil {
		return err
	}

	if err = checkMasterCode(ctx, api, code, master); err != nil {
		return err
	}

	processor, err := indexer.NewProcessor(ctx, code, st, master, log)
	if err != nil {
		return err
	}

	from, err := startSeqno(ctx, st, api, master, cfg, log)
	if err != nil {
		return err
	}
	log.Info("scanning", "network", cfg.network, "master", master.String(), "from", from)

	return scanner.New(api, processor, st, log, cfg.prefetch).Run(ctx, from)
}

func connect(ctx context.Context, configURL, proof string, log *slog.Logger) (ton.APIClientWrapped, error) {
	cfg, err := liteclient.GetConfigFromUrl(ctx, configURL)
	if err != nil {
		return nil, fmt.Errorf("get lite server config: %w", err)
	}

	pool := liteclient.NewConnectionPool()
	if err = pool.AddConnectionsFromConfig(ctx, cfg); err != nil {
		return nil, fmt.Errorf("connect to lite servers: %w", err)
	}

	policy := ton.ProofCheckPolicyFast
	if proof == "unsafe" {
		policy = ton.ProofCheckPolicyUnsafe
	}
	client := ton.NewAPIClient(pool, policy)
	if policy != ton.ProofCheckPolicyUnsafe {
		client.SetTrustedBlockFromConfig(cfg)
		log.Info("verifying the proof chain from the config block, may take about a minute")
	}

	api := client.WithTimeout(30 * time.Second)
	if _, err = api.GetMasterchainInfo(ctx); err != nil {
		return nil, fmt.Errorf("masterchain info: %w", err)
	}
	return api, nil
}

// checkMasterCode makes sure the deployed master runs exactly the embedded code:
// otherwise derived addresses of statements and votes would be wrong.
func checkMasterCode(ctx context.Context, api ton.APIClientWrapped, code vft.Code, master *address.Address) error {
	head, err := api.CurrentMasterchainInfo(ctx)
	if err != nil {
		return err
	}
	acc, err := api.GetAccount(ctx, head, master)
	if err != nil {
		return fmt.Errorf("get master: %w", err)
	}
	if !acc.IsActive {
		return fmt.Errorf("master %s is not deployed", master)
	}
	if string(acc.Code.Hash()) != string(code.Master.Hash()) {
		return fmt.Errorf("master %s runs code %x, the indexer embeds %x: update internal/vft/contracts", master, acc.Code.Hash(), code.Master.Hash())
	}
	return nil
}

func startSeqno(ctx context.Context, st *store.Store, api ton.APIClientWrapped, master *address.Address, cfg config, log *slog.Logger) (uint32, error) {
	cursor, err := st.Cursor(ctx)
	if err == nil {
		return cursor + 1, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return 0, err
	}

	if cfg.startSeqno != 0 {
		return uint32(cfg.startSeqno), nil
	}
	return scanner.FindStartSeqno(ctx, api, master, log)
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}
