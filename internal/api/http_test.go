package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/xssnick/tonutils-go/address"

	"github.com/VoteForTruth/vft-indexer/internal/store"
	"github.com/VoteForTruth/vft-indexer/internal/store/storetest"
)

var t0 = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)

func day(n float64) time.Time { return t0.Add(time.Duration(n * float64(24*time.Hour))) }

func nano(ton float64) *big.Int { return big.NewInt(int64(ton * 1e9)) }

// addr makes a distinct raw address from a short name.
func addr(name string) string {
	data := make([]byte, 32)
	copy(data, name)
	return address.NewAddress(0, 0, data).StringRaw()
}

func friendly(raw string, wallet bool) string {
	a, _ := address.ParseRawAddr(raw)
	return a.Testnet(true).Bounce(!wallet).String()
}

type fixture struct {
	st                          *store.Store
	srv                         *httptest.Server
	root, root2, p1, p2, c1, c2 string
	author, alice, bob, carol   string
}

func setup(t *testing.T) fixture {
	t.Helper()
	ctx := context.Background()

	st := storetest.Open(t)
	var err error

	f := fixture{
		st: st, root: addr("root"), root2: addr("root2"), p1: addr("p1"), p2: addr("p2"), c1: addr("c1"), c2: addr("c2"),
		author: addr("author"), alice: addr("alice"), bob: addr("bob"), carol: addr("carol"),
	}
	master := addr("master")
	dave, eve := addr("dave"), addr("eve")

	lt := uint64(0)
	err = st.Apply(ctx, func(tx store.Tx) error {
		statement := func(a, parent, root string, depth, idx int, pro bool, text string, created time.Time) {
			if err == nil {
				err = tx.InsertContract(ctx, &store.Contract{Address: a, Kind: store.KindStatement, Parent: parent, Idx: uint32(idx), Pro: pro})
			}
			if err == nil {
				lt++
				err = tx.InsertStatement(ctx, store.Statement{Address: a, Root: root, Parent: parent, Depth: depth, Idx: uint32(idx), Pro: pro,
					Author: f.author, Text: text, CreatedAt: created, CreatedLT: lt})
			}
		}
		event := func(kind store.EventKind, statement, wallet string, amount *big.Int, at time.Time) {
			if err == nil {
				lt++
				err = tx.InsertEvent(ctx, store.Event{Account: addr(fmt.Sprintf("acc%d", lt)), LT: lt, TxHash: []byte{1}, Time: at,
					Kind: kind, Statement: statement, Wallet: wallet, Amount: amount})
			}
		}

		if err = tx.InsertContract(ctx, &store.Contract{Address: master, Kind: store.KindMaster}); err != nil {
			return err
		}
		statement(f.root, master, f.root, 0, 0, true, "Question", t0)
		statement(f.p1, f.root, f.root, 1, 0, true, "Pro 1", t0)
		statement(f.p2, f.root, f.root, 1, 1, true, "Pro 2", t0)
		statement(f.c1, f.root, f.root, 1, 0, false, "Con 1", t0)
		statement(f.c2, f.root, f.root, 1, 1, false, "Spam", t0)
		statement(f.root2, master, f.root2, 0, 1, true, "Newer question", day(5))

		// root: 100 TON from three wallets, carol's 60 are only 1 day old
		event(store.EventVoteAdded, f.root, f.alice, nano(10), t0)
		event(store.EventVoteAdded, f.root, f.bob, nano(30), t0)
		event(store.EventVoteAdded, f.root, f.carol, nano(60), day(6))
		// pro: 5 + (20 - 15)
		event(store.EventVoteAdded, f.p1, f.alice, nano(5), t0)
		event(store.EventVoteAdded, f.p2, f.bob, nano(20), t0)
		event(store.EventVoteWithdrawn, f.p2, f.bob, nano(15), day(1))
		// con: carol revoked everything, dave keeps 8; the hidden spam has a whale
		event(store.EventVoteAdded, f.c1, f.carol, nano(50), t0)
		event(store.EventVoteRevokedAll, f.c1, f.carol, nil, day(2))
		event(store.EventVoteAdded, f.c1, dave, nano(8), t0)
		event(store.EventVoteAdded, f.c2, eve, nano(1000), t0)
		// bob donates to the author of the question
		event(store.EventDonation, f.root, f.bob, nano(2), day(3))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = st.SetHidden(ctx, f.c2, true, "spam"); err != nil {
		t.Fatal(err)
	}

	srv := New(st.DB(), true, slog.Default(), func() time.Time { return day(7) })
	f.srv = httptest.NewServer(srv.Handler())
	t.Cleanup(f.srv.Close)
	return f
}

func get(t *testing.T, f fixture, path string, wantStatus int, out any) {
	t.Helper()
	resp, err := http.Get(f.srv.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != wantStatus {
		t.Fatalf("GET %s: status %d, want %d", path, resp.StatusCode, wantStatus)
	}
	if out != nil {
		if err = json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatal(err)
		}
	}
}

func TestStatementDetail(t *testing.T) {
	f := setup(t)

	var body struct {
		Statement statementJSON
		Pro, Con  []statementJSON
	}
	get(t, f, "/statements/"+friendly(f.root, false), 200, &body)

	st := body.Statement
	if st.Text != "Question" || st.Author != friendly(f.author, true) {
		t.Errorf("statement: %+v", st)
	}
	// carol's 60 TON are 1 day old: 60 * 1/7
	wantWeight := new(big.Int).Add(nano(40), new(big.Int).Div(new(big.Int).Mul(nano(60), big.NewInt(86400)), big.NewInt(7*86400)))
	if st.Votes.Amount != nano(100).String() || st.Votes.Weight != wantWeight.String() || st.Votes.Voters != 3 {
		t.Errorf("votes: %+v, want weight %s", st.Votes, wantWeight)
	}
	if st.Votes.Top1Share != 0.6 || st.Votes.Top3Share != 1 {
		t.Errorf("concentration: %+v", st.Votes)
	}
	if st.Donations != nano(2).String() {
		t.Errorf("donations: %s", st.Donations)
	}
	// the hidden spam argument and its 1000 TON don't count
	if st.Result.ProWeight != nano(10).String() || st.Result.ConWeight != nano(8).String() {
		t.Errorf("result: %+v", st.Result)
	}
	if st.Arguments.Pro != 2 || st.Arguments.Con != 1 {
		t.Errorf("arguments: %+v", st.Arguments)
	}

	if len(body.Pro) != 2 || body.Pro[0].Text != "Pro 1" || body.Pro[1].Text != "Pro 2" {
		t.Errorf("pro side: %+v", body.Pro)
	}
	if len(body.Con) != 1 || body.Con[0].Text != "Con 1" || body.Con[0].Votes.Voters != 1 {
		t.Errorf("con side: %+v", body.Con)
	}
}

func TestFeedSorting(t *testing.T) {
	f := setup(t)

	var feed struct {
		Total      int
		Statements []statementJSON
	}
	get(t, f, "/statements", 200, &feed)
	if feed.Total != 2 || feed.Statements[0].Text != "Question" {
		t.Errorf("by weight: %+v", feed)
	}

	get(t, f, "/statements?sort=new", 200, &feed)
	if feed.Statements[0].Text != "Newer question" {
		t.Errorf("by new: first is %q", feed.Statements[0].Text)
	}

	get(t, f, "/statements?limit=1&offset=1", 200, &feed)
	if len(feed.Statements) != 1 || feed.Statements[0].Text != "Newer question" {
		t.Errorf("paging: %+v", feed.Statements)
	}

	get(t, f, "/statements?sort=random", 400, nil)
}

func TestVotesAndWallets(t *testing.T) {
	f := setup(t)

	var votes struct{ Votes []voterJSON }
	get(t, f, "/statements/"+friendly(f.root, false)+"/votes", 200, &votes)
	if len(votes.Votes) != 3 || votes.Votes[0].Wallet != friendly(f.carol, true) || votes.Votes[2].Wallet != friendly(f.alice, true) {
		t.Errorf("voters: %+v", votes.Votes)
	}

	// carol revoked her con vote completely: only the root vote remains
	var wallet struct{ Votes []walletVoteJSON }
	get(t, f, "/wallets/"+friendly(f.carol, true)+"/votes", 200, &wallet)
	if len(wallet.Votes) != 1 || wallet.Votes[0].Text != "Question" || wallet.Votes[0].Amount != nano(60).String() {
		t.Errorf("carol's votes: %+v", wallet.Votes)
	}

	var donations struct{ Received, Sent []donationJSON }
	get(t, f, "/wallets/"+friendly(f.author, true)+"/donations", 200, &donations)
	if len(donations.Received) != 1 || donations.Received[0].Donor != friendly(f.bob, true) || len(donations.Sent) != 0 {
		t.Errorf("author's donations: %+v", donations)
	}
	get(t, f, "/wallets/"+friendly(f.bob, true)+"/donations", 200, &donations)
	if len(donations.Sent) != 1 || donations.Sent[0].Amount != nano(2).String() {
		t.Errorf("bob's donations: %+v", donations)
	}
}

func TestWalletStatements(t *testing.T) {
	f := setup(t)

	var body struct{ Statements []statementJSON }
	get(t, f, "/wallets/"+friendly(f.author, true)+"/statements", 200, &body)
	if len(body.Statements) != 6 {
		t.Fatalf("expected all 6 statements of the author, got %d", len(body.Statements))
	}
	if body.Statements[0].Text != "Newer question" {
		t.Errorf("newest first: got %q", body.Statements[0].Text)
	}

	var hidden *statementJSON
	for i := range body.Statements {
		if body.Statements[i].Text == "Spam" {
			hidden = &body.Statements[i]
		}
	}
	if hidden == nil || hidden.Hidden == nil || *hidden.Hidden.Reason != "spam" {
		t.Errorf("the author must see their hidden statement with the reason: %+v", hidden)
	}

	get(t, f, "/wallets/"+friendly(f.bob, true)+"/statements", 200, &body)
	if len(body.Statements) != 0 {
		t.Errorf("bob wrote nothing: %+v", body.Statements)
	}
}

func TestModerationAndErrors(t *testing.T) {
	f := setup(t)

	var gone struct {
		Hidden bool
		Reason string
	}
	get(t, f, "/statements/"+friendly(f.c2, false), 410, &gone)
	if !gone.Hidden || gone.Reason != "spam" {
		t.Errorf("hidden statement: %+v", gone)
	}

	var log struct{ Hidden []map[string]any }
	get(t, f, "/moderation", 200, &log)
	if len(log.Hidden) != 1 || log.Hidden[0]["reason"] != "spam" {
		t.Errorf("moderation log: %+v", log)
	}

	get(t, f, "/statements/not-an-address", 400, nil)
	get(t, f, "/statements/"+friendly(addr("nothing"), false), 404, nil)

	// raw addresses are accepted too
	get(t, f, "/statements/"+f.root, 200, nil)
}

func TestDeletedStatement(t *testing.T) {
	f := setup(t)
	ctx := context.Background()

	// a third pro argument: alice votes for it, then the author deletes it
	p3 := addr("p3")
	err := f.st.Apply(ctx, func(tx store.Tx) error {
		if err := tx.InsertContract(ctx, &store.Contract{Address: p3, Kind: store.KindStatement, Parent: f.root, Idx: 2, Pro: true}); err != nil {
			return err
		}
		if err := tx.InsertStatement(ctx, store.Statement{Address: p3, Root: f.root, Parent: f.root, Depth: 1, Idx: 2, Pro: true,
			Author: f.author, Text: "Hasty", CreatedAt: day(6), CreatedLT: 1000}); err != nil {
			return err
		}
		if err := tx.InsertEvent(ctx, store.Event{Account: addr("vote-p3"), LT: 1001, TxHash: []byte{1}, Time: day(6),
			Kind: store.EventVoteAdded, Statement: p3, Wallet: f.alice, Amount: nano(4)}); err != nil {
			return err
		}
		return tx.MarkDeleted(ctx, p3, day(6.5))
	})
	if err != nil {
		t.Fatal(err)
	}

	// the root has arguments; p1 has none
	if err = f.st.Apply(ctx, func(tx store.Tx) error {
		return tx.UpdateCounters(ctx, &store.Contract{Address: f.root, NextPro: 3, NextCon: 2})
	}); err != nil {
		t.Fatal(err)
	}
	var flags struct {
		Statement struct{ Deletable bool }
		Pro       []struct {
			Address   string
			Deletable bool
		}
	}
	get(t, f, "/statements/"+friendly(f.root, false), 200, &flags)
	if flags.Statement.Deletable {
		t.Error("a statement with arguments must not be deletable")
	}
	for _, p := range flags.Pro {
		if !p.Deletable {
			t.Errorf("an argument without answers must be deletable: %s", p.Address)
		}
	}

	var gone struct{ Deleted bool }
	get(t, f, "/statements/"+friendly(p3, false), 410, &gone)
	if !gone.Deleted {
		t.Errorf("deleted statement: %+v", gone)
	}

	// it is not an argument of the root anymore
	var detail struct {
		Statement struct{ Arguments struct{ Pro int } }
		Pro       []struct{ Address string }
	}
	get(t, f, "/statements/"+friendly(f.root, false), 200, &detail)
	if detail.Statement.Arguments.Pro != 2 || len(detail.Pro) != 2 {
		t.Errorf("root still counts the deleted argument: %+v", detail)
	}
	for _, p := range detail.Pro {
		if p.Address == friendly(p3, false) {
			t.Error("deleted argument is listed")
		}
	}

	// the author's profile doesn't list it
	var mine struct{ Statements []struct{ Address string } }
	get(t, f, "/wallets/"+friendly(f.author, true)+"/statements", 200, &mine)
	for _, s := range mine.Statements {
		if s.Address == friendly(p3, false) {
			t.Error("deleted statement in the author's list")
		}
	}

	// but alice still sees her vote, without the text, to take the money back
	var votes struct {
		Votes []struct {
			Statement, Text, Amount string
			Deleted                 bool
		}
	}
	get(t, f, "/wallets/"+friendly(f.alice, true)+"/votes", 200, &votes)
	found := false
	for _, v := range votes.Votes {
		if v.Statement == friendly(p3, false) {
			found = true
			if !v.Deleted || v.Text != "" || v.Amount != nano(4).String() {
				t.Errorf("vote on a deleted statement: %+v", v)
			}
		}
	}
	if !found {
		t.Error("vote on a deleted statement is missing")
	}
}
