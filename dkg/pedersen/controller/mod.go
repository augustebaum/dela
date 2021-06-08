package controller

import (
	"go.dedis.ch/dela"
	"go.dedis.ch/dela/cli"
	"go.dedis.ch/dela/cli/node"
	"go.dedis.ch/dela/dkg/pedersen"
	"go.dedis.ch/dela/mino"
	"golang.org/x/xerrors"
)

// NewController returns a new controller initializer
func NewController() node.Initializer {
	return controller{}
}

// controller is an initializer with a set of commands.
//
// - implements node.Initializer
type controller struct{}

// Build implements node.Initializer.
func (m controller) SetCommands(builder node.Builder) {

	cmd := builder.SetCommand("dkg")
	cmd.SetDescription("interact with the DKG service")

	sub := cmd.SetSubCommand("init")
	sub.SetDescription("initialize the DKG protocol")
	sub.SetAction(builder.MakeAction(&initAction{}))

	// memcoin --config /tmp/node1 dkg setup --member $(memcoin --config
	// /tmp/node1 dkg export) --member $(memcoin --config /tmp/node2 dkg export)
	sub = cmd.SetSubCommand("setup")
	sub.SetDescription("creates the public distributed key and the private share on each node")
	sub.SetFlags(cli.StringSliceFlag{
		Name:     "member",
		Usage:    "nodes participating in DKG",
		Required: true,
	})
	sub.SetAction(builder.MakeAction(&setupAction{}))

	sub = cmd.SetSubCommand("export")
	sub.SetDescription("export the node address and public key")
	sub.SetAction(builder.MakeAction(&exportInfoAction{}))

	sub = cmd.SetSubCommand("getPublicKey")
	sub.SetDescription("prints the distributed public Key")
	sub.SetAction(builder.MakeAction(&getPublicKeyAction{}))

	// memcoin --config /tmp/node1 dkg encrypt --plaintext Hello --KfilePath K
	// --CfilePath C
	sub = cmd.SetSubCommand("encrypt")
	sub.SetDescription("encrypt the given string and write the ciphertext pair in the corresponding file")
	sub.SetFlags(cli.StringFlag{
		Name:     "plaintext",
		Usage:    "plaintext to encrypt",
		Required: true,
	}, cli.StringFlag{
		Name:     "KfilePath",
		Usage:    "path to write the K element of the ciphertext pair",
		Required: true,
	}, cli.StringFlag{
		Name:     "CfilePath",
		Usage:    "path to write the C element of the ciphertext pair",
		Required: true,
	})
	sub.SetAction(builder.MakeAction(&encryptAction{}))

	// memcoin --config /tmp/node2 dkg decrypt --KfilePath K --CfilePath C
	sub = cmd.SetSubCommand("decrypt")
	sub.SetDescription("decrypt the given ciphertext pair and print the corresponding plaintext")
	sub.SetFlags(cli.StringFlag{
		Name:     "KfilePath",
		Usage:    "path to retreive the K element of the ciphertext pair",
		Required: true,
	}, cli.StringFlag{
		Name:     "CfilePath",
		Usage:    "path to retreive the C element of the ciphertext pair",
		Required: true,
	})
	sub.SetAction(builder.MakeAction(&decryptAction{}))
}

// OnStart implements node.Initializer. It creates and registers a pedersen DKG.
func (m controller) OnStart(ctx cli.Flags, inj node.Injector) error {
	var no mino.Mino
	err := inj.Resolve(&no)
	if err != nil {
		return xerrors.Errorf("failed to resolve mino: %v", err)
	}

	dkg, pubkey := pedersen.NewPedersen(no)

	inj.Inject(dkg)
	inj.Inject(pubkey)

	pubkeyBuf, err := pubkey.MarshalBinary()
	if err != nil {
		return xerrors.Errorf("failed to encode pubkey: %v", err)
	}

	dela.Logger.Info().
		Hex("public key", pubkeyBuf).
		Msg("perdersen public key")

	return nil
}

// OnStop implements node.Initializer.
func (controller) OnStop(node.Injector) error {
	return nil
}
