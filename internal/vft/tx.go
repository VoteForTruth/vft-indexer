package vft

import (
	"fmt"

	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/tlb"
)

// Tx is a transaction reduced to what the indexer needs.
type Tx struct {
	Account *address.Address
	LT      uint64
	Hash    []byte
	Time    uint32

	// Success means the contract accepted the message: compute and action phases
	// succeeded and the transaction was not aborted, so its state changes persisted.
	Success bool

	// In is the incoming internal message, nil for external/tick-tock transactions.
	In  *tlb.InternalMessage
	Out []*tlb.InternalMessage
}

// DecodeTx extracts the incoming/outgoing internal messages and the success flag.
// All VFT contracts live in the basechain.
func DecodeTx(t *tlb.Transaction) (Tx, error) {
	tx := Tx{
		Account: address.NewAddress(0, workchain, t.AccountAddr),
		LT:      t.LT,
		Hash:    t.Hash,
		Time:    t.Now,
		Success: isSuccessful(t),
	}

	if t.IO.In != nil && t.IO.In.MsgType == tlb.MsgTypeInternal {
		tx.In = t.IO.In.AsInternal()
	}

	if t.IO.Out != nil {
		out, err := t.IO.Out.ToSlice()
		if err != nil {
			return Tx{}, fmt.Errorf("out messages of tx %d: %w", t.LT, err)
		}
		for i := range out {
			if out[i].MsgType == tlb.MsgTypeInternal {
				tx.Out = append(tx.Out, out[i].AsInternal())
			}
		}
	}

	return tx, nil
}

func isSuccessful(t *tlb.Transaction) bool {
	desc, ok := t.Description.(tlb.TransactionDescriptionOrdinary)
	if !ok || desc.Aborted {
		return false
	}

	compute, ok := desc.ComputePhase.Phase.(tlb.ComputePhaseVM)
	if !ok || !compute.Success {
		return false
	}

	// no action phase means nothing could fail there
	return desc.ActionPhase == nil || desc.ActionPhase.Success
}
