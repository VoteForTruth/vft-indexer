// Package vft knows everything about the Vote For Truth contracts that the
// indexer needs: their code, how their addresses are derived, and how their
// messages are encoded.
package vft

import (
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/xssnick/tonutils-go/tvm/cell"
)

// contracts/*.json are copied from vft-contract/build after `acton build`
// (see `make contracts`). The indexer must use exactly the code deployed on-chain,
// otherwise derived addresses won't match.
//
//go:embed contracts/*.json
var contractsFS embed.FS

// Code holds compiled code of the contracts.
type Code struct {
	Master    *cell.Cell
	Statement *cell.Cell
	Vote      *cell.Cell
}

type compiledContract struct {
	CodeBoc64 string `json:"code_boc64"`
	Hash      string `json:"hash"`
}

// LoadCode loads the embedded compiled contracts and checks their hashes.
func LoadCode() (Code, error) {
	master, err := loadContract("VftMaster")
	if err != nil {
		return Code{}, err
	}

	statement, err := loadContract("VftStatement")
	if err != nil {
		return Code{}, err
	}

	vote, err := loadContract("VftVote")
	if err != nil {
		return Code{}, err
	}

	return Code{Master: master, Statement: statement, Vote: vote}, nil
}

func loadContract(name string) (*cell.Cell, error) {
	raw, err := contractsFS.ReadFile("contracts/" + name + ".json")
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", name, err)
	}

	var compiled compiledContract
	if err = json.Unmarshal(raw, &compiled); err != nil {
		return nil, fmt.Errorf("parse %s: %w", name, err)
	}

	boc, err := base64.StdEncoding.DecodeString(compiled.CodeBoc64)
	if err != nil {
		return nil, fmt.Errorf("decode %s boc: %w", name, err)
	}

	code, err := cell.FromBOC(boc)
	if err != nil {
		return nil, fmt.Errorf("parse %s boc: %w", name, err)
	}

	if !strings.EqualFold(hex.EncodeToString(code.Hash()), compiled.Hash) {
		return nil, fmt.Errorf("%s code hash mismatch: artifact says %s, boc is %x", name, compiled.Hash, code.Hash())
	}

	return code, nil
}
