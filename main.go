package main

import (
	"context"
	"fmt"

	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/liteclient"
	"github.com/xssnick/tonutils-go/ton"
)

func main() {
	client := liteclient.NewConnectionPool()

	configUrl := "https://ton-blockchain.github.io/testnet-global.config.json"
	err := client.AddConnectionsFromConfigUrl(context.Background(), configUrl)
	if err != nil {
		panic(err)
	}

	api := ton.NewAPIClient(client).WithRetry()

	// we need fresh block info to run get methods
	block, err := api.CurrentMasterchainInfo(context.Background())
	if err != nil {
		panic(err)
	}

	// contract address
	addr := address.MustParseAddr("EQB161kb_MNinem9ILNO_YcEUdqR_p5GpSNJSczvkyFOxWht")

	res, err := api.RunGetMethod(context.Background(), block, addr, "statementsCount")
	if err != nil {
		// if contract exit code != 0 it will be treated as an error too
		panic(err)
	}

	fmt.Printf("Result: %v\n", res.MustInt(0))
}
