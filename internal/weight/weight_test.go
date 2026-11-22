package weight

import (
	"math/big"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)

func ton(n int64) *big.Int { return big.NewInt(n * 1_000_000_000) }

func day(n float64) time.Time { return t0.Add(time.Duration(n * float64(24*time.Hour))) }

func check(t *testing.T, name string, got, want *big.Int) {
	t.Helper()
	if got.Cmp(want) != 0 {
		t.Errorf("%s: got %s, want %s", name, got, want)
	}
}

func TestLinearGrowth(t *testing.T) {
	var v Vote
	v.Add(ton(70), t0)

	check(t, "at once", v.Weight(t0), big.NewInt(0))
	check(t, "after 1 day", v.Weight(day(1)), ton(10))
	check(t, "after 3.5 days", v.Weight(day(3.5)), ton(35))
	check(t, "after 7 days", v.Weight(day(7)), ton(70))
	check(t, "after 30 days", v.Weight(day(30)), ton(70))
	check(t, "amount", v.Amount(), ton(70))
}

func TestLotsMatureSeparately(t *testing.T) {
	var v Vote
	v.Add(ton(70), t0)
	v.Add(ton(70), day(7))

	// the first lot is mature, the second one is 1 day old
	check(t, "weight", v.Weight(day(8)), ton(80))
	check(t, "amount", v.Amount(), ton(140))
}

func TestWithdrawTakesNewestFirst(t *testing.T) {
	var v Vote
	v.Add(ton(10), t0)        // old honest vote
	v.Add(ton(1000), day(10)) // a whale adds right before a news peak

	check(t, "during the peak", v.Weight(day(10.7)), ton(110))

	v.Withdraw(ton(1000)) // and pulls out
	check(t, "after the peak", v.Weight(day(11)), ton(10))
	check(t, "amount", v.Amount(), ton(10))
	if !v.Since().Equal(t0) {
		t.Errorf("the old lot must stay: since %s", v.Since())
	}
}

func TestPartialWithdrawSplitsALot(t *testing.T) {
	var v Vote
	v.Add(ton(10), t0)
	v.Add(ton(20), day(1))

	v.Withdraw(ton(25)) // all of the newest lot and 5 of the oldest

	check(t, "amount", v.Amount(), ton(5))
	check(t, "weight", v.Weight(day(7)), ton(5))
}

func TestRevokeAll(t *testing.T) {
	var v Vote
	v.Add(ton(10), t0)
	v.Add(ton(20), day(1))
	v.RevokeAll()

	check(t, "amount", v.Amount(), big.NewInt(0))
	check(t, "weight", v.Weight(day(30)), big.NewInt(0))
	if !v.Since().IsZero() {
		t.Errorf("empty vote must have no since")
	}

	v.Add(ton(1), day(2)) // voting again after a full revoke starts from scratch
	check(t, "new vote", v.Weight(day(9)), ton(1))
}
