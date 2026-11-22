package indexer

import (
	"context"
	"encoding/base64"
	"encoding/json"
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
	wallet = address.MustParseAddr("0QDYVkbXA-Iix_5RcZpa2RSFRnU_U5ik1GA9pP-RpytyST7z")
	master = address.MustParseAddr("kQAtLOtQSvcqzpgpgmRoymv4BN5w5lcgF1VN6Ob0F4J0STiU")
	root   = address.MustParseAddr("kQC-MCQhctLfNy4pUxT6kpeugXtaO3ONJ-XazF-ptjHfuEQ5")
)

func testStore(t *testing.T) *store.Store {
	return storetest.Open(t, "indexer_test")
}

// scenarioTxs returns all fixture transactions in global logical time order,
// the order in which the block scanner delivers them.
func scenarioTxs(t *testing.T) []vft.Tx {
	t.Helper()

	var all []vft.Tx
	for _, name := range []string{"master", "root", "arg_pro", "arg_con", "vote"} {
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

	p, err := NewProcessor(ctx, code, st, master, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	txs := scenarioTxs(t)
	applyAll(t, p, txs)

	// the tree
	rows, err := st.Pool().Query(ctx, `select address, root, depth, pro, author, text, coalesce(url, '') from statements order by depth, pro desc`)
	if err != nil {
		t.Fatal(err)
	}
	type row struct {
		address, root string
		depth         int
		pro           bool
		author, text  string
		url           string
	}
	var got []row
	for rows.Next() {
		var r row
		if err = rows.Scan(&r.address, &r.root, &r.depth, &r.pro, &r.author, &r.text, &r.url); err != nil {
			t.Fatal(err)
		}
		got = append(got, r)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 statements, got %+v", got)
	}
	if got[0].address != Raw(root) || got[0].depth != 0 || got[0].text != "Тестовый вопрос" || got[0].url != "https://example.com" {
		t.Errorf("root: %+v", got[0])
	}
	if got[1].text != "Аргумент за" || !got[1].pro || got[1].depth != 1 || got[1].root != Raw(root) {
		t.Errorf("pro argument: %+v", got[1])
	}
	if got[2].text != "Аргумент против" || got[2].pro || got[2].depth != 1 || got[2].root != Raw(root) {
		t.Errorf("con argument: %+v", got[2])
	}
	for _, r := range got {
		if r.author != Raw(wallet) {
			t.Errorf("author of %s: %s", r.text, r.author)
		}
	}

	// the events
	evRows, err := st.Pool().Query(ctx, `select kind, statement, wallet, coalesce(amount::text, '') from events order by lt`)
	if err != nil {
		t.Fatal(err)
	}
	var events []string
	for evRows.Next() {
		var kind, statement, w, amount string
		if err = evRows.Scan(&kind, &statement, &w, &amount); err != nil {
			t.Fatal(err)
		}
		if statement != Raw(root) || w != Raw(wallet) {
			t.Errorf("%s: statement %s wallet %s", kind, statement, w)
		}
		if kind == string(store.EventDonation) {
			amount = "donation" // the exact value depends on fees
		}
		events = append(events, kind+":"+amount)
	}
	expected := []string{
		"vote_added:500000000",
		"vote_added:200000000",
		"vote_withdrawn:300000000",
		"vote_revoked_all:",
		"donation:donation",
	}
	if len(events) != len(expected) {
		t.Fatalf("events %v, expected %v", events, expected)
	}
	for i := range expected {
		if events[i] != expected[i] {
			t.Fatalf("events %v, expected %v", events, expected)
		}
	}

	// counters mirror the contracts
	contracts, err := st.LoadContracts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if m := contracts[Raw(master)]; m.NextIdx != 1 {
		t.Errorf("master next_idx %d", m.NextIdx)
	}
	if r := contracts[Raw(root)]; r.NextPro != 1 || r.NextCon != 1 {
		t.Errorf("root counters %d/%d", r.NextPro, r.NextCon)
	}
	if len(contracts) != 5 {
		t.Errorf("expected master, 3 statements and 1 vote, got %d contracts", len(contracts))
	}

	// replaying the same transactions (e.g. after a crash) changes nothing
	p2, err := NewProcessor(ctx, code, st, master, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	applyAll(t, p2, txs)
	var count int
	if err = st.Pool().QueryRow(ctx, `select (select count(*) from events) + (select count(*) from statements)`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 8 {
		t.Errorf("replay duplicated data: %d rows", count)
	}
}
