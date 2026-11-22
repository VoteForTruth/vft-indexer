// Package weight computes the time-weighted strength of votes.
//
// Every addition to a vote is a separate lot. A lot gains weight linearly and
// reaches its full amount after Maturity. Revoking takes the newest lots first
// (LIFO): money added right before a news peak and pulled out soon after never
// weighs much, while a long-standing vote keeps its matured weight.
package weight

import (
	"math/big"
	"time"
)

// Maturity is the time it takes for a lot to reach its full weight.
const Maturity = 7 * 24 * time.Hour

type lot struct {
	amount *big.Int
	time   time.Time
}

// Vote is the state of one wallet's vote for one statement.
type Vote struct {
	lots []lot
}

// Add records money added to the vote at time t.
func (v *Vote) Add(amount *big.Int, t time.Time) {
	v.lots = append(v.lots, lot{amount: new(big.Int).Set(amount), time: t})
}

// Withdraw removes `amount` starting from the newest lots.
func (v *Vote) Withdraw(amount *big.Int) {
	left := new(big.Int).Set(amount)
	for len(v.lots) > 0 && left.Sign() > 0 {
		last := &v.lots[len(v.lots)-1]
		if last.amount.Cmp(left) > 0 {
			last.amount = new(big.Int).Sub(last.amount, left)
			return
		}
		left.Sub(left, last.amount)
		v.lots = v.lots[:len(v.lots)-1]
	}
}

// RevokeAll removes the whole vote.
func (v *Vote) RevokeAll() {
	v.lots = nil
}

// Amount is the money currently in the vote, in nanotons.
func (v *Vote) Amount() *big.Int {
	sum := new(big.Int)
	for _, l := range v.lots {
		sum.Add(sum, l.amount)
	}
	return sum
}

// Weight is the time-weighted amount at `now`, in nanotons.
func (v *Vote) Weight(now time.Time) *big.Int {
	sum := new(big.Int)
	for _, l := range v.lots {
		sum.Add(sum, lotWeight(l, now))
	}
	return sum
}

// Since is when the oldest money still in the vote was added; zero if empty.
func (v *Vote) Since() time.Time {
	if len(v.lots) == 0 {
		return time.Time{}
	}
	return v.lots[0].time
}

func lotWeight(l lot, now time.Time) *big.Int {
	age := now.Sub(l.time)
	if age >= Maturity {
		return new(big.Int).Set(l.amount)
	}
	if age <= 0 {
		return new(big.Int)
	}
	w := new(big.Int).Mul(l.amount, big.NewInt(int64(age/time.Second)))
	return w.Div(w, big.NewInt(int64(Maturity/time.Second)))
}
