package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"math/big"
	"sort"
	"time"

	"github.com/VoteForTruth/vft-indexer/internal/store"
	"github.com/VoteForTruth/vft-indexer/internal/weight"
)

// reader builds the read model from statements and events. Weights depend on
// the current time, so they are computed on request rather than stored.
type reader struct {
	db *sql.DB
}

// list encodes values for `in (select value from json_each(?))` — SQLite has
// no array parameters.
func list(values []string) string {
	if values == nil {
		values = []string{}
	}
	b, _ := json.Marshal(values)
	return string(b)
}

type statementRow struct {
	Address   string
	Root      string
	Parent    string
	Depth     int
	Idx       int
	Pro       bool
	Author    string
	Text      string
	URL       *string
	CreatedAt time.Time
	Hidden    bool
	Reason    *string
	HiddenAt  *time.Time
}

const statementColumns = `address, root, parent, depth, idx, pro, author, text, url, created_at, hidden, hidden_reason, hidden_at`

func scanStatements(ctx context.Context, db *sql.DB, query string, args ...any) ([]statementRow, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []statementRow
	for rows.Next() {
		var s statementRow
		err = rows.Scan(&s.Address, &s.Root, &s.Parent, &s.Depth, &s.Idx, &s.Pro, &s.Author, &s.Text, &s.URL, &s.CreatedAt, &s.Hidden, &s.Reason, &s.HiddenAt)
		if err != nil {
			return nil, err
		}
		s.CreatedAt = s.CreatedAt.UTC()
		if s.HiddenAt != nil {
			utc := s.HiddenAt.UTC()
			s.HiddenAt = &utc
		}
		list = append(list, s)
	}
	return list, rows.Err()
}

func (r reader) statement(ctx context.Context, address string) (statementRow, error) {
	rows, err := scanStatements(ctx, r.db, `select `+statementColumns+` from statements where address = ?1`, address)
	if err != nil {
		return statementRow{}, err
	}
	if len(rows) == 0 {
		return statementRow{}, store.ErrNotFound
	}
	return rows[0], nil
}

func (r reader) roots(ctx context.Context) ([]statementRow, error) {
	return scanStatements(ctx, r.db, `select `+statementColumns+` from statements where depth = 0 and not hidden`)
}

func (r reader) children(ctx context.Context, parents []string) ([]statementRow, error) {
	return scanStatements(ctx, r.db,
		`select `+statementColumns+` from statements where parent in (select value from json_each(?1)) and not hidden order by idx`, list(parents))
}

func (r reader) byAuthor(ctx context.Context, author string) ([]statementRow, error) {
	return scanStatements(ctx, r.db, `select `+statementColumns+` from statements where author = ?1 order by created_lt desc`, author)
}

func (r reader) hidden(ctx context.Context) ([]statementRow, error) {
	return scanStatements(ctx, r.db, `select `+statementColumns+` from statements where hidden order by hidden_at desc`)
}

// tally is the folded state of all votes and donations of one statement.
type tally struct {
	votes     map[string]*weight.Vote // by wallet
	donations *big.Int
}

// tallies folds events of the given statements, in logical time order.
func (r reader) tallies(ctx context.Context, statements []string) (map[string]*tally, error) {
	result := map[string]*tally{}
	for _, s := range statements {
		result[s] = &tally{votes: map[string]*weight.Vote{}, donations: new(big.Int)}
	}

	rows, err := r.db.QueryContext(ctx, `
		select statement, wallet, kind, coalesce(amount, '0'), time
		from events where statement in (select value from json_each(?1)) order by lt`, list(statements))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var statement, wallet, kind, amountText string
		var t time.Time
		if err = rows.Scan(&statement, &wallet, &kind, &amountText, &t); err != nil {
			return nil, err
		}
		t = t.UTC()
		amount, _ := new(big.Int).SetString(amountText, 10)
		tl := result[statement]

		if store.EventKind(kind) == store.EventDonation {
			tl.donations.Add(tl.donations, amount)
			continue
		}

		v := tl.votes[wallet]
		if v == nil {
			v = &weight.Vote{}
			tl.votes[wallet] = v
		}
		switch store.EventKind(kind) {
		case store.EventVoteAdded:
			v.Add(amount, t)
		case store.EventVoteWithdrawn:
			v.Withdraw(amount)
		case store.EventVoteRevokedAll:
			v.RevokeAll()
		}
	}
	return result, rows.Err()
}

// activity counts events in each tree (by root) since `since`.
func (r reader) activity(ctx context.Context, since time.Time) (map[string]int, error) {
	rows, err := r.db.QueryContext(ctx, `
		select s.root, count(*) from events e join statements s on s.address = e.statement
		where e.time >= ?1 group by s.root`, since.UTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := map[string]int{}
	for rows.Next() {
		var root string
		var n int
		if err = rows.Scan(&root, &n); err != nil {
			return nil, err
		}
		result[root] = n
	}
	return result, rows.Err()
}

// walletVotes returns statements with events of the wallet.
func (r reader) walletStatements(ctx context.Context, wallet string, kinds []string) ([]string, error) {
	rows, err := r.db.QueryContext(ctx, `select distinct statement from events where wallet = ?1 and kind in (select value from json_each(?2))`, wallet, list(kinds))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []string
	for rows.Next() {
		var s string
		if err = rows.Scan(&s); err != nil {
			return nil, err
		}
		result = append(result, s)
	}
	return result, rows.Err()
}

type donationRow struct {
	Statement string
	Donor     string
	Amount    string
	Time      time.Time
}

func (r reader) donations(ctx context.Context, where string, arg string) ([]donationRow, error) {
	rows, err := r.db.QueryContext(ctx, `
		select e.statement, e.wallet, e.amount, e.time
		from events e join statements s on s.address = e.statement
		where e.kind = 'donation' and `+where+` order by e.lt desc`, arg)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []donationRow
	for rows.Next() {
		var d donationRow
		if err = rows.Scan(&d.Statement, &d.Donor, &d.Amount, &d.Time); err != nil {
			return nil, err
		}
		d.Time = d.Time.UTC()
		list = append(list, d)
	}
	return list, rows.Err()
}

// ---------------------------------------------------------------------------
// Aggregates
// ---------------------------------------------------------------------------

type voterShare struct {
	wallet string
	amount *big.Int
	weight *big.Int
	since  time.Time
}

// voters returns non-empty votes sorted by amount, largest first.
func (t *tally) voters(now time.Time) []voterShare {
	var list []voterShare
	for wallet, v := range t.votes {
		amount := v.Amount()
		if amount.Sign() == 0 {
			continue
		}
		list = append(list, voterShare{wallet: wallet, amount: amount, weight: v.Weight(now), since: v.Since()})
	}
	sort.Slice(list, func(i, j int) bool {
		if c := list[i].amount.Cmp(list[j].amount); c != 0 {
			return c > 0
		}
		return list[i].wallet < list[j].wallet
	})
	return list
}

type voteStats struct {
	amount    *big.Int
	weight    *big.Int
	voters    int
	top1Share float64
	top3Share float64
}

func (t *tally) stats(now time.Time) voteStats {
	voters := t.voters(now)
	st := voteStats{amount: new(big.Int), weight: new(big.Int), voters: len(voters)}
	top1, top3 := new(big.Int), new(big.Int)
	for i, v := range voters {
		st.amount.Add(st.amount, v.amount)
		st.weight.Add(st.weight, v.weight)
		if i == 0 {
			top1.Add(top1, v.amount)
		}
		if i < 3 {
			top3.Add(top3, v.amount)
		}
	}
	st.top1Share = share(top1, st.amount)
	st.top3Share = share(top3, st.amount)
	return st
}

func share(part, total *big.Int) float64 {
	if total.Sign() == 0 {
		return 0
	}
	f, _ := new(big.Rat).SetFrac(part, total).Float64()
	return f
}
