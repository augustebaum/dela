package neff

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"github.com/stretchr/testify/require"
	evotingController "go.dedis.ch/dela/contracts/evoting/controller"
	electionTypes "go.dedis.ch/dela/contracts/evoting/types"
	"go.dedis.ch/dela/core/access"
	"go.dedis.ch/dela/core/ordering"
	"go.dedis.ch/dela/core/ordering/cosipbft/authority"
	"go.dedis.ch/dela/core/ordering/cosipbft/blockstore"
	orderingTypes "go.dedis.ch/dela/core/ordering/cosipbft/types"
	"go.dedis.ch/dela/core/store"
	"go.dedis.ch/dela/core/txn"
	"go.dedis.ch/dela/core/txn/pool"
	"go.dedis.ch/dela/core/validation"
	"go.dedis.ch/dela/crypto"
	"go.dedis.ch/dela/internal/testing/fake"
	"go.dedis.ch/dela/mino"
	"go.dedis.ch/dela/serde"
	"go.dedis.ch/dela/shuffle/neff/types"
	"go.dedis.ch/kyber/v3/util/random"
	"golang.org/x/xerrors"
	"io"
	"strconv"
	"testing"
)

func TestHandler_Stream(t *testing.T) {
	handler := Handler{}
	receiver := fake.NewBadReceiver()
	err := handler.Stream(fake.Sender{}, receiver)
	require.EqualError(t, err, fake.Err("failed to receive"))

	receiver = fake.NewReceiver(
		fake.NewRecvMsg(fake.NewAddress(0), fake.Message{}),
	)
	err = handler.Stream(fake.Sender{}, receiver)
	require.EqualError(t, err, "expected StartShuffle message, got: fake.Message")
}

func TestHandler_StartShuffle(t *testing.T) {

	k := 3

	RandomStream := suite.RandomStream()
	h := suite.Scalar().Pick(RandomStream)
	pubKey := suite.Point().Mul(h, nil)

	KsMarshalled := make([][]byte, 0, k)
	CsMarshalled := make([][]byte, 0, k)

	for i := 0; i < k; i++ {
		// Embed the message into a curve point
		message := "Ballot" + strconv.Itoa(i)
		M := suite.Point().Embed([]byte(message), random.New())

		// ElGamal-encrypt the point to produce ciphertext (K,C).
		k := suite.Scalar().Pick(random.New()) // ephemeral private key
		K := suite.Point().Mul(k, nil)         // ephemeral DH public key
		S := suite.Point().Mul(k, pubKey)      // ephemeral DH shared secret
		C := S.Add(S, M)                       // message blinded with secret

		Kmarshalled, _ := K.MarshalBinary()
		Cmarshalled, _ := C.MarshalBinary()

		KsMarshalled = append(KsMarshalled, Kmarshalled)
		CsMarshalled = append(CsMarshalled, Cmarshalled)
	}

	fakeErr := xerrors.Errorf("fake error")

	handler := Handler{
		me: fake.NewAddress(0),
	}

	badService := FakeService{
		err:        fakeErr,
		election:   "fakeElection",
		electionId: "dummyId",
	}
	handler.service = &badService

	badDummyId := "dummyId"
	startShuffle := types.NewStartShuffle(
		1,
		badDummyId,
		nil)

	from := fake.NewAddress(0)

	err := handler.HandleStartShuffleMessage(startShuffle, from, nil, nil)
	require.EqualError(t, err, "failed to decode election id: encoding/hex: invalid byte: U+0075 'u'")

	dummyId := hex.EncodeToString([]byte("dummyId"))
	startShuffle = types.NewStartShuffle(
		1,
		dummyId,
		nil)
	err = handler.HandleStartShuffleMessage(startShuffle, from, nil, nil)
	require.EqualError(t, err, "failed to read on the blockchain: "+"fake error")

	service := FakeService{
		err:        nil,
		election:   "fakeElection",
		electionId: electionTypes.ID(dummyId),
	}
	handler.service = &service

	err = handler.HandleStartShuffleMessage(startShuffle, from, nil, nil)
	require.EqualError(t, err, "failed to unmarshal Election: json: cannot unmarshal string into Go value of type types.Election")

	election := electionTypes.Election{
		Title:            "dummyTitle",
		ElectionID:       electionTypes.ID(dummyId),
		AdminId:          "dummyAdminId",
		Candidates:       nil,
		Status:           0,
		Pubkey:           nil,
		EncryptedBallots: map[string][]byte{},
		ShuffledBallots:  map[int][][]byte{},
		Proofs:           nil,
		DecryptedBallots: nil,
		ShuffleThreshold: 0,
	}

	service = FakeService{
		err:        nil,
		election:   election,
		electionId: electionTypes.ID(dummyId),
	}
	handler.service = &service

	err = handler.HandleStartShuffleMessage(startShuffle, from, nil, nil)
	require.EqualError(t, err, "the election must be closed")

	election.Status = electionTypes.Closed
	election.EncryptedBallots["fakeUser"] = []byte("fakeVote")

	service = FakeService{
		err:        nil,
		election:   election,
		electionId: electionTypes.ID(dummyId),
	}

	handler.service = &service
	err = handler.HandleStartShuffleMessage(startShuffle, from, nil, nil)
	require.EqualError(t, err, "failed to unmarshal Ciphertext: invalid character 'k' in literal false (expecting 'l')")

	delete(election.EncryptedBallots, "fakeUser")

	for i := 0; i < k; i++ {
		ballot := electionTypes.Ciphertext{
			K: []byte("fakeVoteK"),
			C: []byte("fakeVoteC"),
		}
		js, _ := json.Marshal(ballot)
		election.EncryptedBallots["dummyUser"+strconv.Itoa(i)] = js
	}

	service = FakeService{
		err:        nil,
		election:   election,
		electionId: electionTypes.ID(dummyId),
	}

	handler.service = &service
	err = handler.HandleStartShuffleMessage(startShuffle, from, nil, nil)
	require.EqualError(t, err, "failed to unmarshal K: invalid Ed25519 curve point")

	for i := 0; i < k; i++ {
		ballot := electionTypes.Ciphertext{
			K: KsMarshalled[i],
			C: []byte("fakeVoteC"),
		}
		js, _ := json.Marshal(ballot)
		election.EncryptedBallots["dummyUser"+strconv.Itoa(i)] = js
	}

	service = FakeService{
		err:        nil,
		election:   election,
		electionId: electionTypes.ID(dummyId),
	}

	handler.service = &service

	err = handler.HandleStartShuffleMessage(startShuffle, from, nil, nil)
	require.EqualError(t, err, "failed to unmarshal C: invalid Ed25519 curve point")

	for i := 0; i < k; i++ {
		ballot := electionTypes.Ciphertext{
			K: KsMarshalled[i],
			C: CsMarshalled[i],
		}
		js, _ := json.Marshal(ballot)
		election.EncryptedBallots["dummyUser"+strconv.Itoa(i)] = js
	}

	service = FakeService{
		err:        nil,
		election:   election,
		electionId: electionTypes.ID(dummyId),
	}

	handler.service = &service

	err = handler.HandleStartShuffleMessage(startShuffle, from, nil, nil)
	require.EqualError(t, err, "couldn't unmarshal public key: invalid Ed25519 curve point")

	pubKeyMarshalled, _ := pubKey.MarshalBinary()
	election.Pubkey = pubKeyMarshalled

	service = FakeService{
		err:        nil,
		election:   election,
		electionId: electionTypes.ID(dummyId),
	}

	handler.service = &service
	/*
	   todo : use this code to test getNonce from evotingController.Client

	   	badBlockstore := FakeBlockStore{
	   		lastErr: fakeErr,
	   	}

	   	handler.blocks = badBlockstore

	   	err = handler.HandleStartShuffleMessage(startShuffle, from, nil, nil)
	   	require.EqualError(t, err, "failed to get Client: failed to fetch
	   last block: fake error")

	   	badBlockstore = FakeBlockStore{
	   		getErr: fakeErr,
	   	}

	   	handler.blocks = badBlockstore

	   	err = handler.HandleStartShuffleMessage(startShuffle, from, nil, nil)
	   	require.EqualError(t, err, "failed to get Client: failed to fetch previous
	   block: fake error")

	   	blocks := FakeBlockStore{
	   		getErr: xerrors.Errorf("not found: no block"),
	   	}

	   	handler.blocks = blocks

	*/
	handler.signer = fake.NewBadSigner()
	handler.client = &evotingController.Client{
		Nonce: 0,
		Blocks: FakeBlockStore{
			getErr:  nil,
			lastErr: nil,
		},
	}
	err = handler.HandleStartShuffleMessage(startShuffle, from, nil, nil)
	require.EqualError(t, err, fake.Err("failed to make transaction: failed to sign: signer"))

	handler.signer = fake.NewSigner()
	badPool := FakePool{err: fakeErr}
	handler.p = &badPool

	service = FakeService{
		err:        nil,
		election:   election,
		electionId: electionTypes.ID(dummyId),
		pool:       &badPool,
	}
	handler.service = &service

	err = handler.HandleStartShuffleMessage(startShuffle, from, nil, nil)
	require.EqualError(t, err, "failed to add transaction to the pool: fake error")

	fakePool := FakePool{}
	service = FakeService{
		err:        nil,
		election:   election,
		electionId: electionTypes.ID(dummyId),
		pool:       &fakePool,
		status:     true,
	}

	handler.service = &service
	handler.p = &fakePool

	startShuffle = types.NewStartShuffle(
		1,
		dummyId,
		[]mino.Address{fake.NewAddress(0), fake.NewAddress(1)})

	badSender := fake.NewBadSender()

	err = handler.HandleStartShuffleMessage(startShuffle, from, badSender, nil)
	require.EqualError(t, err, fake.Err("failed to send EndShuffle message"))

	fakeSender := fake.Sender{}
	err = handler.HandleStartShuffleMessage(startShuffle, from, fakeSender, nil)
	require.NoError(t, err)

	startShuffle = types.NewStartShuffle(
		2,
		dummyId,
		[]mino.Address{fake.NewAddress(0), fake.NewAddress(1)})

	badReceiver := fake.NewBadReceiver()
	err = handler.HandleStartShuffleMessage(startShuffle, from, fakeSender, badReceiver)
	require.EqualError(t, err, fake.Err("got an error from '%!s(<nil>)' while receiving"))

	fakeReceiver := fake.NewReceiver(fake.NewRecvMsg(fake.NewAddress(0), nil))
	err = handler.HandleStartShuffleMessage(startShuffle, from, fakeSender, fakeReceiver)
	require.EqualError(t, err, "expected to receive an EndShuffle message, but go the following: <nil>")

	fakeReceiver = fake.NewReceiver(fake.NewRecvMsg(fake.NewAddress(0), types.NewEndShuffle()))
	err = handler.HandleStartShuffleMessage(startShuffle, from, fakeSender, fakeReceiver)
	require.NoError(t, err)

	service.status = false
	handler.service = &service
	startShuffle = types.NewStartShuffle(
		1,
		dummyId,
		[]mino.Address{fake.NewAddress(0), fake.NewAddress(1)})
	err = handler.HandleStartShuffleMessage(startShuffle, from, fakeSender, fakeReceiver)
	require.EqualError(t, err, "failed to shuffle, all your transactions got denied")

	startShuffle = types.NewStartShuffle(
		2,
		dummyId,
		[]mino.Address{fake.NewAddress(0), fake.NewAddress(1)})

	for _, value := range election.EncryptedBallots {
		election.ShuffledBallots[1] = append(election.ShuffledBallots[1], value)
	}

	fakePool = FakePool{}
	service = FakeService{
		err:        nil,
		election:   election,
		electionId: electionTypes.ID(dummyId),
		pool:       &fakePool,
		status:     false,
	}

	err = handler.HandleStartShuffleMessage(startShuffle, from, fakeSender, fakeReceiver)
	require.NoError(t, err)

}

// -----------------------------------------------------------------------------
// Utility functions

// FakeProof
// - implements ordering.Proof
type FakeProof struct {
	key   []byte
	value []byte
}

// GetKey implements ordering.Proof. It returns the key associated to the proof.
func (f FakeProof) GetKey() []byte {
	return f.key
}

// GetValue implements ordering.Proof. It returns the value associated to the
// proof if the key exists, otherwise it returns nil.
func (f FakeProof) GetValue() []byte {
	return f.value
}

//
// Fake Service
//

type FakeService struct {
	err        error
	election   interface{}
	electionId electionTypes.ID
	pool       *FakePool
	status     bool
}

func (f FakeService) GetProof(key []byte) (ordering.Proof, error) {
	electionIDBuff, _ := hex.DecodeString(string(f.electionId))

	if bytes.Equal(key, electionIDBuff) {
		js, _ := json.Marshal(f.election)
		proof := FakeProof{
			key:   key,
			value: js,
		}
		return proof, f.err
	}

	return nil, f.err
}

func (f FakeService) GetStore() store.Readable {
	return nil
}

func (f *FakeService) Watch(ctx context.Context) <-chan ordering.Event {

	results := make([]validation.TransactionResult, 3)

	electionIDBuffDummy1, _ := hex.DecodeString("dummyId1")
	results[0] = FakeTransactionResult{
		status:      true,
		message:     "",
		transaction: FakeTransaction{nonce: 10, id: electionIDBuffDummy1},
	}

	electionIDBuffDummy2, _ := hex.DecodeString("dummyId2")
	results[1] = FakeTransactionResult{
		status:      true,
		message:     "",
		transaction: FakeTransaction{nonce: 11, id: electionIDBuffDummy2},
	}

	results[2] = FakeTransactionResult{
		status:      f.status,
		message:     "",
		transaction: f.pool.transaction,
	}

	f.status = true

	channel := make(chan ordering.Event, 1)
	channel <- ordering.Event{
		Index:        0,
		Transactions: results,
	}
	close(channel)

	return channel

}

func (f FakeService) Close() error {
	return f.err
}

//
// Fake Pool
//

type FakePool struct {
	err         error
	transaction FakeTransaction
}

func (f FakePool) SetPlayers(players mino.Players) error {
	return nil
}

func (f FakePool) AddFilter(filter pool.Filter) {
}

func (f FakePool) Len() int {
	return 0
}

func (f *FakePool) Add(transaction txn.Transaction) error {
	newTx := FakeTransaction{
		nonce: transaction.GetNonce(),
		id:    transaction.GetID(),
	}

	f.transaction = newTx
	return f.err
}

func (f FakePool) Remove(transaction txn.Transaction) error {
	return nil
}

func (f FakePool) Gather(ctx context.Context, config pool.Config) []txn.Transaction {
	return nil
}

func (f FakePool) Close() error {
	return nil
}

//
// Fake Transaction
//

type FakeTransaction struct {
	nonce uint64
	id    []byte
}

func (f FakeTransaction) Serialize(ctx serde.Context) ([]byte, error) {
	return nil, nil
}

func (f FakeTransaction) Fingerprint(writer io.Writer) error {
	return nil
}

func (f FakeTransaction) GetID() []byte {
	return f.id
}

func (f FakeTransaction) GetNonce() uint64 {
	return f.nonce
}

func (f FakeTransaction) GetIdentity() access.Identity {
	return nil
}

func (f FakeTransaction) GetArg(key string) []byte {
	return nil
}

//
// Fake TransactionResult
//

type FakeTransactionResult struct {
	status      bool
	message     string
	transaction FakeTransaction
}

func (f FakeTransactionResult) Serialize(ctx serde.Context) ([]byte, error) {
	return nil, nil
}

func (f FakeTransactionResult) GetTransaction() txn.Transaction {
	return f.transaction
}

func (f FakeTransactionResult) GetStatus() (bool, string) {
	return f.status, f.message
}

//
// Fake Result
//

type FakeResult struct {
}

func (f FakeResult) Serialize(ctx serde.Context) ([]byte, error) {
	return nil, nil
}

func (f FakeResult) Fingerprint(writer io.Writer) error {
	return nil
}

func (f FakeResult) GetTransactionResults() []validation.TransactionResult {
	results := make([]validation.TransactionResult, 1)

	results[0] = FakeTransactionResult{
		status:      true,
		message:     "",
		transaction: FakeTransaction{nonce: 10},
	}

	return results
}

//
// Fake BlockLink
//

type FakeBlockLink struct {
}

func (f FakeBlockLink) Serialize(ctx serde.Context) ([]byte, error) {
	return nil, nil
}

func (f FakeBlockLink) Fingerprint(writer io.Writer) error {
	return nil
}

func (f FakeBlockLink) GetHash() orderingTypes.Digest {
	return orderingTypes.Digest{}
}

func (f FakeBlockLink) GetFrom() orderingTypes.Digest {
	return orderingTypes.Digest{}
}

func (f FakeBlockLink) GetTo() orderingTypes.Digest {
	return orderingTypes.Digest{}
}

func (f FakeBlockLink) GetPrepareSignature() crypto.Signature {
	return nil
}

func (f FakeBlockLink) GetCommitSignature() crypto.Signature {
	return nil
}

func (f FakeBlockLink) GetChangeSet() authority.ChangeSet {
	return nil
}

func (f FakeBlockLink) GetBlock() orderingTypes.Block {

	result := FakeResult{}

	block, _ := orderingTypes.NewBlock(result)
	return block
}

func (f FakeBlockLink) Reduce() orderingTypes.Link {
	return nil
}

//
// Fake BlockStore
//

type FakeBlockStore struct {
	getErr  error
	lastErr error
}

func (f FakeBlockStore) Len() uint64 {
	return 0
}

func (f FakeBlockStore) Store(link orderingTypes.BlockLink) error {
	return nil
}

func (f FakeBlockStore) Get(id orderingTypes.Digest) (orderingTypes.BlockLink, error) {
	return FakeBlockLink{}, f.getErr
}

func (f FakeBlockStore) GetByIndex(index uint64) (orderingTypes.BlockLink, error) {
	return nil, nil
}

func (f FakeBlockStore) GetChain() (orderingTypes.Chain, error) {
	return nil, nil
}

func (f FakeBlockStore) Last() (orderingTypes.BlockLink, error) {
	return FakeBlockLink{}, f.lastErr
}

func (f FakeBlockStore) Watch(ctx context.Context) <-chan orderingTypes.BlockLink {
	return nil
}

func (f FakeBlockStore) WithTx(transaction store.Transaction) blockstore.BlockStore {
	return nil
}
