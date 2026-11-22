// Package scanner walks masterchain blocks in order and feeds transactions of
// known VFT contracts to the processor.
//
// Why blocks and not account histories: a vote contract is destroyed on a full
// revoke, and the history of a destroyed account can't be listed anymore.
// Blocks contain every transaction, including those of destroyed accounts.
package scanner

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/ton"

	"github.com/VoteForTruth/vft-indexer/internal/indexer"
	"github.com/VoteForTruth/vft-indexer/internal/store"
	"github.com/VoteForTruth/vft-indexer/internal/vft"
)

const basechain = 0

// maxParentWalk bounds the walk back through shard parents after a split/merge.
const maxParentWalk = 64

type Scanner struct {
	api       ton.APIClientWrapped
	processor *indexer.Processor
	store     *store.Store
	log       *slog.Logger

	// how many masterchain blocks are fetched ahead in parallel
	prefetch int
}

func New(api ton.APIClientWrapped, processor *indexer.Processor, st *store.Store, log *slog.Logger, prefetch int) *Scanner {
	return &Scanner{api: api, processor: processor, store: st, log: log, prefetch: prefetch}
}

// shardTx is a transaction of some basechain account listed in a shard block.
type shardTx struct {
	block   *ton.BlockIDExt
	account []byte
	lt      uint64
}

// batch holds the basechain transactions that appeared with one masterchain block.
type batch struct {
	seqno uint32
	txs   []shardTx
	err   error
}

// Run processes masterchain blocks starting from `from` until ctx is cancelled.
func (s *Scanner) Run(ctx context.Context, from uint32) error {
	head, err := s.api.GetMasterchainInfo(ctx)
	if err != nil {
		return fmt.Errorf("masterchain info: %w", err)
	}
	masterShard := head.Shard
	headSeqno := head.SeqNo

	// Batches are fetched concurrently and consumed strictly in order: a parent's
	// transaction must register a child before the child's transactions are checked.
	// Blocks are requested only up to the known head: asking lite servers for
	// future blocks just times out and backs off, which stalls the live mode.
	results := make(chan chan batch, s.prefetch)
	go func() {
		defer close(results)
		for seqno := from; ctx.Err() == nil; seqno++ {
			for seqno > headSeqno {
				if headSeqno, err = s.waitHead(ctx, headSeqno); err != nil {
					return
				}
			}

			ch := make(chan batch, 1)
			select {
			case results <- ch:
			case <-ctx.Done():
				return
			}
			go func(seqno uint32) {
				txs, err := s.fetchBatch(ctx, masterShard, seqno)
				ch <- batch{seqno: seqno, txs: txs, err: err}
			}(seqno)
		}
	}()

	lastLog := time.Now()
	for ch := range results {
		var b batch
		select {
		case b = <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
		if b.err != nil {
			return fmt.Errorf("fetch block %d: %w", b.seqno, b.err)
		}

		if err = s.processBatch(ctx, b); err != nil {
			return fmt.Errorf("process block %d: %w", b.seqno, err)
		}
		if err = s.store.SetCursor(ctx, b.seqno); err != nil {
			return fmt.Errorf("save cursor: %w", err)
		}

		if time.Since(lastLog) > 10*time.Second {
			s.log.Info("scanning", "seqno", b.seqno)
			lastLog = time.Now()
		}
	}
	return ctx.Err()
}

// waitHead polls the masterchain head until it moves past `known`.
func (s *Scanner) waitHead(ctx context.Context, known uint32) (uint32, error) {
	for {
		head, err := s.api.GetMasterchainInfo(ctx)
		if err == nil && head.SeqNo > known {
			return head.SeqNo, nil
		}
		if err != nil {
			s.log.Debug("masterchain info", "err", err)
		}

		select {
		case <-time.After(300 * time.Millisecond):
		case <-ctx.Done():
			return known, ctx.Err()
		}
	}
}

func (s *Scanner) processBatch(ctx context.Context, b batch) error {
	for _, t := range b.txs {
		account := address.NewAddress(0, basechain, t.account)
		if !s.processor.IsKnown(account) {
			continue
		}

		full, err := retry(ctx, s.log, func() (*tlb.Transaction, error) { return s.api.GetTransaction(ctx, t.block, account, t.lt) })
		if err != nil {
			return fmt.Errorf("get tx %d of %s: %w", t.lt, indexer.Raw(account), err)
		}

		tx, err := vft.DecodeTx(full)
		if err != nil {
			return err
		}
		if err = s.processor.Apply(ctx, tx); err != nil {
			return err
		}
		s.log.Debug("applied", "account", indexer.Raw(account), "lt", t.lt, "seqno", b.seqno)
	}
	return nil
}

// fetchBatch lists basechain transactions of shard blocks that were committed
// by masterchain block `seqno`, sorted by logical time.
func (s *Scanner) fetchBatch(ctx context.Context, masterShard int64, seqno uint32) ([]shardTx, error) {
	cur, err := s.shardsOf(ctx, masterShard, seqno)
	if err != nil {
		return nil, err
	}
	prev, err := s.shardsOf(ctx, masterShard, seqno-1)
	if err != nil {
		return nil, err
	}

	prevSeqno := map[int64]uint32{}
	for _, sh := range prev {
		prevSeqno[sh.Shard] = sh.SeqNo
	}

	var blocks []*ton.BlockIDExt
	for _, sh := range cur {
		newBlocks, err := s.newShardBlocks(ctx, sh, prevSeqno)
		if err != nil {
			return nil, err
		}
		blocks = append(blocks, newBlocks...)
	}

	var txs []shardTx
	for _, block := range blocks {
		blockTxs, err := s.listTransactions(ctx, block)
		if err != nil {
			return nil, err
		}
		txs = append(txs, blockTxs...)
	}

	sort.Slice(txs, func(i, j int) bool { return txs[i].lt < txs[j].lt })
	return txs, nil
}

func (s *Scanner) shardsOf(ctx context.Context, masterShard int64, seqno uint32) ([]*ton.BlockIDExt, error) {
	shards, err := retry(ctx, s.log, func() ([]*ton.BlockIDExt, error) {
		master, err := s.api.WaitForBlock(seqno).LookupBlock(ctx, -1, masterShard, seqno)
		if err != nil {
			return nil, fmt.Errorf("lookup master %d: %w", seqno, err)
		}
		return s.api.GetBlockShardsInfo(ctx, master)
	})
	if err != nil {
		return nil, err
	}

	var basechainShards []*ton.BlockIDExt
	for _, sh := range shards {
		if sh.Workchain == basechain {
			basechainShards = append(basechainShards, sh)
		}
	}
	return basechainShards, nil
}

// newShardBlocks returns shard blocks after the ones referenced by the previous
// masterchain block, up to and including `top`. Usually the shard is the same and
// the gap is filled by seqno; after a split/merge the parents are walked instead.
func (s *Scanner) newShardBlocks(ctx context.Context, top *ton.BlockIDExt, prevSeqno map[int64]uint32) ([]*ton.BlockIDExt, error) {
	if last, ok := prevSeqno[top.Shard]; ok {
		var blocks []*ton.BlockIDExt
		for seqno := last + 1; seqno < top.SeqNo; seqno++ {
			block, err := retry(ctx, s.log, func() (*ton.BlockIDExt, error) { return s.api.LookupBlock(ctx, top.Workchain, top.Shard, seqno) })
			if err != nil {
				return nil, fmt.Errorf("lookup shard block %d: %w", seqno, err)
			}
			blocks = append(blocks, block)
		}
		if top.SeqNo > last {
			blocks = append(blocks, top)
		}
		return blocks, nil
	}

	return s.walkParents(ctx, top, prevSeqno, 0)
}

func (s *Scanner) walkParents(ctx context.Context, block *ton.BlockIDExt, prevSeqno map[int64]uint32, depth int) ([]*ton.BlockIDExt, error) {
	if last, ok := prevSeqno[block.Shard]; ok && block.SeqNo <= last {
		return nil, nil
	}
	if depth > maxParentWalk {
		return nil, fmt.Errorf("shard %x: no known ancestor within %d blocks", uint64(block.Shard), maxParentWalk)
	}

	data, err := retry(ctx, s.log, func() (*tlb.Block, error) { return s.api.GetBlockData(ctx, block) })
	if err != nil {
		return nil, fmt.Errorf("block data: %w", err)
	}
	parents, err := ton.GetParentBlocks(&data.BlockInfo)
	if err != nil {
		return nil, err
	}

	var blocks []*ton.BlockIDExt
	for _, parent := range parents {
		ancestors, err := s.walkParents(ctx, parent, prevSeqno, depth+1)
		if err != nil {
			return nil, err
		}
		blocks = append(blocks, ancestors...)
	}
	return append(blocks, block), nil
}

func (s *Scanner) listTransactions(ctx context.Context, block *ton.BlockIDExt) ([]shardTx, error) {
	var txs []shardTx
	var after *ton.TransactionID3
	for {
		type page struct {
			list []ton.TransactionShortInfo
			more bool
		}
		p, err := retry(ctx, s.log, func() (page, error) {
			list, more, err := s.api.GetBlockTransactionsV2(ctx, block, 256, after)
			return page{list, more}, err
		})
		if err != nil {
			return nil, fmt.Errorf("block %d transactions: %w", block.SeqNo, err)
		}

		for _, t := range p.list {
			txs = append(txs, shardTx{block: block, account: t.Account, lt: t.LT})
		}
		if !p.more || len(p.list) == 0 {
			return txs, nil
		}
		last := p.list[len(p.list)-1]
		after = &ton.TransactionID3{Account: last.Account, LT: last.LT}
	}
}

// retry repeats lite server failures with backoff until ctx is done. Failures
// are expected to be transient (timeouts, unavailable servers), so it never gives
// up on its own; persistent problems are visible as warnings in the log.
func retry[T any](ctx context.Context, log *slog.Logger, fn func() (T, error)) (T, error) {
	delay := 200 * time.Millisecond
	for attempt := 1; ; attempt++ {
		res, err := fn()
		if err == nil {
			return res, nil
		}
		if ctx.Err() != nil {
			var zero T
			return zero, ctx.Err()
		}

		if attempt%8 == 0 {
			log.Warn("lite server request keeps failing", "attempts", attempt, "err", err)
		} else {
			log.Debug("retrying", "attempt", attempt, "err", err)
		}

		select {
		case <-time.After(delay):
		case <-ctx.Done():
			var zero T
			return zero, ctx.Err()
		}
		delay = min(delay*2, 5*time.Second)
	}
}
