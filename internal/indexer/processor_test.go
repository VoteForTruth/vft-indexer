package indexer

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"testing"

	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/tvm/cell"

	"github.com/VoteForTruth/vft-indexer/internal/store"
	"github.com/VoteForTruth/vft-indexer/internal/store/storetest"
	"github.com/VoteForTruth/vft-indexer/internal/vft"
)

// The fixtures are real testnet transactions of the full scenario, see internal/vft/testdata.
var (
	wallet  = address.MustParseAddr("0QDYVkbXA-Iix_5RcZpa2RSFRnU_U5ik1GA9pP-RpytyST7z")
	master  = address.MustParseAddr("EQD8W1gEgzOT3CFO2XFe5DLIVxWWTzdalBzE1d-4jQBCT_mP")
	root    = address.MustParseAddr("EQAp_01uq2jzDZpTU9Pr5LI69KuQ-So0rQzOgJbFTbrdc0f4")
	deleted = address.MustParseAddr("EQC_Y9v1cteZt0h5FjieQT_5bGrei9DKEkLjo6tObC4uiyKV")
)

func testStore(t *testing.T) *store.Store {
	return storetest.Open(t)
}

// scenarioTxs returns all fixture transactions in global logical time order,
// the order in which the block scanner delivers them.
func scenarioTxs(t *testing.T) []vft.Tx {
	t.Helper()

	var all []vft.Tx
	for _, name := range []string{"master", "root", "arg_pro", "arg_con", "arg_deleted", "vote", "vote_deleted"} {
		raw, err := os.ReadFile("../vft/testdata/" + name + ".json")
		if err != nil {
			t.Fatal(err)
		}
		var f struct{ Transactions []struct{ Boc string } }
		if err = json.Unmarshal(raw, &f); err != nil {
			t.Fatal(err)
		}
		for _, rawTx := range f.Transactions {
			boc, _ := base64.StdEncoding.DecodeString(rawTx.Boc)
			c, err := cell.FromBOC(boc)
			if err != nil {
				t.Fatal(err)
			}
			var tlbTx tlb.Transaction
			if err = tlb.LoadFromCell(&tlbTx, c.MustBeginParse()); err != nil {
				t.Fatal(err)
			}
			tlbTx.Hash = c.Hash()
			tx, err := vft.DecodeTx(&tlbTx)
			if err != nil {
				t.Fatal(err)
			}
			all = append(all, tx)
		}
	}

	sort.Slice(all, func(i, j int) bool { return all[i].LT < all[j].LT })
	return all
}

func applyAll(t *testing.T, p *Processor, txs []vft.Tx) {
	t.Helper()
	for _, tx := range txs {
		if !p.IsKnown(tx.Account) {
			t.Fatalf("tx %d of %s: account must be discovered before its transactions", tx.LT, Raw(tx.Account))
		}
		if err := p.Apply(context.Background(), tx); err != nil {
			t.Fatal(err)
		}
	}
}

func TestProcessorBuildsTreeAndEvents(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	code, err := vft.LoadCode()
	if err != nil {
		t.Fatal(err)
	}
	// the fixtures must be recorded on the current contracts (see internal/vft)
	if !code.MasterAddress(wallet).Equals(master) {
		t.Skip("testdata was recorded on an older contract version; re-record the scenario on the current master")
	}

	p, err := NewProcessor(ctx, code, st, master, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	txs := scenarioTxs(t)
	applyAll(t, p, txs)

	// the tree, the deleted argument included (it stays for its voters)
	rows, err := st.DB().QueryContext(ctx, `select address, root, depth, pro, author, text, coalesce(url, ''), deleted_at is not null
		from statements order by depth, pro desc, idx`)
	if err != nil {
		t.Fatal(err)
	}
	type row struct {
		address, root string
		depth         int
		pro           bool
		author, text  string
		url           string
		deleted       bool
	}
	var got []row
	for rows.Next() {
		var r row
		if err = rows.Scan(&r.address, &r.root, &r.depth, &r.pro, &r.author, &r.text, &r.url, &r.deleted); err != nil {
			t.Fatal(err)
		}
		got = append(got, r)
	}
	if len(got) != 4 {
		t.Fatalf("expected 4 statements, got %+v", got)
	}
	// the current link is the last one set by the author
	if got[0].address != Raw(root) || got[0].depth != 0 || got[0].text != "Тестовое утверждение" || got[0].url != "https://web.archive.org" {
		t.Errorf("root: %+v", got[0])
	}
	if got[1].text != "Однозначно да" || !got[1].pro || got[1].deleted || got[1].root != Raw(root) {
		t.Errorf("pro argument: %+v", got[1])
	}
	if got[2].address != Raw(deleted) || got[2].text != "Delete me" || !got[2].pro || !got[2].deleted {
		t.Errorf("deleted argument: %+v", got[2])
	}
	if got[3].text != "Нафиг" || got[3].pro || got[3].depth != 1 || got[3].deleted {
		t.Errorf("con argument: %+v", got[3])
	}
	for _, r := range got {
		if r.author != Raw(wallet) {
			t.Errorf("author of %s: %s", r.text, r.author)
		}
	}

	// the public history of the root's link: created without one, replaced twice
	var history []string
	hRows, err := st.DB().QueryContext(ctx, `select coalesce(url, '-') from url_history where statement = ?1 order by lt`, Raw(root))
	if err != nil {
		t.Fatal(err)
	}
	for hRows.Next() {
		var u string
		if err = hRows.Scan(&u); err != nil {
			t.Fatal(err)
		}
		history = append(history, u)
	}
	if fmt.Sprint(history) != "[- https://example.com https://web.archive.org]" {
		t.Errorf("link history %v", history)
	}

	// the events
	evRows, err := st.DB().QueryContext(ctx, `select kind, statement, wallet, coalesce(amount, '') from events order by lt`)
	if err != nil {
		t.Fatal(err)
	}
	var events []string
	for evRows.Next() {
		var kind, statement, w, amount string
		if err = evRows.Scan(&kind, &statement, &w, &amount); err != nil {
			t.Fatal(err)
		}
		if w != Raw(wallet) {
			t.Errorf("%s: wallet %s", kind, w)
		}
		if kind == string(store.EventDonation) {
			amount = "donation" // the exact value depends on fees
		}
		on := "root"
		if statement == Raw(deleted) {
			on = "deleted"
		} else if statement != Raw(root) {
			on = statement
		}
		events = append(events, on+" "+kind+":"+amount)
	}
	expected := []string{
		"deleted vote_added:100000000",
		"deleted vote_revoked_all:",
		"root vote_added:500000000",
		"root vote_added:200000000",
		"root vote_withdrawn:300000000",
		"root vote_revoked_all:",
		"root donation:donation",
	}
	if fmt.Sprint(events) != fmt.Sprint(expected) {
		t.Fatalf("events\n%v\nexpected\n%v", events, expected)
	}

	// counters mirror the contracts: a deleted argument still counts
	contracts, err := st.LoadContracts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if m := contracts[Raw(master)]; m.NextIdx != 1 {
		t.Errorf("master next_idx %d", m.NextIdx)
	}
	if r := contracts[Raw(root)]; r.NextPro != 2 || r.NextCon != 1 {
		t.Errorf("root counters %d/%d", r.NextPro, r.NextCon)
	}
	if len(contracts) != 7 {
		t.Errorf("expected master, 4 statements and 2 votes, got %d contracts", len(contracts))
	}

	// replaying the same transactions (e.g. after a crash) changes nothing
	p2, err := NewProcessor(ctx, code, st, master, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	applyAll(t, p2, txs)
	var count int
	if err = st.DB().QueryRowContext(ctx, `select (select count(*) from events) + (select count(*) from statements)`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 11 {
		t.Errorf("replay duplicated data: %d rows", count)
	}
}
