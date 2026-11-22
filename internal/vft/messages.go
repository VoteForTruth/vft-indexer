package vft

import (
	"errors"
	"fmt"
	"math/big"

	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/tvm/cell"
)

// Opcodes from vft-contract/contracts/types.tolk.
const (
	OpNewParentStatement uint64 = 0x56f40001
	OpNewChildStatement  uint64 = 0x56f40002
	OpSetStatementData   uint64 = 0x56f40003
	OpChildCreated       uint64 = 0x56f40004
	OpVote               uint64 = 0x56f40005
	OpAddVote            uint64 = 0x56f40006
	OpWithdraw           uint64 = 0x56f40007
	OpWithdrawAll        uint64 = 0x56f40008
	OpDeleteStatement    uint64 = 0x56f40009
	OpTextComment        uint64 = 0x00000000
)

const DonateComment = "donate"

// ErrUnknownMessage is returned for bodies that are not VFT messages.
var ErrUnknownMessage = errors.New("unknown message")

// Data is the content of a statement.
type Data struct {
	Statement string
	URL       *string
}

type NewParentStatement struct {
	Data Data
}

type NewChildStatement struct {
	Data Data
	Pro  bool
}

type SetStatementData struct {
	Owner *address.Address
	Data  Data
}

type Vote struct {
	Amount *big.Int
}

type AddVote struct {
	Amount *big.Int
}

type Withdraw struct {
	Amount *big.Int
}

type WithdrawAll struct{}

// DeleteStatement: the author deletes a statement that has no arguments.
type DeleteStatement struct{}

// Donate is the "donate" text comment.
type Donate struct{}

// ParseMessage decodes an incoming message body of a VFT contract.
// It returns one of the message types above or ErrUnknownMessage.
func ParseMessage(body *cell.Cell) (any, error) {
	if body == nil {
		return nil, ErrUnknownMessage
	}

	s, err := body.BeginParse()
	if err != nil {
		return nil, fmt.Errorf("parse body: %w", err)
	}
	if s.BitsLeft() < 32 {
		return nil, ErrUnknownMessage
	}
	op := s.MustLoadUInt(32)

	var msg any
	switch op {
	case OpNewParentStatement:
		var d Data
		d, err = loadData(s)
		msg = NewParentStatement{Data: d}
	case OpNewChildStatement:
		var d Data
		var pro bool
		if d, err = loadData(s); err == nil {
			pro, err = s.LoadBoolBit()
		}
		msg = NewChildStatement{Data: d, Pro: pro}
	case OpSetStatementData:
		var owner *address.Address
		var d Data
		if owner, err = s.LoadAddr(); err == nil {
			d, err = loadData(s)
		}
		msg = SetStatementData{Owner: owner, Data: d}
	case OpVote:
		var amount *big.Int
		amount, err = s.LoadBigCoins()
		msg = Vote{Amount: amount}
	case OpAddVote:
		var amount *big.Int
		amount, err = s.LoadBigCoins()
		msg = AddVote{Amount: amount}
	case OpWithdraw:
		var amount *big.Int
		amount, err = s.LoadBigCoins()
		msg = Withdraw{Amount: amount}
	case OpWithdrawAll:
		msg = WithdrawAll{}
	case OpDeleteStatement:
		msg = DeleteStatement{}
	case OpTextComment:
		var text string
		text, err = s.LoadStringSnake()
		if err == nil && text != DonateComment {
			return nil, ErrUnknownMessage
		}
		msg = Donate{}
	default:
		return nil, ErrUnknownMessage
	}

	if err != nil {
		return nil, fmt.Errorf("parse message 0x%08x: %w", op, err)
	}
	return msg, nil
}

// loadData reads VftData { statement: string, url: string? }:
// a string is a ref to a snake cell, a nullable string is a maybe-ref.
func loadData(s *cell.Slice) (Data, error) {
	ref, err := s.LoadRef()
	if err != nil {
		return Data{}, fmt.Errorf("statement: %w", err)
	}

	statement, err := ref.LoadStringSnake()
	if err != nil {
		return Data{}, fmt.Errorf("statement: %w", err)
	}

	urlRef, err := s.LoadMaybeRef()
	if err != nil {
		return Data{}, fmt.Errorf("url: %w", err)
	}

	d := Data{Statement: statement}
	if urlRef != nil {
		url, err := urlRef.LoadStringSnake()
		if err != nil {
			return Data{}, fmt.Errorf("url: %w", err)
		}
		d.URL = &url
	}
	return d, nil
}
