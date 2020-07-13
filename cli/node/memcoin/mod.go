// Package main implements a ledger based on in-memory components.
package main

import (
	"os"

	calypso "go.dedis.ch/dela/calypso/controller"
	"go.dedis.ch/dela/cli/node"
	pedersen "go.dedis.ch/dela/dkg/pedersen/controller"
	byzcoin "go.dedis.ch/dela/ledger/byzcoin/controller"
	mino "go.dedis.ch/dela/mino/minogrpc/controller"
)

func main() {
	builder := node.NewBuilder(mino.NewMinimal(), byzcoin.NewMinimal(),
		pedersen.NewMinimal(), calypso.NewMinimal())

	app := builder.Build()

	err := app.Run(os.Args)
	if err != nil {
		panic(err)
	}
}
