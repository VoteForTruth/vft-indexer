package vft

import (
	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/tvm/cell"
)

// Contracts are deployed with a canonical initial storage, so their addresses
// depend only on a few parameters. These functions mirror the storage layouts
// in vft-contract/contracts/types.tolk and must be kept in sync with them.

const workchain = 0

// MasterAddress derives the master address: MasterStorage { owner, statementsCount: 0 }.
func (c Code) MasterAddress(owner *address.Address) *address.Address {
	data := cell.BeginCell().
		MustStoreAddr(owner).
		MustStoreUInt(0, 32).
		EndCell()

	return stateInitAddress(c.Master, data)
}

// StatementAddress derives the address of a statement:
// StatementStorage { parent, idx, pro, owner: null, data: null, proCount: 0, conCount: 0 }.
func (c Code) StatementAddress(parent *address.Address, idx uint32, pro bool) *address.Address {
	data := cell.BeginCell().
		MustStoreAddr(parent).
		MustStoreUInt(uint64(idx), 32).
		MustStoreBoolBit(pro).
		MustStoreAddr(address.NewAddressNone()).
		MustStoreMaybeRef(nil).
		MustStoreUInt(0, 32).
		MustStoreUInt(0, 32).
		EndCell()

	return stateInitAddress(c.Statement, data)
}

// VoteAddress derives the address of a vote: VoteStorage { statement, owner, amount: 0 }.
func (c Code) VoteAddress(statement, owner *address.Address) *address.Address {
	data := cell.BeginCell().
		MustStoreAddr(statement).
		MustStoreAddr(owner).
		MustStoreCoins(0).
		EndCell()

	return stateInitAddress(c.Vote, data)
}

func stateInitAddress(code, data *cell.Cell) *address.Address {
	return tlb.StateInit{Code: code, Data: data}.CalcAddress(workchain)
}
