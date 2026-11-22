CONTRACT_DIR ?= ../vft-contract

.PHONY: build test contracts

build:
	go build -o vft-indexer ./cmd/vft-indexer

test:
	go test ./...

# Copy compiled contracts after `acton build` in vft-contract. The indexer must
# embed exactly the code deployed on-chain, otherwise derived addresses differ.
contracts:
	cp $(CONTRACT_DIR)/build/VftMaster.json $(CONTRACT_DIR)/build/VftStatement.json $(CONTRACT_DIR)/build/VftVote.json internal/vft/contracts/
