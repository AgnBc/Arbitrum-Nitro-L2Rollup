// Copyright 2021-2022, Offchain Labs, Inc.
// For license information, see https://github.com/nitro/blob/master/LICENSE

package arbtest

import (
	"context"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
	"github.com/offchainlabs/nitro/arbos/l2pricing"
	"github.com/offchainlabs/nitro/util"
	"math/big"
	"os"
	"testing"
	"time"
)

func TestSnapSync(t *testing.T) {
	ctx, cancelCtx := context.WithCancel(context.Background())
	defer cancelCtx()

	var transferGas = util.NormalizeL2GasForL1GasInitial(800_000, params.GWei) // include room for aggregator L1 costs

	// 1st node with sequencer, stays up all the time.
	builder := NewNodeBuilder(ctx).DefaultConfig(t, true)
	builder.L2Info = NewBlockChainTestInfo(
		t,
		types.NewArbitrumSigner(types.NewLondonSigner(builder.chainConfig.ChainID)), big.NewInt(l2pricing.InitialBaseFeeWei*2),
		transferGas,
	)
	cleanup := builder.Build(t)
	defer cleanup()

	// 2nd node without sequencer, syncs up to the first node.
	// This node will be stopped in middle and arbitrumdata will be deleted.
	nodeB, cleanupB := builder.Build2ndNode(t, &SecondNodeParams{})

	builder.BridgeBalance(t, "Faucet", big.NewInt(1).Mul(big.NewInt(params.Ether), big.NewInt(10000)))

	builder.L2Info.GenerateAccount("BackgroundUser")

	// Create transactions till batch count is 10
	for {
		tx := builder.L2Info.PrepareTx("Faucet", "BackgroundUser", builder.L2Info.TransferGas, big.NewInt(1), nil)
		err := builder.L2.Client.SendTransaction(ctx, tx)
		Require(t, err)
		_, err = builder.L2.EnsureTxSucceeded(tx)
		Require(t, err)
		count, err := builder.L2.ConsensusNode.InboxTracker.GetBatchCount()
		Require(t, err)
		if count > 10 {
			break
		}

	}
	// Wait for nodeB to sync up to the first node
	for {
		count, err := builder.L2.ConsensusNode.InboxTracker.GetBatchCount()
		Require(t, err)
		countNodeB, err := nodeB.ConsensusNode.InboxTracker.GetBatchCount()
		Require(t, err)
		if count == countNodeB {
			break
		} else {
			<-time.After(10 * time.Millisecond)
		}
	}

	batchCount, err := builder.L2.ConsensusNode.InboxTracker.GetBatchCount()
	Require(t, err)
	delayedCount, err := builder.L2.ConsensusNode.InboxTracker.GetDelayedCount()
	Require(t, err)
	// Last batch is batchCount - 1, so prev batch is batchCount - 2
	prevBatchMetaData, err := builder.L2.ConsensusNode.InboxTracker.GetBatchMetadata(batchCount - 2)
	Require(t, err)
	prevMessage, err := builder.L2.ConsensusNode.TxStreamer.GetMessage(prevBatchMetaData.MessageCount - 1)
	Require(t, err)
	// Create a config with snap sync enabled and same database directory as the 2nd node
	nodeConfig := builder.nodeConfig
	nodeConfig.SnapSync.Enabled = true
	nodeConfig.SnapSync.BatchCount = batchCount
	nodeConfig.SnapSync.DelayedCount = delayedCount
	nodeConfig.SnapSync.PrevDelayedRead = prevMessage.DelayedMessagesRead
	nodeConfig.SnapSync.PrevBatchMessageCount = uint64(prevBatchMetaData.MessageCount)
	// Cleanup the message data of 2nd node, but keep the block state data.
	// This is to simulate a snap sync environment where we’ve just gotten the block state but don’t have any messages.
	err = os.RemoveAll(nodeB.ConsensusNode.Stack.ResolvePath("arbitrumdata"))
	Require(t, err)

	// Cleanup the 2nd node to release the database lock
	cleanupB()
	// New node with snap sync enabled, and the same database directory as the 2nd node but with no message data.
	nodeC, cleanupC := builder.Build2ndNode(t, &SecondNodeParams{stackConfig: nodeB.ConsensusNode.Stack.Config(), nodeConfig: nodeConfig})
	defer cleanupC()

	// Create transactions till batch count is 20
	for {
		tx := builder.L2Info.PrepareTx("Faucet", "BackgroundUser", builder.L2Info.TransferGas, big.NewInt(1), nil)
		err := builder.L2.Client.SendTransaction(ctx, tx)
		Require(t, err)
		_, err = builder.L2.EnsureTxSucceeded(tx)
		Require(t, err)
		count, err := builder.L2.ConsensusNode.InboxTracker.GetBatchCount()
		Require(t, err)
		if count > 20 {
			break
		}
	}
	// Wait for nodeB to sync up to the first node
	for {
		count, err := builder.L2.ConsensusNode.InboxTracker.GetBatchCount()
		Require(t, err)
		countNodeC, err := nodeC.ConsensusNode.InboxTracker.GetBatchCount()
		Require(t, err)
		if count == countNodeC {
			// Once the node is synced up, check if the batch metadata is the same for the last batch
			// This is to ensure that the snap sync worked correctly
			metadata, err := builder.L2.ConsensusNode.InboxTracker.GetBatchMetadata(count - 1)
			Require(t, err)
			metadataNodeC, err := nodeC.ConsensusNode.InboxTracker.GetBatchMetadata(countNodeC - 1)
			Require(t, err)
			if metadata != metadataNodeC {
				t.Error("Batch metadata mismatch")
			}
			break
		} else {
			<-time.After(10 * time.Millisecond)
		}
	}

}
