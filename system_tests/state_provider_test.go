// Copyright 2023, Offchain Labs, Inc.
// For license information, see https://github.com/offchainlabs/bold/blob/main/LICENSE

// race detection makes things slow and miss timeouts
//go:build challengetest && !race

package arbtest

import (
	"context"
	"errors"
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/params"

	"github.com/offchainlabs/nitro/arbnode"
	"github.com/offchainlabs/nitro/arbos/l2pricing"
	"github.com/offchainlabs/nitro/staker"
	"github.com/offchainlabs/nitro/util"
	"github.com/offchainlabs/nitro/validator/valnode"

	protocol "github.com/OffchainLabs/bold/chain-abstraction"
	"github.com/OffchainLabs/bold/containers/option"
	l2stateprovider "github.com/OffchainLabs/bold/layer2-state-provider"
	"github.com/OffchainLabs/bold/solgen/go/bridgegen"
	prefixproofs "github.com/OffchainLabs/bold/state-commitments/prefix-proofs"
	mockmanager "github.com/OffchainLabs/bold/testing/mocks/state-provider"
)

func TestStateProvider_BOLD_Bisections(t *testing.T) {
	t.Parallel()
	ctx, cancelCtx := context.WithCancel(context.Background())
	defer cancelCtx()
	l2node, l1info, l2info, l1stack, l1client, stateManager := setupBoldStateProvider(t, ctx)
	defer requireClose(t, l1stack)
	defer l2node.StopAndWait()
	l2info.GenerateAccount("Destination")
	sequencerTxOpts := l1info.GetDefaultTransactOpts("Sequencer", ctx)

	seqInbox := l1info.GetAddress("SequencerInbox")
	seqInboxBinding, err := bridgegen.NewSequencerInbox(seqInbox, l1client)
	Require(t, err)

	// We will make two batches, with 5 messages in each batch.
	numMessagesPerBatch := int64(5)
	divergeAt := int64(-1) // No divergence.
	makeBoldBatch(t, l2node, l2info, l1client, &sequencerTxOpts, seqInboxBinding, seqInbox, numMessagesPerBatch, divergeAt)
	numMessagesPerBatch = int64(10)
	makeBoldBatch(t, l2node, l2info, l1client, &sequencerTxOpts, seqInboxBinding, seqInbox, numMessagesPerBatch, divergeAt)

	bridgeBinding, err := bridgegen.NewBridge(l1info.GetAddress("Bridge"), l1client)
	Require(t, err)
	totalBatchesBig, err := bridgeBinding.SequencerMessageCount(&bind.CallOpts{Context: ctx})
	Require(t, err)
	totalBatches := totalBatchesBig.Uint64()
	totalMessageCount, err := l2node.InboxTracker.GetBatchMessageCount(totalBatches - 1)
	Require(t, err)

	// Wait until the validator has validated the batches.
	for {
		if _, err := l2node.TxStreamer.ResultAtCount(totalMessageCount); err == nil {
			break
		}
	}

	historyCommitter := l2stateprovider.NewHistoryCommitmentProvider(
		stateManager,
		stateManager,
		stateManager, []l2stateprovider.Height{
			1 << 5,
			1 << 5,
			1 << 5,
		},
		stateManager,
	)
	bisectionHeight := l2stateprovider.Height(16)
	request := &l2stateprovider.HistoryCommitmentRequest{
		WasmModuleRoot:              common.Hash{},
		FromBatch:                   1,
		ToBatch:                     3,
		UpperChallengeOriginHeights: []l2stateprovider.Height{},
		FromHeight:                  0,
		UpToHeight:                  option.Some(bisectionHeight),
	}
	bisectionCommitment, err := historyCommitter.HistoryCommitment(ctx, request)
	Require(t, err)

	request.UpToHeight = option.None[l2stateprovider.Height]()
	packedProof, err := historyCommitter.PrefixProof(ctx, request, bisectionHeight)
	Require(t, err)

	data, err := mockmanager.ProofArgs.Unpack(packedProof)
	Require(t, err)
	preExpansion, ok := data[0].([][32]byte)
	if !ok {
		Fatal(t, "wrong type")
	}

	hashes := make([]common.Hash, len(preExpansion))
	for i, h := range preExpansion {
		hash := h
		hashes[i] = hash
	}

	computed, err := prefixproofs.Root(hashes)
	Require(t, err)
	if computed != bisectionCommitment.Merkle {
		Fatal(t, "wrong commitment")
	}
}

func TestStateProvider_BOLD(t *testing.T) {
	t.Parallel()
	ctx, cancelCtx := context.WithCancel(context.Background())
	defer cancelCtx()
	l2node, l1info, l2info, l1stack, l1client, stateManager := setupBoldStateProvider(t, ctx)
	defer requireClose(t, l1stack)
	defer l2node.StopAndWait()
	l2info.GenerateAccount("Destination")
	sequencerTxOpts := l1info.GetDefaultTransactOpts("Sequencer", ctx)

	seqInbox := l1info.GetAddress("SequencerInbox")
	seqInboxBinding, err := bridgegen.NewSequencerInbox(seqInbox, l1client)
	Require(t, err)

	// We will make two batches, with 5 messages in each batch.
	numMessagesPerBatch := int64(5)
	divergeAt := int64(-1) // No divergence.
	makeBoldBatch(t, l2node, l2info, l1client, &sequencerTxOpts, seqInboxBinding, seqInbox, numMessagesPerBatch, divergeAt)
	makeBoldBatch(t, l2node, l2info, l1client, &sequencerTxOpts, seqInboxBinding, seqInbox, numMessagesPerBatch, divergeAt)

	bridgeBinding, err := bridgegen.NewBridge(l1info.GetAddress("Bridge"), l1client)
	Require(t, err)
	totalBatchesBig, err := bridgeBinding.SequencerMessageCount(&bind.CallOpts{Context: ctx})
	Require(t, err)
	totalBatches := totalBatchesBig.Uint64()
	totalMessageCount, err := l2node.InboxTracker.GetBatchMessageCount(totalBatches - 1)
	Require(t, err)

	// Wait until the validator has validated the batches.
	for {
		if _, err := l2node.TxStreamer.ResultAtCount(totalMessageCount); err == nil {
			break
		}
	}

	t.Run("StatesInBatchRange", func(t *testing.T) {
		fromBatch := l2stateprovider.Batch(1)
		toBatch := l2stateprovider.Batch(3)
		fromHeight := l2stateprovider.Height(0)
		toHeight := l2stateprovider.Height(14)
		stateRoots, err := stateManager.StatesInBatchRange(fromHeight, toHeight, fromBatch, toBatch)
		Require(t, err)

		if stateRoots.Length() != 15 {
			Fatal(t, "wrong number of state roots")
		}
	})
	t.Run("AgreesWithExecutionState", func(t *testing.T) {
		// Non-zero position in batch shoould fail.
		err = stateManager.AgreesWithExecutionState(ctx, &protocol.ExecutionState{
			GlobalState: protocol.GoGlobalState{
				Batch:      0,
				PosInBatch: 1,
			},
			MachineStatus: protocol.MachineStatusFinished,
		})
		if err == nil {
			Fatal(t, "should not agree with execution state")
		}
		if !strings.Contains(err.Error(), "position in batch must be zero") {
			Fatal(t, "wrong error message")
		}

		// Always agrees with genesis.
		err = stateManager.AgreesWithExecutionState(ctx, &protocol.ExecutionState{
			GlobalState: protocol.GoGlobalState{
				Batch:      0,
				PosInBatch: 0,
			},
			MachineStatus: protocol.MachineStatusFinished,
		})
		Require(t, err)

		// Always agrees with the init message.
		err = stateManager.AgreesWithExecutionState(ctx, &protocol.ExecutionState{
			GlobalState: protocol.GoGlobalState{
				Batch:      1,
				PosInBatch: 0,
			},
			MachineStatus: protocol.MachineStatusFinished,
		})
		Require(t, err)

		// Chain catching up if it has not seen batch 10.
		err = stateManager.AgreesWithExecutionState(ctx, &protocol.ExecutionState{
			GlobalState: protocol.GoGlobalState{
				Batch:      10,
				PosInBatch: 0,
			},
			MachineStatus: protocol.MachineStatusFinished,
		})
		if err == nil {
			Fatal(t, "should not agree with execution state")
		}
		if !errors.Is(err, staker.ErrChainCatchingUp) {
			Fatal(t, "wrong error")
		}

		// Check if we agree with the last posted batch to the inbox.
		result, err := l2node.TxStreamer.ResultAtCount(totalMessageCount)
		Require(t, err)

		state := &protocol.ExecutionState{
			GlobalState: protocol.GoGlobalState{
				BlockHash:  result.BlockHash,
				SendRoot:   result.SendRoot,
				Batch:      3,
				PosInBatch: 0,
			},
			MachineStatus: protocol.MachineStatusFinished,
		}
		err = stateManager.AgreesWithExecutionState(ctx, state)
		Require(t, err)

		// See if we agree with one batch immediately after that and see that we fail with
		// "ErrChainCatchingUp".
		state.GlobalState.Batch += 1

		err = stateManager.AgreesWithExecutionState(ctx, state)
		if err == nil {
			Fatal(t, "should not agree with execution state")
		}
		if !errors.Is(err, staker.ErrChainCatchingUp) {
			Fatal(t, "wrong error")
		}
	})
	t.Run("ExecutionStateAfterBatchCount", func(t *testing.T) {
		_, err = stateManager.ExecutionStateAfterBatchCount(ctx, 0)
		if err == nil {
			Fatal(t, "should have failed")
		}
		if !strings.Contains(err.Error(), "batch count cannot be zero") {
			Fatal(t, "wrong error message")
		}

		execState, err := stateManager.ExecutionStateAfterBatchCount(ctx, totalBatches)
		Require(t, err)

		// We should agree with the last posted batch to the inbox based on our
		// retrieved execution state.
		err = stateManager.AgreesWithExecutionState(ctx, execState)
		Require(t, err)
	})
}

func setupBoldStateProvider(t *testing.T, ctx context.Context) (*arbnode.Node, *BlockchainTestInfo, *BlockchainTestInfo, *node.Node, *ethclient.Client, *staker.StateManager) {
	var transferGas = util.NormalizeL2GasForL1GasInitial(800_000, params.GWei) // include room for aggregator L1 costs
	l2chainConfig := params.ArbitrumDevTestChainConfig()
	l2info := NewBlockChainTestInfo(
		t,
		types.NewArbitrumSigner(types.NewLondonSigner(l2chainConfig.ChainID)), big.NewInt(l2pricing.InitialBaseFeeWei*2),
		transferGas,
	)
	ownerBal := big.NewInt(params.Ether)
	ownerBal.Mul(ownerBal, big.NewInt(1_000_000))
	l2info.GenerateGenesisAccount("Owner", ownerBal)

	_, l2node, _, _, l1info, _, l1client, l1stack, _, _ := createTestNodeOnL1ForBoldProtocol(t, ctx, true, nil, l2chainConfig, nil, l2info)

	valnode.TestValidationConfig.UseJit = false
	_, valStack := createTestValidationNode(t, ctx, &valnode.TestValidationConfig)
	blockValidatorConfig := staker.TestBlockValidatorConfig

	stateless, err := staker.NewStatelessBlockValidator(
		l2node.InboxReader,
		l2node.InboxTracker,
		l2node.TxStreamer,
		l2node.Execution,
		l2node.ArbDB,
		nil,
		nil,
		StaticFetcherFrom(t, &blockValidatorConfig),
		valStack,
	)
	Require(t, err)
	err = stateless.Start(ctx)
	Require(t, err)

	stateManager, err := staker.NewStateManager(
		stateless,
		"",
		[]l2stateprovider.Height{
			l2stateprovider.Height(blockChallengeLeafHeight),
			l2stateprovider.Height(bigStepChallengeLeafHeight),
			l2stateprovider.Height(smallStepChallengeLeafHeight),
		},
		"good",
	)
	Require(t, err)
	return l2node, l1info, l2info, l1stack, l1client, stateManager
}
