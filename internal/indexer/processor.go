// Package indexer turns transactions of VFT contracts into stored events.
package indexer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/xssnick/tonutils-go/address"

	"github.com/VoteForTruth/vft-indexer/internal/store"
	"github.com/VoteForTruth/vft-indexer/internal/vft"
)

// Raw returns the canonical form of an address used as a key everywhere: "0:<hex>".
func Raw(a *address.Address) string {
	return a.StringRaw()
}

// Processor applies transactions of known contracts. Transactions must be
// applied in logical time order per account; a parent's transaction that deploys
// a child always comes before the child's own transactions.
type Processor struct {
	code      vft.Code
	store     *store.Store
	contracts map[string]*store.Contract
	log       *slog.Logger
}

func NewProcessor(ctx context.Context, code vft.Code, st *store.Store, master *address.Address, log *slog.Logger) (*Processor, error) {
	contracts, err := st.LoadContracts(ctx)
	if err != nil {
		return nil, fmt.Errorf("load contracts: %w", err)
	}

	p := &Processor{code: code, store: st, contracts: contracts, log: log}

	if _, ok := contracts[Raw(master)]; !ok {
		c := &store.Contract{Address: Raw(master), Kind: store.KindMaster}
		err = st.Apply(ctx, func(tx store.Tx) error { return tx.InsertContract(ctx, c) })
		if err != nil {
			return nil, fmt.Errorf("register master: %w", err)
		}
		contracts[c.Address] = c
	}

	return p, nil
}

// IsKnown reports whether transactions of the account must be applied.
func (p *Processor) IsKnown(account *address.Address) bool {
	_, ok := p.contracts[Raw(account)]
	return ok
}

// changes collects updates of the in-memory contract set; they are applied
// only after the database transaction commits.
type changes struct {
	added   []*store.Contract
	updated *store.Contract
}

// Apply stores the effects of a transaction of a known contract.
// Applying the same transaction twice is a no-op.
func (p *Processor) Apply(ctx context.Context, tx vft.Tx) error {
	c, ok := p.contracts[Raw(tx.Account)]
	if !ok {
		return nil
	}

	var ch changes
	err := p.store.Apply(ctx, func(dbtx store.Tx) error {
		ch = changes{}

		fresh, err := dbtx.MarkProcessed(ctx, c.Address, tx.LT)
		if err != nil || !fresh {
			return err
		}

		// rejected and bounced messages change nothing on-chain
		if !tx.Success || tx.In == nil || tx.In.Bounced {
			return nil
		}

		msg, err := vft.ParseMessage(tx.In.Body)
		if errors.Is(err, vft.ErrUnknownMessage) {
			return nil // deploy, tips and other plain transfers
		}
		if err != nil {
			return err
		}

		switch c.Kind {
		case store.KindMaster:
			return p.applyMaster(ctx, dbtx, c, tx, msg, &ch)
		case store.KindStatement:
			return p.applyStatement(ctx, dbtx, c, tx, msg, &ch)
		case store.KindVote:
			return p.applyVote(ctx, dbtx, c, tx, msg)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("apply tx %d of %s: %w", tx.LT, c.Address, err)
	}

	for _, added := range ch.added {
		p.contracts[added.Address] = added
	}
	if ch.updated != nil {
		p.contracts[ch.updated.Address] = ch.updated
	}
	return nil
}

func (p *Processor) applyMaster(ctx context.Context, dbtx store.Tx, c *store.Contract, tx vft.Tx, msg any, ch *changes) error {
	if _, ok := msg.(vft.NewParentStatement); !ok {
		return nil
	}

	updated := *c
	updated.NextIdx++
	return p.registerStatement(ctx, dbtx, &updated, c.NextIdx, true, tx, ch)
}

func (p *Processor) applyStatement(ctx context.Context, dbtx store.Tx, c *store.Contract, tx vft.Tx, msg any, ch *changes) error {
	switch m := msg.(type) {
	case vft.SetStatementData:
		return p.storeStatement(ctx, dbtx, c, tx, m)

	case vft.NewChildStatement:
		updated := *c
		idx := c.NextCon
		if m.Pro {
			idx = c.NextPro
			updated.NextPro++
		} else {
			updated.NextCon++
		}
		return p.registerStatement(ctx, dbtx, &updated, idx, m.Pro, tx, ch)

	case vft.Vote:
		voter := tx.In.SrcAddr
		voteAddr := p.code.VoteAddress(tx.Account, voter)
		if err := expectOut(tx, voteAddr); err != nil {
			return err
		}
		if _, known := p.contracts[Raw(voteAddr)]; known {
			return nil // adding to an existing vote, or re-creating a destroyed one
		}
		vote := &store.Contract{Address: Raw(voteAddr), Kind: store.KindVote, Statement: c.Address, Owner: Raw(voter)}
		ch.added = append(ch.added, vote)
		return dbtx.InsertContract(ctx, vote)

	case vft.SetUrl:
		// the contract accepts it only from the author
		return dbtx.RecordURL(ctx, c.Address, m.URL, txTime(tx), tx.LT)

	case vft.DeleteStatement:
		// the contract accepts it only from the author and only without arguments
		return dbtx.MarkDeleted(ctx, c.Address, txTime(tx))

	case vft.Donate:
		// the whole message value minus fees goes to the author in a single payout
		if len(tx.Out) != 1 {
			return fmt.Errorf("donation: expected 1 payout, got %d", len(tx.Out))
		}
		return dbtx.InsertEvent(ctx, store.Event{
			Account:   c.Address,
			LT:        tx.LT,
			TxHash:    tx.Hash,
			Time:      txTime(tx),
			Kind:      store.EventDonation,
			Statement: c.Address,
			Wallet:    Raw(tx.In.SrcAddr),
			Amount:    tx.Out[0].Amount.Nano(),
		})
	}
	return nil
}

func (p *Processor) applyVote(ctx context.Context, dbtx store.Tx, c *store.Contract, tx vft.Tx, msg any) error {
	e := store.Event{
		Account:   c.Address,
		LT:        tx.LT,
		TxHash:    tx.Hash,
		Time:      txTime(tx),
		Statement: c.Statement,
		Wallet:    c.Owner,
	}

	switch m := msg.(type) {
	case vft.AddVote:
		e.Kind, e.Amount = store.EventVoteAdded, m.Amount
	case vft.Withdraw:
		e.Kind, e.Amount = store.EventVoteWithdrawn, m.Amount
	case vft.WithdrawAll:
		e.Kind = store.EventVoteRevokedAll
	default:
		return nil
	}
	return dbtx.InsertEvent(ctx, e)
}

// registerStatement records a child statement deployed by `parent` (master or statement)
// and the parent's incremented counters.
func (p *Processor) registerStatement(ctx context.Context, dbtx store.Tx, parent *store.Contract, idx uint32, pro bool, tx vft.Tx, ch *changes) error {
	childAddr := p.code.StatementAddress(tx.Account, idx, pro)
	if err := expectOut(tx, childAddr); err != nil {
		return err
	}

	child := &store.Contract{Address: Raw(childAddr), Kind: store.KindStatement, Parent: parent.Address, Idx: idx, Pro: pro}
	if err := dbtx.InsertContract(ctx, child); err != nil {
		return err
	}
	if err := dbtx.UpdateCounters(ctx, parent); err != nil {
		return err
	}

	ch.added = append(ch.added, child)
	ch.updated = parent
	return nil
}

// storeStatement saves the content of a statement when its parent initializes it.
func (p *Processor) storeStatement(ctx context.Context, dbtx store.Tx, c *store.Contract, tx vft.Tx, m vft.SetStatementData) error {
	root, depth := c.Address, 0
	if p.contracts[c.Parent].Kind == store.KindStatement {
		parentRoot, parentDepth, err := dbtx.StatementPosition(ctx, c.Parent)
		if err != nil {
			return fmt.Errorf("parent %s: %w", c.Parent, err)
		}
		root, depth = parentRoot, parentDepth+1
	}

	err := dbtx.InsertStatement(ctx, store.Statement{
		Address:   c.Address,
		Root:      root,
		Parent:    c.Parent,
		Depth:     depth,
		Idx:       c.Idx,
		Pro:       c.Pro,
		Author:    Raw(m.Owner),
		Text:      m.Data.Statement,
		URL:       m.Data.URL,
		CreatedAt: txTime(tx),
		CreatedLT: tx.LT,
	})
	if err != nil {
		return err
	}
	return dbtx.RecordURL(ctx, c.Address, m.Data.URL, txTime(tx), tx.LT)
}

// expectOut checks that the transaction sent a message to the derived address.
// A mismatch means the embedded contract code differs from the deployed one.
func expectOut(tx vft.Tx, dst *address.Address) error {
	for _, out := range tx.Out {
		if out.DstAddr.Equals(dst) {
			return nil
		}
	}
	return fmt.Errorf("no message to derived address %s: contract code mismatch?", Raw(dst))
}

func txTime(tx vft.Tx) time.Time {
	return time.Unix(int64(tx.Time), 0).UTC()
}
