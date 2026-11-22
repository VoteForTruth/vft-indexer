package vft

import (
	"encoding/base64"
	"encoding/json"
	"math/big"
	"os"
	"testing"

	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/tvm/cell"
)

// testdata/*.json are real testnet transactions of the full scenario:
// deploy master, create a root statement, a pro and a con argument,
// vote 0.5, add 0.2, revoke 0.3, revoke all, donate 0.1.
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
	vote, _ := loadFixture(t, "vote")

	cases := []struct {
		name     string
		derived  *address.Address
		expected *address.Address
	}{
		{"master", code.MasterAddress(wallet), master},
		{"root", code.StatementAddress(master, 0, true), root},
		{"pro argument", code.StatementAddress(root, 0, true), argPro},
		{"con argument", code.StatementAddress(root, 0, false), argCon},
		{"vote", code.VoteAddress(root, wallet), vote},
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
	if len(txs) != 2 {
		t.Fatalf("expected deploy + 1 statement, got %d txs", len(txs))
	}

	deploy := txs[0]
	if !deploy.Success || deploy.In.Body.BitsSize() != 0 {
		t.Errorf("deploy: success=%v, expected an empty body", deploy.Success)
	}

	create := txs[1]
	msg, ok := parse(t, create).(NewParentStatement)
	if !ok {
		t.Fatalf("expected NewParentStatement, got %T", parse(t, create))
	}
	if !create.Success || !create.In.SrcAddr.Equals(wallet) {
		t.Errorf("create: success=%v from=%s", create.Success, create.In.SrcAddr)
	}
	if msg.Data.Statement != "Тестовый вопрос" || msg.Data.URL == nil || *msg.Data.URL != "https://example.com" {
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
	for _, tx := range txs {
		if !tx.Success {
			t.Errorf("tx %d failed", tx.LT)
		}

		switch msg := parse(t, tx).(type) {
		case SetStatementData:
			kinds = append(kinds, "set")
			if !tx.In.SrcAddr.Equals(master) || !msg.Owner.Equals(wallet) || msg.Data.Statement != "Тестовый вопрос" {
				t.Errorf("SetStatementData: from %s owner %s %q", tx.In.SrcAddr, msg.Owner, msg.Data.Statement)
			}
		case NewChildStatement:
			kinds = append(kinds, "child")
			expected := code.StatementAddress(root, 0, msg.Pro)
			if len(tx.Out) != 1 || !tx.Out[0].DstAddr.Equals(expected) {
				t.Errorf("NewChildStatement pro=%v: wrong destination", msg.Pro)
			}
			if msg.Data.URL != nil {
				t.Errorf("argument url should be null, got %q", *msg.Data.URL)
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
		default:
			t.Errorf("unexpected %T", msg)
		}
	}

	expected := []string{"set", "child", "child", "vote", "vote", "donate"}
	if len(kinds) != len(expected) {
		t.Fatalf("kinds %v, expected %v", kinds, expected)
	}
	for i := range expected {
		if kinds[i] != expected[i] {
			t.Fatalf("kinds %v, expected %v", kinds, expected)
		}
	}

	if v := parse(t, txs[3]).(Vote).Amount; v.Cmp(ton("0.5")) != 0 {
		t.Errorf("first vote %s", v)
	}
	if v := parse(t, txs[4]).(Vote).Amount; v.Cmp(ton("0.2")) != 0 {
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
