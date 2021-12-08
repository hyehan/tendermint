package consensus

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tendermint/tendermint/abci/example/kvstore"
	"github.com/tendermint/tendermint/internal/eventbus"
	tmpubsub "github.com/tendermint/tendermint/internal/pubsub"
	"github.com/tendermint/tendermint/libs/log"
	tmtimemocks "github.com/tendermint/tendermint/libs/time/mocks"
	tmproto "github.com/tendermint/tendermint/proto/tendermint/types"
	"github.com/tendermint/tendermint/types"
)

const (
	// blockTimeIota is used in the test harness as the time between
	// blocks when not otherwise specified.
	blockTimeIota = time.Millisecond
)

// pbtsTestHarness constructs a Tendermint network that can be used for testing the
// implementation of the Proposer-Based timestamps algorithm.
// It runs a series of consensus heights and captures timing of votes and events.
type pbtsTestHarness struct {
	// configuration options set by the user of the test harness.
	pbtsTestConfiguration

	// The Tendermint consensus state machine being run during
	// a run of the pbtsTestHarness.
	observedState *State

	// A stub for signing votes and messages using the key
	// from the observedState.
	observedValidator *validatorStub

	// A list of simulated validators that interact with the observedState and are
	// fully controlled by the test harness.
	otherValidators []*validatorStub

	// The mock time source used by all of the validator stubs in the test harness.
	// This mock clock allows the test harness to produce votes and blocks with arbitrary
	// timestamps.
	validatorClock *tmtimemocks.Source

	chainID string

	// channels for verifying that the observed validator completes certain actions.
	ensureProposalCh, roundCh, blockCh, ensureVoteCh <-chan tmpubsub.Message

	// channel of events from the observed validator annotated with the timestamp
	// the event was received.
	eventCh <-chan timestampedEvent

	currentHeight int64
	currentRound  int32

	t   *testing.T
	ctx context.Context
}

type pbtsTestConfiguration struct {
	// The timestamp consensus parameters to be used by the state machine under test.
	timingParams types.TimingParams

	// The setting to use for the TimeoutPropose configuration parameter.
	timeoutPropose time.Duration

	// The timestamp of the first block produced by the network.
	genesisTime time.Time

	// The time at which the proposal at height 2 should be delivered.
	height2ProposalDeliverTime time.Time

	// The timestamp of the block proposed at height 2.
	height2ProposedBlockTime time.Time

	// The timestamp of the block proposed at height 4.
	// At height 4, the proposed block time and the deliver time are the same so
	// that timely-ness does not affect height 4.
	height4ProposedBlockTime time.Time
}

func newPBTSTestHarness(ctx context.Context, t *testing.T, tc pbtsTestConfiguration) pbtsTestHarness {
	t.Helper()
	const validators = 4
	cfg := configSetup(t)
	clock := new(tmtimemocks.Source)
	if tc.height4ProposedBlockTime.IsZero() {

		// Set a default height4ProposedBlockTime.
		// Use a proposed block time that is greater than the time that the
		// block at height 2 was delivered. Height 3 is not relevant for testing
		// and always occurs blockTimeIota before height 4. If not otherwise specified,
		// height 4 therefore occurs 2*blockTimeIota after height 2.
		tc.height4ProposedBlockTime = tc.height2ProposalDeliverTime.Add(2 * blockTimeIota)
	}
	cfg.Consensus.TimeoutPropose = tc.timeoutPropose
	consensusParams := types.DefaultConsensusParams()
	consensusParams.Timing = tc.timingParams

	state, privVals := makeGenesisState(t, cfg, genesisStateArgs{
		Params:     consensusParams,
		Time:       tc.genesisTime,
		Validators: validators,
	})
	cs := newState(ctx, t, log.NewTestingLogger(t), state, privVals[0], kvstore.NewApplication())
	vss := make([]*validatorStub, validators)
	for i := 0; i < validators; i++ {
		vss[i] = newValidatorStub(privVals[i], int32(i))
	}
	incrementHeight(vss[1:]...)

	for _, vs := range vss {
		vs.clock = clock
	}
	pubKey, err := vss[0].PrivValidator.GetPubKey(ctx)
	require.NoError(t, err)

	eventCh := timestampedCollector(ctx, t, cs.eventBus)

	return pbtsTestHarness{
		pbtsTestConfiguration: tc,
		observedValidator:     vss[0],
		observedState:         cs,
		otherValidators:       vss[1:],
		validatorClock:        clock,
		currentHeight:         1,
		chainID:               cfg.ChainID(),
		roundCh:               subscribe(ctx, t, cs.eventBus, types.EventQueryNewRound),
		ensureProposalCh:      subscribe(ctx, t, cs.eventBus, types.EventQueryCompleteProposal),
		blockCh:               subscribe(ctx, t, cs.eventBus, types.EventQueryNewBlock),
		ensureVoteCh:          subscribeToVoterBuffered(ctx, t, cs, pubKey.Address()),
		eventCh:               eventCh,
		t:                     t,
		ctx:                   ctx,
	}
}

func (p *pbtsTestHarness) observedValidatorProposerHeight(previousBlockTime time.Time) heightResult {
	p.validatorClock.On("Now").Return(p.height2ProposedBlockTime).Times(6)

	ensureNewRound(p.t, p.roundCh, p.currentHeight, p.currentRound)
	propBlock, partSet, err := p.observedState.createProposalBlock()
	require.NoError(p.t, err)
	bid := types.BlockID{Hash: propBlock.Hash(), PartSetHeader: partSet.Header()}

	timeout := time.Until(previousBlockTime.Add(ensureTimeout))
	ensureProposalWithTimeout(p.t, p.ensureProposalCh, p.currentHeight, p.currentRound, bid, timeout)
	ensurePrevote(p.t, p.ensureVoteCh, p.currentHeight, p.currentRound)
	signAddVotes(p.ctx, p.t, p.observedState, tmproto.PrevoteType, p.chainID, bid, p.otherValidators...)

	signAddVotes(p.ctx, p.t, p.observedState, tmproto.PrecommitType, p.chainID, bid, p.otherValidators...)
	ensurePrecommit(p.t, p.ensureVoteCh, p.currentHeight, p.currentRound)

	ensureNewBlock(p.t, p.blockCh, p.currentHeight)

	vk, err := p.observedValidator.GetPubKey(context.Background())
	require.NoError(p.t, err)
	res := collectHeightResults(p.ctx, p.t, p.eventCh, p.currentHeight, vk.Address())

	p.currentHeight++
	incrementHeight(p.otherValidators...)
	return res
}

func (p *pbtsTestHarness) height2() heightResult {
	signer := p.otherValidators[0].PrivValidator
	height3BlockTime := p.height2ProposedBlockTime.Add(-blockTimeIota)
	return p.nextHeight(signer, p.height2ProposalDeliverTime, p.height2ProposedBlockTime, height3BlockTime)
}

func (p *pbtsTestHarness) intermediateHeights() {
	signer := p.otherValidators[1].PrivValidator
	blockTimeHeight3 := p.height4ProposedBlockTime.Add(-blockTimeIota)
	p.nextHeight(signer, blockTimeHeight3, blockTimeHeight3, p.height4ProposedBlockTime)

	signer = p.otherValidators[2].PrivValidator
	p.nextHeight(signer, p.height4ProposedBlockTime, p.height4ProposedBlockTime, time.Now())
}

func (p *pbtsTestHarness) height5() heightResult {
	return p.observedValidatorProposerHeight(p.height4ProposedBlockTime)
}

func (p *pbtsTestHarness) nextHeight(proposer types.PrivValidator, deliverTime, proposedTime, nextProposedTime time.Time) heightResult {
	p.validatorClock.On("Now").Return(nextProposedTime).Times(6)

	ensureNewRound(p.t, p.roundCh, p.currentHeight, p.currentRound)

	b, _, err := p.observedState.createProposalBlock()
	require.NoError(p.t, err)
	b.Height = p.currentHeight
	b.Header.Height = p.currentHeight
	b.Header.Time = proposedTime

	k, err := proposer.GetPubKey(context.Background())
	require.NoError(p.t, err)
	b.Header.ProposerAddress = k.Address()
	ps, err := b.MakePartSet(types.BlockPartSizeBytes)
	require.NoError(p.t, err)
	bid := types.BlockID{Hash: b.Hash(), PartSetHeader: ps.Header()}
	prop := types.NewProposal(p.currentHeight, 0, -1, bid)
	tp := prop.ToProto()

	if err := proposer.SignProposal(context.Background(), p.observedState.state.ChainID, tp); err != nil {
		p.t.Fatalf("error signing proposal: %s", err)
	}

	time.Sleep(time.Until(deliverTime))
	prop.Signature = tp.Signature
	if err := p.observedState.SetProposalAndBlock(p.ctx, prop, b, ps, "peerID"); err != nil {
		p.t.Fatal(err)
	}
	ensureProposal(p.t, p.ensureProposalCh, p.currentHeight, 0, bid)

	ensurePrevote(p.t, p.ensureVoteCh, p.currentHeight, p.currentRound)
	signAddVotes(p.ctx, p.t, p.observedState, tmproto.PrevoteType, p.chainID, bid, p.otherValidators...)

	signAddVotes(p.ctx, p.t, p.observedState, tmproto.PrecommitType, p.chainID, bid, p.otherValidators...)
	ensurePrecommit(p.t, p.ensureVoteCh, p.currentHeight, p.currentRound)

	vk, err := p.observedValidator.GetPubKey(context.Background())
	require.NoError(p.t, err)
	res := collectHeightResults(p.ctx, p.t, p.eventCh, p.currentHeight, vk.Address())
	ensureNewBlock(p.t, p.blockCh, p.currentHeight)

	p.currentHeight++
	incrementHeight(p.otherValidators...)
	return res
}

func timestampedCollector(ctx context.Context, t *testing.T, eb *eventbus.EventBus) <-chan timestampedEvent {
	t.Helper()

	// Since eventCh is not read until the end of each height, it must be large
	// enough to hold all of the events produced during a single height.
	eventCh := make(chan timestampedEvent, 100)

	if err := eb.Observe(ctx, func(msg tmpubsub.Message) error {
		eventCh <- timestampedEvent{
			ts: time.Now(),
			m:  msg,
		}
		return nil
	}, types.EventQueryVote, types.EventQueryCompleteProposal); err != nil {
		t.Fatalf("Failed to observe query %v: %v", types.EventQueryVote, err)
	}
	return eventCh
}

func collectHeightResults(ctx context.Context, t *testing.T, eventCh <-chan timestampedEvent, height int64, address []byte) heightResult {
	t.Helper()
	var res heightResult
	for event := range eventCh {
		switch v := event.m.Data().(type) {
		case types.EventDataVote:
			if v.Vote.Height > height {
				t.Fatalf("received prevote from unexpected height, expected: %d, saw: %d", height, v.Vote.Height)
			}
			if !bytes.Equal(address, v.Vote.ValidatorAddress) {
				continue
			}
			if v.Vote.Type != tmproto.PrevoteType {
				continue
			}
			res.prevote = v.Vote
			res.prevoteIssuedAt = event.ts

		case types.EventDataCompleteProposal:
			if v.Height > height {
				t.Fatalf("received proposal from unexpected height, expected: %d, saw: %d", height, v.Height)
			}
			res.proposalIssuedAt = event.ts
		}
		if res.isComplete() {
			return res
		}
	}
	t.Fatalf("complete height result never seen for height %d", height)

	panic("unreachable")
}

type timestampedEvent struct {
	ts time.Time
	m  tmpubsub.Message
}

func (p *pbtsTestHarness) run() resultSet {
	startTestRound(p.ctx, p.observedState, p.currentHeight, p.currentRound)

	r1 := p.observedValidatorProposerHeight(p.genesisTime)
	r2 := p.height2()
	p.intermediateHeights()
	r5 := p.height5()
	err := p.observedState.Stop()
	require.NoError(p.t, err)
	return resultSet{
		genesisHeight: r1,
		height2:       r2,
		height5:       r5,
	}
}

type resultSet struct {
	genesisHeight heightResult
	height2       heightResult
	height5       heightResult
}

type heightResult struct {
	proposalIssuedAt time.Time
	prevote          *types.Vote
	prevoteIssuedAt  time.Time
}

func (hr heightResult) isComplete() bool {
	return !hr.proposalIssuedAt.IsZero() && !hr.prevoteIssuedAt.IsZero() && hr.prevote != nil
}

// TestReceiveProposalWaitsForPreviousBlockTime tests that a validator receiving
// a proposal waits until the previous block time passes before issuing a prevote.
// The test delivers the block to the validator after the configured `timeout-propose`,
// but before the proposer-based timestamp bound on block delivery and checks that
// the consensus algorithm correctly waits for the new block to be delivered
// and issues a prevote for it.
func TestReceiveProposalWaitsForPreviousBlockTime(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	initialTime := time.Now().Add(50 * time.Millisecond)
	cfg := pbtsTestConfiguration{
		timingParams: types.TimingParams{
			Precision:    100 * time.Millisecond,
			MessageDelay: 500 * time.Millisecond,
		},
		timeoutPropose:             50 * time.Millisecond,
		genesisTime:                initialTime,
		height2ProposalDeliverTime: initialTime.Add(450 * time.Millisecond),
		height2ProposedBlockTime:   initialTime.Add(350 * time.Millisecond),
	}

	pbtsTest := newPBTSTestHarness(ctx, t, cfg)
	results := pbtsTest.run()

	// Check that the validator waited until after the proposer-based timestamp
	// waitingTime bound.
	assert.True(t, results.height2.prevoteIssuedAt.After(cfg.height2ProposalDeliverTime))
	maxWaitingTime := cfg.genesisTime.Add(cfg.timingParams.Precision).Add(cfg.timingParams.MessageDelay)
	assert.True(t, results.height2.prevoteIssuedAt.Before(maxWaitingTime))

	// Check that the validator did not prevote for nil.
	assert.NotNil(t, results.height2.prevote.BlockID.Hash)
}

// TestReceiveProposalTimesOutOnSlowDelivery tests that a validator receiving
// a proposal times out and prevotes nil if the block is not delivered by the
// within the proposer-based timestamp algorithm's waitingTime bound.
// The test delivers the block to the validator after the previous block's time
// and after the proposer-based timestamp bound on block delivery.
// The test then checks that the validator correctly waited for the new block
// and prevoted nil after timing out.
func TestReceiveProposalTimesOutOnSlowDelivery(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	initialTime := time.Now()
	cfg := pbtsTestConfiguration{
		timingParams: types.TimingParams{
			Precision:    100 * time.Millisecond,
			MessageDelay: 500 * time.Millisecond,
		},
		timeoutPropose:             50 * time.Millisecond,
		genesisTime:                initialTime,
		height2ProposalDeliverTime: initialTime.Add(660 * time.Millisecond),
		height2ProposedBlockTime:   initialTime.Add(350 * time.Millisecond),
	}

	pbtsTest := newPBTSTestHarness(ctx, t, cfg)
	results := pbtsTest.run()

	// Check that the validator waited until after the proposer-based timestamp
	// waitinTime bound.
	maxWaitingTime := initialTime.Add(cfg.timingParams.Precision).Add(cfg.timingParams.MessageDelay)
	assert.True(t, results.height2.prevoteIssuedAt.After(maxWaitingTime))

	// Ensure that the validator issued a prevote for nil.
	assert.Nil(t, results.height2.prevote.BlockID.Hash)
}

// TestProposerWaitsForGenesisTime tests that a proposer will not propose a block
// until after the genesis time has passed. The test sets the genesis time in the
// future and then ensures that the observed validator waits to propose a block.
func TestProposerWaitsForGenesisTime(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// create a genesis time far (enough) in the future.
	initialTime := time.Now().Add(800 * time.Millisecond)
	cfg := pbtsTestConfiguration{
		timingParams: types.TimingParams{
			Precision:    10 * time.Millisecond,
			MessageDelay: 10 * time.Millisecond,
		},
		timeoutPropose:             10 * time.Millisecond,
		genesisTime:                initialTime,
		height2ProposalDeliverTime: initialTime.Add(10 * time.Millisecond),
		height2ProposedBlockTime:   initialTime.Add(10 * time.Millisecond),
	}

	pbtsTest := newPBTSTestHarness(ctx, t, cfg)
	results := pbtsTest.run()

	// ensure that the proposal was issued after the genesis time.
	assert.True(t, results.genesisHeight.proposalIssuedAt.After(cfg.genesisTime))
}

// TestProposerWaitsForPreviousBlock tests that the proposer of a block waits until
// the block time of the previous height has passed to propose the next block.
// The test harness ensures that the observed validator will be the proposer at
// height 1 and height 5. The test sets the block time of height 4 in the future
// and then verifies that the observed validator waits until after the block time
// of height 4 to propose a block at height 5.
func TestProposerWaitsForPreviousBlock(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	initialTime := time.Now().Add(time.Millisecond * 50)
	cfg := pbtsTestConfiguration{
		timingParams: types.TimingParams{
			Precision:    100 * time.Millisecond,
			MessageDelay: 500 * time.Millisecond,
		},
		timeoutPropose:             50 * time.Millisecond,
		genesisTime:                initialTime,
		height2ProposalDeliverTime: initialTime.Add(150 * time.Millisecond),
		height2ProposedBlockTime:   initialTime.Add(100 * time.Millisecond),
		height4ProposedBlockTime:   initialTime.Add(800 * time.Millisecond),
	}

	pbtsTest := newPBTSTestHarness(ctx, t, cfg)
	results := pbtsTest.run()

	// the observed validator is the proposer at height 5.
	// ensure that the observed validator did not propose a block until after
	// the time configured for height 4.
	assert.True(t, results.height5.proposalIssuedAt.After(cfg.height4ProposedBlockTime))

	// Ensure that the validator issued a prevote for a non-nil block.
	assert.NotNil(t, results.height5.prevote.BlockID.Hash)
}

func TestProposerWaitTime(t *testing.T) {
	genesisTime, err := time.Parse(time.RFC3339, "2019-03-13T23:00:00Z")
	require.NoError(t, err)
	testCases := []struct {
		name              string
		previousBlockTime time.Time
		localTime         time.Time
		expectedWait      time.Duration
	}{
		{
			name:              "block time greater than local time",
			previousBlockTime: genesisTime.Add(5 * time.Nanosecond),
			localTime:         genesisTime.Add(1 * time.Nanosecond),
			expectedWait:      4 * time.Nanosecond,
		},
		{
			name:              "local time greater than block time",
			previousBlockTime: genesisTime.Add(1 * time.Nanosecond),
			localTime:         genesisTime.Add(5 * time.Nanosecond),
			expectedWait:      0,
		},
		{
			name:              "both times equal",
			previousBlockTime: genesisTime.Add(5 * time.Nanosecond),
			localTime:         genesisTime.Add(5 * time.Nanosecond),
			expectedWait:      0,
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			mockSource := new(tmtimemocks.Source)
			mockSource.On("Now").Return(testCase.localTime)

			ti := proposerWaitTime(mockSource, testCase.previousBlockTime)
			assert.Equal(t, testCase.expectedWait, ti)
		})
	}
}

func TestProposalTimeout(t *testing.T) {
	genesisTime, err := time.Parse(time.RFC3339, "2019-03-13T23:00:00Z")
	require.NoError(t, err)
	testCases := []struct {
		name              string
		localTime         time.Time
		previousBlockTime time.Time
		precision         time.Duration
		msgDelay          time.Duration
		expectedDuration  time.Duration
	}{
		{
			name:              "MessageDelay + Precision has not quite elapsed",
			localTime:         genesisTime.Add(525 * time.Millisecond),
			previousBlockTime: genesisTime.Add(6 * time.Millisecond),
			precision:         time.Millisecond * 20,
			msgDelay:          time.Millisecond * 500,
			expectedDuration:  1 * time.Millisecond,
		},
		{
			name:              "MessageDelay + Precision equals current time",
			localTime:         genesisTime.Add(525 * time.Millisecond),
			previousBlockTime: genesisTime.Add(5 * time.Millisecond),
			precision:         time.Millisecond * 20,
			msgDelay:          time.Millisecond * 500,
			expectedDuration:  0,
		},
		{
			name:              "MessageDelay + Precision has elapsed",
			localTime:         genesisTime.Add(725 * time.Millisecond),
			previousBlockTime: genesisTime.Add(5 * time.Millisecond),
			precision:         time.Millisecond * 20,
			msgDelay:          time.Millisecond * 500,
			expectedDuration:  0,
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {

			mockSource := new(tmtimemocks.Source)
			mockSource.On("Now").Return(testCase.localTime)

			tp := types.TimingParams{
				Precision:    testCase.precision,
				MessageDelay: testCase.msgDelay,
			}

			ti := proposalStepWaitingTime(mockSource, testCase.previousBlockTime, tp)
			assert.Equal(t, testCase.expectedDuration, ti)
		})
	}
}
