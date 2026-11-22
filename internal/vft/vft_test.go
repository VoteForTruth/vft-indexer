package vft

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"testing"

	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/tvm/cell"
)

// testdata/*.json are real testnet transactions of the full scenario (master
// kQD8W1…, contracts 2.2.0): deploy master (three attempts, the first one
// deploys), create a root statement, a pro argument, a pro argument "Delete me"
// that gets a 0.1 vote and is deleted (the vote is then withdrawn), a con
// argument; vote 0.5 for the root, add 0.2, revoke 0.3, revoke all, donate 0.1,
// replace the root's link twice. Transactions from strangers' wallets (testnet
// bots probing new contracts) are left out. Destroyed accounts were fetched
// starting from a known transaction (toncenter v2 getTransactions with lt+hash).
var wallet = address.MustParseAddr("0QDYVkbXA-Iix_5RcZpa2RSFRnU_U5ik1GA9pP-RpytyST7z")

type fixture struct {
	Address      string
	Transactions []struct {
		Boc string
	}
}

func loadFixture(t *testing.T, name string) (*address.Address, []Tx) {
	t.Helper()

	raw, err := os.ReadFile("testdata/" + name + ".json")
	if err != nil {
		t.Fatal(err)
	}

	var f fixture
	if err = json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}

	var txs []Tx
	for _, rawTx := range f.Transactions {
		boc, err := base64.StdEncoding.DecodeString(rawTx.Boc)
		if err != nil {
			t.Fatal(err)
		}
		c, err := cell.FromBOC(boc)
		if err != nil {
			t.Fatal(err)
		}

		var tlbTx tlb.Transaction
		if err = tlb.LoadFromCell(&tlbTx, c.MustBeginParse()); err != nil {
			t.Fatal(err)
		}
		tlbTx.Hash = c.Hash()

		tx, err := DecodeTx(&tlbTx)
		if err != nil {
			t.Fatal(err)
		}
		txs = append(txs, tx)
	}

	return address.MustParseAddr(f.Address), txs
}

func mustCode(t *testing.T) Code {
	t.Helper()
	code, err := LoadCode()
	if err != nil {
		t.Fatal(err)
	}
	return code
}

func parse(t *testing.T, tx Tx) any {
	t.Helper()
	if tx.In == nil {
		t.Fatalf("tx %d has no internal in-message", tx.LT)
	}
	msg, err := ParseMessage(tx.In.Body)
	if err != nil {
		t.Fatalf("tx %d: %v", tx.LT, err)
	}
	return msg
}

func ton(s string) *big.Int {
	v, ok := new(big.Rat).SetString(s)
	if !ok {
		panic(s)
	}
	return new(big.Int).Div(new(big.Int).Mul(v.Num(), big.NewInt(1_000_000_000)), v.Denom())
}

func TestDerivedAddressesMatchTestnet(t *testing.T) {
	code := mustCode(t)

	master, _ := loadFixture(t, "master")
	root, _ := loadFixture(t, "root")
	argPro, _ := loadFixture(t, "arg_pro")
	argCon, _ := loadFixture(t, "arg_con")
	argDeleted, _ := loadFixture(t, "arg_deleted")
	vote, _ := loadFixture(t, "vote")
	voteDeleted, _ := loadFixture(t, "vote_deleted")

	cases := []struct {
		name     string
		derived  *address.Address
		expected *address.Address
	}{
		{"master", code.MasterAddress(wallet), master},
		{"root", code.StatementAddress(master, 0, true), root},
		{"pro argument", code.StatementAddress(root, 0, true), argPro},
		{"deleted pro argument", code.StatementAddress(root, 1, true), argDeleted},
		{"con argument", code.StatementAddress(root, 0, false), argCon},
		{"vote", code.VoteAddress(root, wallet), vote},
		{"vote on the deleted argument", code.VoteAddress(argDeleted, wallet), voteDeleted},
	}
	for _, c := range cases {
		if !c.derived.Equals(c.expected) {
			t.Errorf("%s: derived %s, on-chain %s", c.name, c.derived.StringRaw(), c.expected.StringRaw())
		}
	}
}

func TestMasterTransactions(t *testing.T) {
	code := mustCode(t)
	master, txs := loadFixture(t, "master")
	if len(txs) < 2 {
		t.Fatalf("expected deploys + 1 statement, got %d txs", len(txs))
	}

	// the deploy, repeated by the deploy script; the repeats are plain transfers
	for _, deploy := range txs[:len(txs)-1] {
		if !deploy.Success || deploy.In.Body.BitsSize() != 0 {
			t.Errorf("deploy %d: success=%v, expected an empty body", deploy.LT, deploy.Success)
		}
	}

	create := txs[len(txs)-1]
	msg, ok := parse(t, create).(NewParentStatement)
	if !ok {
		t.Fatalf("expected NewParentStatement, got %T", parse(t, create))
	}
	if !create.Success || !create.In.SrcAddr.Equals(wallet) {
		t.Errorf("create: success=%v from=%s", create.Success, create.In.SrcAddr)
	}
	if msg.Data.Statement != "Тестовое утверждение" || msg.Data.URL != nil {
		t.Errorf("data: %q %v", msg.Data.Statement, msg.Data.URL)
	}
	if len(create.Out) != 1 || !create.Out[0].DstAddr.Equals(code.StatementAddress(master, 0, true)) {
		t.Errorf("expected one out message to statement #0")
	}
}

func TestStatementTransactions(t *testing.T) {
	code := mustCode(t)
	master, _ := loadFixture(t, "master")
	root, txs := loadFixture(t, "root")

	var kinds []string
	var urls []string
	next := map[bool]int{} // next argument index per side
	for _, tx := range txs {
		if !tx.Success {
			t.Errorf("tx %d failed", tx.LT)
		}

		switch msg := parse(t, tx).(type) {
		case SetStatementData:
			kinds = append(kinds, "set")
			if !tx.In.SrcAddr.Equals(master) || !msg.Owner.Equals(wallet) || msg.Data.Statement != "Тестовое утверждение" {
				t.Errorf("SetStatementData: from %s owner %s %q", tx.In.SrcAddr, msg.Owner, msg.Data.Statement)
			}
		case NewChildStatement:
			kinds = append(kinds, "child")
			expected := code.StatementAddress(root, uint32(next[msg.Pro]), msg.Pro)
			next[msg.Pro]++
			if len(tx.Out) != 1 || !tx.Out[0].DstAddr.Equals(expected) {
				t.Errorf("NewChildStatement %q: wrong destination", msg.Data.Statement)
			}
		case Vote:
			kinds = append(kinds, "vote")
			if len(tx.Out) != 1 || !tx.Out[0].DstAddr.Equals(code.VoteAddress(root, wallet)) {
				t.Errorf("Vote: wrong destination")
			}
		case Donate:
			kinds = append(kinds, "donate")
			if len(tx.Out) != 1 || !tx.Out[0].DstAddr.Equals(wallet) {
				t.Errorf("Donate: expected a payout to the author")
			}
		case SetUrl:
			kinds = append(kinds, "url")
			if !tx.In.SrcAddr.Equals(wallet) || msg.URL == nil {
				t.Errorf("SetUrl: from %s url %v", tx.In.SrcAddr, msg.URL)
			} else {
				urls = append(urls, *msg.URL)
			}
		default:
			t.Errorf("unexpected %T", msg)
		}
	}

	expected := []string{"set", "child", "child", "child", "vote", "vote", "donate", "url", "url"}
	if fmt.Sprint(kinds) != fmt.Sprint(expected) {
		t.Fatalf("kinds %v, expected %v", kinds, expected)
	}
	if fmt.Sprint(urls) != "[https://example.com https://web.archive.org]" {
		t.Errorf("links %v", urls)
	}
	if v := parse(t, txs[4]).(Vote).Amount; v.Cmp(ton("0.5")) != 0 {
		t.Errorf("first vote %s", v)
	}
	if v := parse(t, txs[5]).(Vote).Amount; v.Cmp(ton("0.2")) != 0 {
		t.Errorf("second vote %s", v)
	}
}

func TestVoteTransactions(t *testing.T) {
	root, _ := loadFixture(t, "root")
	_, txs := loadFixture(t, "vote")
	if len(txs) != 4 {
		t.Fatalf("expected 4 vote txs, got %d", len(txs))
	}

	for _, tx := range txs {
		if !tx.Success {
			t.Errorf("tx %d failed", tx.LT)
		}
	}

	add1, ok1 := parse(t, txs[0]).(AddVote)
	add2, ok2 := parse(t, txs[1]).(AddVote)
	withdraw, ok3 := parse(t, txs[2]).(Withdraw)
	_, ok4 := parse(t, txs[3]).(WithdrawAll)
	if !ok1 || !ok2 || !ok3 || !ok4 {
		t.Fatalf("unexpected message types")
	}

	if add1.Amount.Cmp(ton("0.5")) != 0 || add2.Amount.Cmp(ton("0.2")) != 0 || withdraw.Amount.Cmp(ton("0.3")) != 0 {
		t.Errorf("amounts: %s %s %s", add1.Amount, add2.Amount, withdraw.Amount)
	}
	if !txs[0].In.SrcAddr.Equals(root) || !txs[2].In.SrcAddr.Equals(wallet) {
		t.Errorf("senders: AddVote from %s, Withdraw from %s", txs[0].In.SrcAddr, txs[2].In.SrcAddr)
	}
}

func TestDeletionTransactions(t *testing.T) {
	code := mustCode(t)
	root, _ := loadFixture(t, "root")
	arg, txs := loadFixture(t, "arg_deleted")
	if len(txs) != 3 {
		t.Fatalf("expected set, vote, delete; got %d txs", len(txs))
	}
	for _, tx := range txs {
		if !tx.Success {
			t.Errorf("tx %d failed", tx.LT)
		}
	}

	if _, ok := parse(t, txs[0]).(SetStatementData); !ok || !txs[0].In.SrcAddr.Equals(root) {
		t.Errorf("first: %T from %s", parse(t, txs[0]), txs[0].In.SrcAddr)
	}
	if v, ok := parse(t, txs[1]).(Vote); !ok || v.Amount.Cmp(ton("0.1")) != 0 ||
		len(txs[1].Out) != 1 || !txs[1].Out[0].DstAddr.Equals(code.VoteAddress(arg, wallet)) {
		t.Errorf("vote: %#v", parse(t, txs[1]))
	}
	del := txs[2]
	if _, ok := parse(t, del).(DeleteStatement); !ok || !del.In.SrcAddr.Equals(wallet) {
		t.Errorf("delete: %T from %s", parse(t, del), del.In.SrcAddr)
	}
	// the reserve goes back to the author
	if len(del.Out) != 1 || !del.Out[0].DstAddr.Equals(wallet) {
		t.Errorf("delete: expected a refund to the author")
	}

	// the vote outlives the statement and is withdrawn by its owner
	_, votes := loadFixture(t, "vote_deleted")
	if len(votes) != 2 {
		t.Fatalf("expected 2 txs of the vote, got %d", len(votes))
	}
	if a, ok := parse(t, votes[0]).(AddVote); !ok || a.Amount.Cmp(ton("0.1")) != 0 || !votes[0].In.SrcAddr.Equals(arg) {
		t.Errorf("AddVote: %#v", parse(t, votes[0]))
	}
	if _, ok := parse(t, votes[1]).(WithdrawAll); !ok || !votes[1].Success || !votes[1].In.SrcAddr.Equals(wallet) {
		t.Errorf("WithdrawAll: %#v", parse(t, votes[1]))
	}
}

func TestParseDeleteStatement(t *testing.T) {
	body := cell.BeginCell().MustStoreUInt(OpDeleteStatement, 32).EndCell()
	msg, err := ParseMessage(body)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := msg.(DeleteStatement); !ok {
		t.Errorf("got %T, want DeleteStatement", msg)
	}
}

func TestParseSetUrl(t *testing.T) {
	link := cell.BeginCell().MustStoreStringSnake("https://mirror.example").EndCell()
	body := cell.BeginCell().MustStoreUInt(OpSetUrl, 32).MustStoreMaybeRef(link).EndCell()
	msg, err := ParseMessage(body)
	if err != nil {
		t.Fatal(err)
	}
	if m, ok := msg.(SetUrl); !ok || m.URL == nil || *m.URL != "https://mirror.example" {
		t.Errorf("got %#v", msg)
	}

	removed := cell.BeginCell().MustStoreUInt(OpSetUrl, 32).MustStoreMaybeRef(nil).EndCell()
	if msg, err = ParseMessage(removed); err != nil || msg.(SetUrl).URL != nil {
		t.Errorf("removed link: %#v, %v", msg, err)
	}
}

func TestParseMessageRejectsUnknown(t *testing.T) {
	unknown := cell.BeginCell().MustStoreUInt(0x12345678, 32).EndCell()
	if _, err := ParseMessage(unknown); err != ErrUnknownMessage {
		t.Errorf("expected ErrUnknownMessage, got %v", err)
	}

	hello := cell.BeginCell().MustStoreUInt(0, 32).MustStoreStringSnake("hello").EndCell()
	if _, err := ParseMessage(hello); err != ErrUnknownMessage {
		t.Errorf("a non-donate comment must be unknown, got %v", err)
	}

	if _, err := ParseMessage(cell.BeginCell().EndCell()); err != ErrUnknownMessage {
		t.Errorf("an empty body must be unknown, got %v", err)
	}
}
