package scanner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/ton"
)

// FindStartSeqno returns a masterchain seqno generated before the master contract
// was deployed, so scanning from it sees the whole history of the tree.
func FindStartSeqno(ctx context.Context, api ton.APIClientWrapped, master *address.Address, log *slog.Logger) (uint32, error) {
	head, err := api.CurrentMasterchainInfo(ctx)
	if err != nil {
		return 0, fmt.Errorf("masterchain info: %w", err)
	}

	deployTime, err := firstTxTime(ctx, api, head, master)
	if err != nil {
		return 0, err
	}
	log.Info("master deploy found", "time", deployTime)

	utimeAt := func(seqno uint32) (uint32, error) {
		return retry(ctx, log, func() (uint32, error) {
			block, err := api.LookupBlock(ctx, head.Workchain, head.Shard, seqno)
			if err != nil {
				return 0, err
			}
			data, err := api.GetBlockData(ctx, block)
			if err != nil {
				return 0, err
			}
			return data.BlockInfo.GenUtime, nil
		})
	}

	// exponential search back from the head, then binary search
	hi := head.SeqNo
	step := uint32(1024)
	lo := hi
	for {
		if step >= hi {
			lo = 1
			break
		}
		lo = hi - step
		utime, err := utimeAt(lo)
		if err != nil {
			return 0, fmt.Errorf("block %d: %w", lo, err)
		}
		if utime < deployTime {
			break
		}
		step *= 2
	}

	// invariant: utime(lo) < deployTime <= utime(hi)
	for hi-lo > 1 {
		mid := lo + (hi-lo)/2
		utime, err := utimeAt(mid)
		if err != nil {
			return 0, fmt.Errorf("block %d: %w", mid, err)
		}
		if utime < deployTime {
			lo = mid
		} else {
			hi = mid
		}
	}
	return lo, nil
}

func firstTxTime(ctx context.Context, api ton.APIClientWrapped, head *ton.BlockIDExt, addr *address.Address) (uint32, error) {
	acc, err := api.GetAccount(ctx, head, addr)
	if err != nil {
		return 0, fmt.Errorf("get master account: %w", err)
	}
	if !acc.IsActive {
		return 0, fmt.Errorf("master %s is not deployed", addr)
	}

	lt, hash := acc.LastTxLT, acc.LastTxHash
	var oldest *tlb.Transaction
	for lt != 0 {
		txs, err := api.ListTransactions(ctx, addr, 16, lt, hash)
		if errors.Is(err, ton.ErrNoTransactionsWereFound) {
			break
		}
		if err != nil {
			return 0, fmt.Errorf("list master transactions: %w", err)
		}
		if len(txs) == 0 {
			break
		}
		// txs are ordered from old to new; continue before the oldest one
		oldest = txs[0]
		lt, hash = oldest.PrevTxLT, oldest.PrevTxHash
	}
	if oldest == nil {
		return 0, fmt.Errorf("master %s has no transactions", addr)
	}
	return oldest.Now, nil
}
