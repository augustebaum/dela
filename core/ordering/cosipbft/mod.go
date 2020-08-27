package cosipbft

import (
	"context"
	"fmt"
	"time"

	"go.dedis.ch/dela"
	"go.dedis.ch/dela/consensus/viewchange"
	"go.dedis.ch/dela/consensus/viewchange/roster"
	"go.dedis.ch/dela/core/ordering"
	"go.dedis.ch/dela/core/ordering/cosipbft/blockstore"
	"go.dedis.ch/dela/core/ordering/cosipbft/blocksync"
	"go.dedis.ch/dela/core/ordering/cosipbft/pbft"
	"go.dedis.ch/dela/core/ordering/cosipbft/types"
	"go.dedis.ch/dela/core/store"
	"go.dedis.ch/dela/core/store/hashtree"
	"go.dedis.ch/dela/core/txn"
	"go.dedis.ch/dela/core/txn/pool"
	"go.dedis.ch/dela/core/validation"
	"go.dedis.ch/dela/cosi"
	"go.dedis.ch/dela/crypto"
	"go.dedis.ch/dela/mino"
	"golang.org/x/xerrors"
)

const (
	// RoundTimeout is the maximum of time the service waits for an event to
	// happen.
	RoundTimeout = 10 * time.Second

	rpcName = "cosipbft"
)

// Service is an ordering service using collective signatures combined with PBFT
// to create a chain of blocks.
type Service struct {
	*processor

	me    mino.Address
	rpc   mino.RPC
	actor cosi.Actor
	val   validation.Service

	timeout time.Duration
	events  chan ordering.Event
	closing chan struct{}
	closed  chan struct{}
}

type serviceTemplate struct {
	hashFac crypto.HashFactory
	blocks  blockstore.BlockStore
	genesis blockstore.GenesisStore
}

// ServiceOption is the type of option to set some fields of the service.
type ServiceOption func(*serviceTemplate)

// WithHashFactory is an option to set the hash factory used by the service.
func WithHashFactory(fac crypto.HashFactory) ServiceOption {
	return func(tmpl *serviceTemplate) {
		tmpl.hashFac = fac
	}
}

// ServiceParam is the different components to provide to the service. All the
// fields are mandatory and it will panic if any is nil.
type ServiceParam struct {
	Mino       mino.Mino
	Cosi       cosi.CollectiveSigning
	Validation validation.Service
	Pool       pool.Pool
	Tree       hashtree.Tree
}

// NewService starts a new service.
func NewService(param ServiceParam, opts ...ServiceOption) (*Service, error) {
	tmpl := serviceTemplate{
		hashFac: crypto.NewSha256Factory(),
		genesis: blockstore.NewGenesisStore(),
		blocks:  blockstore.NewInMemory(),
	}

	for _, opt := range opts {
		opt(&tmpl)
	}

	proc := newProcessor()
	proc.hashFactory = tmpl.hashFac
	proc.blocks = tmpl.blocks
	proc.genesis = tmpl.genesis
	proc.pool = param.Pool
	proc.rosterFac = roster.NewFactory(param.Mino.GetAddressFactory(), param.Cosi.GetPublicKeyFactory())
	proc.tree = blockstore.NewTreeCache(param.Tree)
	proc.logger = dela.Logger.With().Str("addr", param.Mino.GetAddress().String()).Logger()

	pcparam := pbft.StateMachineParam{
		Validation:      param.Validation,
		VerifierFactory: param.Cosi.GetVerifierFactory(),
		Blocks:          tmpl.blocks,
		Genesis:         tmpl.genesis,
		Tree:            proc.tree,
		AuthorityReader: proc.readRoster,
	}

	proc.pbftsm = pbft.NewStateMachine(pcparam)

	blockFac := types.NewBlockFactory(param.Validation.GetFactory())
	csFac := roster.NewChangeSetFactory(param.Mino.GetAddressFactory(), param.Cosi.GetPublicKeyFactory())
	linkFac := types.NewBlockLinkFactory(blockFac, param.Cosi.GetSignatureFactory(), csFac)

	syncparam := blocksync.SyncParam{
		Mino:        param.Mino,
		Blocks:      tmpl.blocks,
		PBFT:        proc.pbftsm,
		LinkFactory: linkFac,
	}

	blocksync, err := blocksync.NewSynchronizer(syncparam)
	if err != nil {
		return nil, xerrors.Errorf("creating sync failed: %v", err)
	}

	proc.sync = blocksync

	fac := types.NewMessageFactory(
		types.NewGenesisFactory(proc.rosterFac),
		blockFac,
		param.Cosi.GetSignatureFactory(),
		csFac,
	)

	proc.MessageFactory = fac

	rpc, err := param.Mino.MakeRPC(rpcName, proc, fac)
	if err != nil {
		return nil, xerrors.Errorf("creating rpc failed: %v", err)
	}

	actor, err := param.Cosi.Listen(proc)
	if err != nil {
		return nil, xerrors.Errorf("creating cosi failed: %v", err)
	}

	s := &Service{
		processor: proc,
		me:        param.Mino.GetAddress(),
		rpc:       rpc,
		actor:     actor,
		val:       param.Validation,
		timeout:   RoundTimeout,
		events:    make(chan ordering.Event, 1),
		closing:   make(chan struct{}),
		closed:    make(chan struct{}),
	}

	go func() {
		err := s.main()
		if err != nil {
			s.logger.Err(err).Msg("main loop failed")
		}

		close(s.closed)
	}()

	go s.watchBlocks()

	return s, nil
}

// Setup creates a genesis block and sends it to the collective authority.
func (s *Service) Setup(ctx context.Context, ca crypto.CollectiveAuthority) error {
	genesis, err := types.NewGenesis(ca, types.WithGenesisHashFactory(s.hashFactory))
	if err != nil {
		return xerrors.Errorf("creating genesis: %v", err)
	}

	resps, err := s.rpc.Call(ctx, types.NewGenesisMessage(genesis), ca)
	if err != nil {
		return xerrors.Errorf("sending genesis: %v", err)
	}

	for resp := range resps {
		_, err := resp.GetMessageOrError()
		if err != nil {
			return xerrors.Errorf("one request failed: %v", err)
		}
	}

	return nil
}

// GetProof implements ordering.Service.
func (s *Service) GetProof(key []byte) (ordering.Proof, error) {
	return nil, nil
}

// Watch implements ordering.Service.
func (s *Service) Watch(ctx context.Context) <-chan ordering.Event {
	ch := make(chan ordering.Event, 1)

	obs := observer{ch: ch}
	s.watcher.Add(obs)

	go func() {
		<-ctx.Done()
		s.watcher.Remove(obs)
	}()

	return ch
}

// Close gracefully closes the service. It will announce the closing request and
// wait for the current to end before returning.
func (s *Service) Close() error {
	close(s.closing)
	<-s.closed

	return nil
}

func (s *Service) watchBlocks() {
	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		<-s.closing
		cancel()
	}()

	linkCh := s.blocks.Watch(ctx)

	for link := range linkCh {
		// 1. Remove the transactions from the pool to avoid duplicates.
		for _, res := range link.GetTo().GetData().GetTransactionResults() {
			s.pool.Remove(res.GetTransaction())
		}

		// 2. Update the current membership.
		err := s.refreshRoster()
		if err != nil {
			s.logger.Err(err).Msg("roster refresh failed")
		}

		event := ordering.Event{
			Index: link.GetTo().GetIndex(),
		}

		// 3. Notify the main loop that a new block has been created, but ignore
		// if the channel is busy.
		select {
		case s.events <- event:
		default:
		}

		// 4. Notify the new block to potential listeners.
		s.watcher.Notify(event)
	}
}

func (s *Service) refreshRoster() error {
	roster, err := s.getCurrentRoster()
	if err != nil {
		return xerrors.Errorf("reading roster: %v", err)
	}

	err = s.pool.SetPlayers(roster)
	if err != nil {
		return xerrors.Errorf("updating tx pool: %v", err)
	}

	return nil
}

func (s *Service) main() error {
	select {
	case <-s.started:
		// A genesis block has been set, the node will then follow the chain
		// related to it.
	case <-s.closing:
		return nil
	}

	// Update the components that need to learn about the participants like the
	// transaction pool.
	err := s.refreshRoster()
	if err != nil {
		return xerrors.Errorf("refreshing roster: %v", err)
	}

	s.logger.Debug().Msg("node has started")

	for {
		ctx, cancel := context.WithCancel(context.Background())

		select {
		case <-s.closing:
			cancel()
			return nil
		default:
			err := s.doRound(ctx)
			cancel()

			if err != nil {
				return xerrors.Errorf("round failed: %v", err)
			}
		}
	}
}

func (s *Service) doRound(ctx context.Context) error {
	roster, err := s.getCurrentRoster()
	if err != nil {
		return xerrors.Errorf("reading roster: %v", err)
	}

	leader, err := s.pbftsm.GetLeader()
	if err != nil {
		return xerrors.Errorf("reading leader: %v", err)
	}

	for !s.me.Equal(leader) {
		// Only enters the loop if the node is not the leader. It has to wait
		// for the new block, or the round timeout, to proceed.

		select {
		case <-time.After(s.timeout):
			s.logger.Warn().Msg("round reached the timeout")

			ctx, cancel := context.WithTimeout(ctx, s.timeout)

			view, err := s.pbftsm.Expire(s.me)
			if err != nil {
				cancel()
				return xerrors.Errorf("pbft expire failed: %v", err)
			}

			resps, err := s.rpc.Call(ctx, types.NewViewMessage(view.ID, view.Leader), roster)
			if err != nil {
				cancel()
				return xerrors.Errorf("rpc failed: %v", err)
			}

			for resp := range resps {
				_, err = resp.GetMessageOrError()
				if err != nil {
					s.logger.Warn().Err(err).Msg("view propagation failure")
				}
			}

			statesCh := s.pbftsm.Watch(ctx)

			state := s.pbftsm.GetState()
			var more bool

			for state != pbft.InitialState {
				state, more = <-statesCh
				if !more {
					cancel()
					return xerrors.New("viewchange failed")
				}
			}

			s.logger.Debug().Msg("view change successful")

			cancel()
		case <-s.events:
			// A block has been created meaning that the round is over.
			return nil
		case <-s.closing:
			return nil
		}
	}

	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	s.logger.Debug().Uint64("index", s.blocks.Len()+1).Msg("round has started")

	// Send a synchronization to the roster so that they can learn about the
	// latest block of the chain.
	// TODO: get pbft threshold
	err = s.sync.Sync(ctx, roster, blocksync.Config{MinHard: roster.Len()})
	if err != nil {
		return xerrors.Errorf("sync failed: %v", err)
	}

	// TODO: check that no committed block exists in the case of a leader
	// failure when propagating the collective signature.

	s.logger.Debug().Uint64("index", s.blocks.Len()+1).Msg("pbft has started")

	err = s.doPBFT(ctx)
	if err != nil {
		return xerrors.Errorf("pbft failed: %v", err)
	}

	return nil
}

func (s *Service) doPBFT(ctx context.Context) error {
	txs := s.pool.Gather(ctx, pool.Config{Min: 1})

	if ctx.Err() != nil {
		// Don't bother trying PBFT if the context is done.
		return ctx.Err()
	}

	data, root, err := s.prepareData(txs)
	if err != nil {
		return xerrors.Errorf("failed to prepare data: %v", err)
	}

	block, err := types.NewBlock(
		data,
		types.WithTreeRoot(root),
		types.WithIndex(uint64(s.blocks.Len()+1)),
		types.WithHashFactory(s.hashFactory))

	if err != nil {
		return xerrors.Errorf("creating block failed: %v", err)
	}

	id, err := s.pbftsm.Prepare(block)
	if err != nil {
		return xerrors.Errorf("pbft prepare failed: %v", err)
	}

	roster, err := s.getCurrentRoster()
	if err != nil {
		return xerrors.Errorf("read roster failed: %v", err)
	}

	// 1. Prepare phase
	req := types.NewBlockMessage(block)

	sig, err := s.actor.Sign(ctx, req, roster)
	if err != nil {
		return xerrors.Errorf("prepare phase failed: %v", err)
	}

	s.logger.Debug().Str("signature", fmt.Sprintf("%v", sig)).Msg("prepare done")

	// 2. Commit phase
	commit := types.NewCommit(id, sig)

	sig, err = s.actor.Sign(ctx, commit, roster)
	if err != nil {
		return xerrors.Errorf("commit phase failed: %v", err)
	}

	s.logger.Debug().Str("signature", fmt.Sprintf("%v", sig)).Msg("commit done")

	// 3. Propagation phase
	done := types.NewDone(id, sig)

	resps, err := s.rpc.Call(ctx, done, roster)
	if err != nil {
		return xerrors.Errorf("rpc failed: %v", err)
	}

	for resp := range resps {
		_, err = resp.GetMessageOrError()
		if err != nil {
			s.logger.Warn().Err(err).Msg("propagation failed")
		}
	}

	// 4. Wake up new participants so that they can learn about the chain.
	err = s.wakeUp(ctx, roster)
	if err != nil {
		return xerrors.Errorf("wake up failed: %v", err)
	}

	return nil
}

func (s *Service) prepareData(txs []txn.Transaction) (data validation.Data, id types.Digest, err error) {
	var stageTree hashtree.StagingTree

	stageTree, err = s.tree.Get().Stage(func(snap store.Snapshot) error {
		data, err = s.val.Validate(snap, txs)
		if err != nil {
			return xerrors.Errorf("validation failed: %v", err)
		}

		return nil
	})

	if err != nil {
		err = xerrors.Errorf("staging tree failed: %v", err)
		return
	}

	copy(id[:], stageTree.GetRoot())

	return
}

func (s *Service) wakeUp(ctx context.Context, roster viewchange.Authority) error {
	newRoster, err := s.getCurrentRoster()
	if err != nil {
		return xerrors.Errorf("read roster failed: %v", err)
	}

	changeset := roster.Diff(newRoster)

	genesis, err := s.genesis.Get()
	if err != nil {
		return xerrors.Errorf("read genesis failed: %v", err)
	}

	resps, err := s.rpc.Call(ctx, types.NewGenesisMessage(genesis), mino.NewAddresses(changeset.GetNewAddresses()...))
	if err != nil {
		return xerrors.Errorf("rpc failed: %v", err)
	}

	for resp := range resps {
		_, err := resp.GetMessageOrError()
		if err != nil {
			s.logger.Warn().Err(err).Msg("wake up failed")
		}
	}

	return nil
}

type observer struct {
	ch chan ordering.Event
}

func (obs observer) NotifyCallback(event interface{}) {
	obs.ch <- event.(ordering.Event)
}