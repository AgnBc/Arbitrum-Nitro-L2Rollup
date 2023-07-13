package gethexec

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params"
	"github.com/offchainlabs/nitro/arbos"
	"github.com/offchainlabs/nitro/arbos/arbosState"
	"github.com/offchainlabs/nitro/arbos/arbostypes"
	"github.com/offchainlabs/nitro/arbos/l1pricing"
	"github.com/offchainlabs/nitro/arbutil"
	"github.com/offchainlabs/nitro/consensus"
	"github.com/offchainlabs/nitro/execution"
	"github.com/offchainlabs/nitro/util/containers"
	"github.com/offchainlabs/nitro/util/sharedmetrics"
	"github.com/offchainlabs/nitro/util/stopwaiter"
)

var messageHashHeader = []byte("messageHash_")

type ExecutionEngine struct {
	stopwaiter.StopWaiter

	bc        *core.BlockChain
	db        ethdb.Database
	consensus consensus.FullConsensusClient
	recorder  *BlockRecorder

	resequenceChan chan []*arbostypes.MessageWithMetadata

	createBlocksMutex sync.Mutex

	newBlockNotifier chan struct{}
	latestBlockMutex sync.Mutex
	latestBlock      *types.Block

	nextScheduledVersionCheck time.Time // protected by the createBlocksMutex

	reorgSequencing bool
}

func NewExecutionEngine(bc *core.BlockChain, db ethdb.Database, consensus consensus.FullConsensusClient) (*ExecutionEngine, error) {
	return &ExecutionEngine{
		bc:               bc,
		db:               db,
		consensus:        consensus,
		resequenceChan:   make(chan []*arbostypes.MessageWithMetadata),
		newBlockNotifier: make(chan struct{}, 1),
	}, nil
}

func (s *ExecutionEngine) SetRecorder(recorder *BlockRecorder) {
	if s.Started() {
		panic("trying to set recorder after start")
	}
	if s.recorder != nil {
		panic("trying to set recorder policy when already set")
	}
	s.recorder = recorder
}

func (s *ExecutionEngine) EnableReorgSequencing() {
	if s.Started() {
		panic("trying to enable reorg sequencing after start")
	}
	if s.reorgSequencing {
		panic("trying to enable reorg sequencing when already set")
	}
	s.reorgSequencing = true
}

func (s *ExecutionEngine) SetTransactionStreamer(consensus consensus.FullConsensusClient) error {
	if s.Started() {
		return errors.New("trying to set transaction consensus after start")
	}
	if s.consensus != nil {
		return errors.New("trying to set transaction consensus when already set")
	}
	s.consensus = consensus
	return nil
}

func (s *ExecutionEngine) GetBatchFetcher() consensus.BatchFetcher {
	return s.consensus
}

func (s *ExecutionEngine) Reorg(count arbutil.MessageIndex, newMessages []arbostypes.MessageWithMetadata, oldMessages []*arbostypes.MessageWithMetadata) containers.PromiseInterface[struct{}] {
	promise := containers.NewPromise[struct{}](nil)
	promise.ProduceError(s.reorg(count, newMessages, oldMessages))
	return &promise
}

func (s *ExecutionEngine) reorg(count arbutil.MessageIndex, newMessages []arbostypes.MessageWithMetadata, oldMessages []*arbostypes.MessageWithMetadata) error {
	if count == 0 {
		return errors.New("cannot reorg out genesis")
	}
	s.createBlocksMutex.Lock()
	resequencing := false
	defer func() {
		// if we are resequencing old messages - don't release the lock
		// lock will be relesed by thread listening to resequenceChan
		if !resequencing {
			s.createBlocksMutex.Unlock()
		}
	}()
	blockNum := s.MessageIndexToBlockNumber(count - 1)
	// We can safely cast blockNum to a uint64 as it comes from MessageCountToBlockNumber
	targetBlock := s.bc.GetBlockByNumber(uint64(blockNum))
	if targetBlock == nil {
		log.Warn("reorg target block not found", "block", blockNum)
		return nil
	}

	err := s.deleteMessageHashStartingAt(count)
	if err != nil {
		// shouldn't happen - but if it does we'll naturally overwrite these with time.
		log.Warn("deleting messages on reorg", "err", err)
	}

	err = s.bc.ReorgToOldBlock(targetBlock)
	if err != nil {
		return err
	}
	for i := range newMessages {
		pos := count + arbutil.MessageIndex(i)
		sharedmetrics.UpdateSequenceNumberInBlockGauge(pos)
		err := s.digestMessageWithBlockMutex(&newMessages[i], pos)
		if err != nil {
			return err
		}
	}
	if s.recorder != nil {
		s.recorder.ReorgTo(targetBlock.Header())
	}
	if len(oldMessages) > 0 {
		s.resequenceChan <- oldMessages
		resequencing = true
	}
	return nil
}

func (s *ExecutionEngine) getCurrentHeader() (*types.Header, error) {
	currentBlock := s.bc.CurrentBlock()
	if currentBlock == nil {
		return nil, errors.New("failed to get current block")
	}
	return currentBlock, nil
}

func (s *ExecutionEngine) headMessageNumber() (arbutil.MessageIndex, error) {
	currentHeader, err := s.getCurrentHeader()
	if err != nil {
		return 0, err
	}
	return s.BlockNumberToMessageIndex(currentHeader.Number.Uint64())
}

func (s *ExecutionEngine) HeadMessageNumber() containers.PromiseInterface[arbutil.MessageIndex] {
	return containers.NewReadyPromise[arbutil.MessageIndex](s.headMessageNumber())
}

func (s *ExecutionEngine) HeadMessageNumberSync(t *testing.T) containers.PromiseInterface[arbutil.MessageIndex] {
	s.createBlocksMutex.Lock()
	defer s.createBlocksMutex.Unlock()
	return s.HeadMessageNumber()
}

func (s *ExecutionEngine) NextDelayedMessageNumber() containers.PromiseInterface[uint64] {
	currentHeader, err := s.getCurrentHeader()
	return containers.NewReadyPromise[uint64](currentHeader.Nonce.Uint64(), err)
}

func messageFromTxes(header *arbostypes.L1IncomingMessageHeader, txes types.Transactions, txErrors []error) (*arbostypes.L1IncomingMessage, error) {
	var l2Message []byte
	if len(txes) == 1 && txErrors[0] == nil {
		txBytes, err := txes[0].MarshalBinary()
		if err != nil {
			return nil, err
		}
		l2Message = append(l2Message, arbos.L2MessageKind_SignedTx)
		l2Message = append(l2Message, txBytes...)
	} else {
		l2Message = append(l2Message, arbos.L2MessageKind_Batch)
		sizeBuf := make([]byte, 8)
		for i, tx := range txes {
			if txErrors[i] != nil {
				continue
			}
			txBytes, err := tx.MarshalBinary()
			if err != nil {
				return nil, err
			}
			binary.BigEndian.PutUint64(sizeBuf, uint64(len(txBytes)+1))
			l2Message = append(l2Message, sizeBuf...)
			l2Message = append(l2Message, arbos.L2MessageKind_SignedTx)
			l2Message = append(l2Message, txBytes...)
		}
	}
	return &arbostypes.L1IncomingMessage{
		Header: header,
		L2msg:  l2Message,
	}, nil
}

// The caller must hold the createBlocksMutex
func (s *ExecutionEngine) resequenceReorgedMessages(ctx context.Context, messages []*arbostypes.MessageWithMetadata) {
	if !s.reorgSequencing {
		return
	}

	log.Info("Trying to resequence messages", "number", len(messages))
	lastBlockHeader, err := s.getCurrentHeader()
	if err != nil {
		log.Error("block header not found during resequence", "err", err)
		return
	}

	nextDelayedSeqNum := lastBlockHeader.Nonce.Uint64()

	for _, msg := range messages {
		// Check if the message is non-nil just to be safe
		if msg == nil || msg.Message == nil || msg.Message.Header == nil {
			continue
		}
		header := msg.Message.Header
		if header.RequestId != nil {
			delayedSeqNum := header.RequestId.Big().Uint64()
			if delayedSeqNum != nextDelayedSeqNum {
				log.Info("not resequencing delayed message due to unexpected index", "expected", nextDelayedSeqNum, "found", delayedSeqNum)
				continue
			}
			_, err := s.sequencerWrapper(ctx, func(ctx context.Context) (*types.Block, error) {
				return s.sequenceDelayedMessageWithBlockMutex(ctx, msg.Message, delayedSeqNum)
			})
			if err != nil {
				log.Error("failed to re-sequence old delayed message removed by reorg", "err", err)
			}
			nextDelayedSeqNum += 1
			continue
		}
		if header.Kind != arbostypes.L1MessageType_L2Message || header.Poster != l1pricing.BatchPosterAddress {
			// This shouldn't exist?
			log.Warn("skipping non-standard sequencer message found from reorg", "header", header)
			continue
		}
		// We don't need a batch fetcher as this is an L2 message
		txes, err := arbos.ParseL2Transactions(msg.Message, s.bc.Config().ChainID)
		if err != nil {
			log.Warn("failed to parse sequencer message found from reorg", "err", err)
			continue
		}
		hooks := arbos.NoopSequencingHooks()
		hooks.DiscardInvalidTxsEarly = true
		_, err = s.sequenceTransactionsWithBlockMutex(msg.Message.Header, txes, hooks)
		if err != nil {
			log.Error("failed to re-sequence old user message removed by reorg", "err", err)
			return
		}
	}
}

func (s *ExecutionEngine) sequencerWrapper(ctx context.Context, sequencerFunc func(ctx context.Context) (*types.Block, error)) (*types.Block, error) {
	attempts := 0
	for {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		block, err := sequencerFunc(ctx)
		if !errors.Is(err, consensus.ErrSequencerInsertLockTaken) {
			return block, err
		}
		// We got SequencerInsertLockTaken
		// option 1: there was a race, we are no longer main sequencer
		_, chosenErr := s.consensus.ExpectChosenSequencer().Await(ctx)
		if chosenErr != nil {
			return nil, chosenErr
		}
		// option 2: we are in a test without very orderly sequencer coordination
		if !s.bc.Config().ArbitrumChainParams.AllowDebugPrecompiles {
			// option 3: something weird. send warning
			log.Warn("sequence transactions: insert lock takent", "attempts", attempts)
		}
		// options 2/3 fail after too many attempts
		attempts++
		if attempts > 20 {
			return nil, err
		}
		<-time.After(time.Millisecond * 100)
	}
}

func (s *ExecutionEngine) SequenceTransactions(ctx context.Context, header *arbostypes.L1IncomingMessageHeader, txes types.Transactions, hooks *arbos.SequencingHooks) (*types.Block, error) {
	return s.sequencerWrapper(ctx, func(ctx context.Context) (*types.Block, error) {
		s.createBlocksMutex.Lock()
		defer s.createBlocksMutex.Unlock()
		hooks.TxErrors = nil
		return s.sequenceTransactionsWithBlockMutex(header, txes, hooks)
	})
}

func (s *ExecutionEngine) sequenceTransactionsWithBlockMutex(header *arbostypes.L1IncomingMessageHeader, txes types.Transactions, hooks *arbos.SequencingHooks) (*types.Block, error) {
	lastBlockHeader, err := s.getCurrentHeader()
	if err != nil {
		return nil, err
	}

	statedb, err := s.bc.StateAt(lastBlockHeader.Root)
	if err != nil {
		return nil, err
	}

	delayedMessagesRead := lastBlockHeader.Nonce.Uint64()

	startTime := time.Now()
	block, receipts, err := arbos.ProduceBlockAdvanced(
		header,
		txes,
		delayedMessagesRead,
		lastBlockHeader,
		statedb,
		s.bc,
		s.bc.Config(),
		hooks,
	)
	if err != nil {
		return nil, err
	}
	blockCalcTime := time.Since(startTime)
	if len(hooks.TxErrors) != len(txes) {
		return nil, fmt.Errorf("unexpected number of error results: %v vs number of txes %v", len(hooks.TxErrors), len(txes))
	}

	if len(receipts) == 0 {
		return nil, nil
	}

	allTxsErrored := true
	for _, err := range hooks.TxErrors {
		if err == nil {
			allTxsErrored = false
			break
		}
	}
	if allTxsErrored {
		return nil, nil
	}

	msg, err := messageFromTxes(header, txes, hooks.TxErrors)
	if err != nil {
		return nil, err
	}

	msgWithMeta := arbostypes.MessageWithMetadata{
		Message:             msg,
		DelayedMessagesRead: delayedMessagesRead,
	}

	pos, err := s.BlockNumberToMessageIndex(lastBlockHeader.Number.Uint64() + 1)
	if err != nil {
		return nil, err
	}

	result, err := s.resultFromHeader(block.Header())
	if err != nil {
		return nil, err
	}
	_, err = s.consensus.WriteMessageFromSequencer(pos, msgWithMeta, *result).Await(s.GetContext())
	if err != nil {
		return nil, err
	}

	// Only write the block after we've written the messages, so if the node dies in the middle of this,
	// it will naturally recover on startup by regenerating the missing block.
	err = s.appendBlockAndMessage(block, statedb, receipts, blockCalcTime, pos, &msgWithMeta)
	if err != nil {
		return nil, err
	}

	return block, nil
}

func (s *ExecutionEngine) SequenceDelayedMessage(message *arbostypes.L1IncomingMessage, delayedSeqNum uint64) containers.PromiseInterface[struct{}] {
	return stopwaiter.LaunchPromiseThread[struct{}](&s.StopWaiterSafe, func(ctx context.Context) (struct{}, error) {
		_, err := s.sequencerWrapper(ctx, func(ctx context.Context) (*types.Block, error) {
			s.createBlocksMutex.Lock()
			defer s.createBlocksMutex.Unlock()
			return s.sequenceDelayedMessageWithBlockMutex(ctx, message, delayedSeqNum)
		})
		return struct{}{}, err
	})
}

func (s *ExecutionEngine) sequenceDelayedMessageWithBlockMutex(ctx context.Context, message *arbostypes.L1IncomingMessage, delayedSeqNum uint64) (*types.Block, error) {
	currentHeader, err := s.getCurrentHeader()
	if err != nil {
		return nil, err
	}

	expectedDelayed := currentHeader.Nonce.Uint64()

	lastMsg, err := s.BlockNumberToMessageIndex(currentHeader.Number.Uint64())
	if err != nil {
		return nil, err
	}

	if expectedDelayed != delayedSeqNum {
		return nil, fmt.Errorf("wrong delayed message sequenced got %d expected %d", delayedSeqNum, expectedDelayed)
	}

	messageWithMeta := arbostypes.MessageWithMetadata{
		Message:             message,
		DelayedMessagesRead: delayedSeqNum + 1,
	}

	startTime := time.Now()
	block, statedb, receipts, err := s.createBlockFromNextMessage(&messageWithMeta)
	if err != nil {
		return nil, err
	}

	result, err := s.resultFromHeader(block.Header())
	if err != nil {
		return nil, err
	}

	_, err = s.consensus.WriteMessageFromSequencer(lastMsg+1, messageWithMeta, *result).Await(s.GetContext())
	if err != nil {
		return nil, err
	}

	err = s.appendBlockAndMessage(block, statedb, receipts, time.Since(startTime), lastMsg+1, &messageWithMeta)
	if err != nil {
		return nil, err
	}

	log.Info("ExecutionEngine: Added DelayedMessages", "pos", lastMsg+1, "delayed", delayedSeqNum, "block-header", block.Header())

	return block, nil
}

func (s *ExecutionEngine) GetGenesisBlockNumber() uint64 {
	return s.bc.Config().ArbitrumChainParams.GenesisBlockNum
}

func (s *ExecutionEngine) BlockNumberToMessageIndex(blockNum uint64) (arbutil.MessageIndex, error) {
	genesis := s.GetGenesisBlockNumber()
	if blockNum < genesis {
		return 0, fmt.Errorf("blockNum %d < genesis %d", blockNum, genesis)
	}
	return arbutil.MessageIndex(blockNum - genesis), nil
}

func (s *ExecutionEngine) MessageIndexToBlockNumber(messageNum arbutil.MessageIndex) uint64 {
	return uint64(messageNum) + s.GetGenesisBlockNumber()
}

// must hold createBlockMutex
func (s *ExecutionEngine) createBlockFromNextMessage(msg *arbostypes.MessageWithMetadata) (*types.Block, *state.StateDB, types.Receipts, error) {
	currentHeader := s.bc.CurrentBlock()
	if currentHeader == nil {
		return nil, nil, nil, errors.New("failed to get current block header")
	}

	currentBlock := s.bc.GetBlock(currentHeader.Hash(), currentHeader.Number.Uint64())
	if currentBlock == nil {
		return nil, nil, nil, errors.New("can't find block for current header")
	}

	err := s.bc.RecoverState(currentBlock)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to recover block %v state: %w", currentBlock.Number(), err)
	}

	statedb, err := s.bc.StateAt(currentHeader.Root)
	if err != nil {
		return nil, nil, nil, err
	}
	statedb.StartPrefetcher("TransactionStreamer")
	defer statedb.StopPrefetcher()

	block, receipts, err := arbos.ProduceBlock(
		msg.Message,
		msg.DelayedMessagesRead,
		currentHeader,
		statedb,
		s.bc,
		s.bc.Config(),
	)

	return block, statedb, receipts, err
}

func messageHashKey(pos arbutil.MessageIndex) []byte {
	key := make([]byte, 8)
	binary.BigEndian.PutUint64(key, uint64(pos))
	return append(messageHashHeader, key...)
}

func (s *ExecutionEngine) writeMessageHash(pos arbutil.MessageIndex, msgHash common.Hash) error {
	key := messageHashKey(pos)
	return s.db.Put(key, msgHash.Bytes())
}

func (s *ExecutionEngine) deleteMessageHashStartingAt(pos arbutil.MessageIndex) error {
	minKey := make([]byte, 8)
	binary.BigEndian.PutUint64(minKey, uint64(pos))
	batch := s.db.NewBatch()
	iter := s.db.NewIterator(messageHashHeader, minKey)
	var err error
	defer iter.Release()
	for iter.Next() {
		err = batch.Delete(iter.Key())
		if err != nil {
			log.Warn("deleting message hash: got error while iterating", "err", err)
			break
		}
	}
	if err == nil {
		err = iter.Error()
		if err != nil {
			log.Warn("deleting message hash: got error after iterating", "err", err)
		}
	}
	return batch.Write()
}

// must hold createBlockMutex
func (s *ExecutionEngine) appendBlockAndMessage(block *types.Block, statedb *state.StateDB, receipts types.Receipts, duration time.Duration, pos arbutil.MessageIndex, msg *arbostypes.MessageWithMetadata) error {
	var logs []*types.Log

	msgHash, err := msg.Hash(pos, s.bc.Config().ChainID.Uint64())
	if err != nil {
		return err
	}
	err = s.writeMessageHash(pos, msgHash)
	if err != nil {
		return err
	}

	for _, receipt := range receipts {
		logs = append(logs, receipt.Logs...)
	}
	status, err := s.bc.WriteBlockAndSetHeadWithTime(block, receipts, logs, statedb, true, duration)
	if err != nil {
		return err
	}
	if status == core.SideStatTy {
		return errors.New("geth rejected block as non-canonical")
	}
	return nil
}

func (s *ExecutionEngine) resultFromHeader(header *types.Header) (*execution.MessageResult, error) {
	if header == nil {
		return nil, fmt.Errorf("result not found")
	}
	info := types.DeserializeHeaderExtraInformation(header)
	return &execution.MessageResult{
		BlockHash: header.Hash(),
		SendRoot:  info.SendRoot,
	}, nil
}

func (s *ExecutionEngine) ResultAtPos(pos arbutil.MessageIndex) containers.PromiseInterface[*execution.MessageResult] {
	return stopwaiter.LaunchPromiseThread[*execution.MessageResult](&s.StopWaiterSafe, func(context.Context) (*execution.MessageResult, error) {
		return s.resultFromHeader(s.bc.GetHeaderByNumber(s.MessageIndexToBlockNumber(pos)))
	})
}

func (s *ExecutionEngine) existingMessageResultFor(num arbutil.MessageIndex, msg *arbostypes.MessageWithMetadata) (*execution.MessageResult, error) {
	headMsgNum, err := s.headMessageNumber()
	if err != nil {
		return nil, err
	}
	if headMsgNum < num {
		return nil, fmt.Errorf("msgNum too large got: %d, expected up to: %d", num, headMsgNum)
	}
	msgHash, err := msg.Hash(num, s.bc.Config().ChainID.Uint64())
	if err != nil {
		return nil, err
	}
	expHashKey := messageHashKey(num)
	msgExists, err := s.db.Has(expHashKey)
	if err != nil {
		return nil, err
	}
	if !msgExists {
		return nil, fmt.Errorf("message not found in database: %d head: %d", num, headMsgNum)
	}
	expHashBytes, err := s.db.Get(expHashKey)
	if err != nil {
		return nil, err
	}
	expHash := common.BytesToHash(expHashBytes)
	if msgHash != expHash {
		return nil, fmt.Errorf("wrong hash for msg %d got: %v, expected: %v", num, msgHash, expHash)
	}
	blockNum := s.MessageIndexToBlockNumber(num)
	header := s.bc.GetHeaderByNumber(blockNum)
	return s.resultFromHeader(header)
}

func (s *ExecutionEngine) DigestMessage(num arbutil.MessageIndex, msg *arbostypes.MessageWithMetadata) containers.PromiseInterface[*execution.MessageResult] {
	return stopwaiter.LaunchPromiseThread[*execution.MessageResult](&s.StopWaiterSafe, func(ctx context.Context) (*execution.MessageResult, error) {
		// don't catch locks to handle old existing messages
		result, err := s.existingMessageResultFor(num, msg)
		if err == nil {
			return result, nil
		}

		if !s.createBlocksMutex.TryLock() {
			return nil, errors.New("createBlock mutex held")
		}
		defer s.createBlocksMutex.Unlock()
		currentNum, err := s.headMessageNumber()
		if err != nil {
			return nil, err
		}
		if num > currentNum+1 {
			return nil, fmt.Errorf("wrong message number in digest got %d expected %d", num, currentNum+1)
		} else if num < currentNum+1 {
			return s.existingMessageResultFor(num, msg)
		}
		err = s.digestMessageWithBlockMutex(msg, num)
		if err != nil {
			return nil, err
		}
		sharedmetrics.UpdateSequenceNumberInBlockGauge(num)
		return s.resultFromHeader(s.bc.CurrentHeader())
	})
}

func (s *ExecutionEngine) digestMessageWithBlockMutex(msg *arbostypes.MessageWithMetadata, num arbutil.MessageIndex) error {
	startTime := time.Now()
	block, statedb, receipts, err := s.createBlockFromNextMessage(msg)
	if err != nil {
		return err
	}

	err = s.appendBlockAndMessage(block, statedb, receipts, time.Since(startTime), num, msg)
	if err != nil {
		return err
	}

	if time.Now().After(s.nextScheduledVersionCheck) {
		s.nextScheduledVersionCheck = time.Now().Add(time.Minute)
		arbState, err := arbosState.OpenSystemArbosState(statedb, nil, true)
		if err != nil {
			return err
		}
		version, timestampInt, err := arbState.GetScheduledUpgrade()
		if err != nil {
			return err
		}
		var timeUntilUpgrade time.Duration
		var timestamp time.Time
		if timestampInt == 0 {
			// This upgrade will take effect in the next block
			timestamp = time.Now()
		} else {
			// This upgrade is scheduled for the future
			timestamp = time.Unix(int64(timestampInt), 0)
			timeUntilUpgrade = time.Until(timestamp)
		}
		maxSupportedVersion := params.ArbitrumDevTestChainConfig().ArbitrumChainParams.InitialArbOSVersion
		logLevel := log.Warn
		if timeUntilUpgrade < time.Hour*24 {
			logLevel = log.Error
		}
		if version > maxSupportedVersion {
			logLevel(
				"you need to update your node to the latest version before this scheduled ArbOS upgrade",
				"timeUntilUpgrade", timeUntilUpgrade,
				"upgradeScheduledFor", timestamp,
				"maxSupportedArbosVersion", maxSupportedVersion,
				"pendingArbosUpgradeVersion", version,
			)
		}
	}

	s.latestBlockMutex.Lock()
	s.latestBlock = block
	s.latestBlockMutex.Unlock()
	select {
	case s.newBlockNotifier <- struct{}{}:
	default:
	}
	return nil
}

func (s *ExecutionEngine) Start(ctx_in context.Context) {
	s.StopWaiter.Start(ctx_in, s)
	s.LaunchThread(func(ctx context.Context) {
		for {
			select {
			case <-ctx.Done():
				return
			case resequence := <-s.resequenceChan:
				<-time.After(time.Millisecond * 100) // give consensus time to finish it's reorg
				s.resequenceReorgedMessages(ctx, resequence)
				s.createBlocksMutex.Unlock()
			}
		}
	})
	s.LaunchThread(func(ctx context.Context) {
		var lastBlock *types.Block
		for {
			select {
			case <-s.newBlockNotifier:
			case <-ctx.Done():
				return
			}
			s.latestBlockMutex.Lock()
			block := s.latestBlock
			s.latestBlockMutex.Unlock()
			if block != lastBlock && block != nil {
				log.Info(
					"created block",
					"l2Block", block.Number(),
					"l2BlockHash", block.Hash(),
				)
				lastBlock = block
				select {
				case <-time.After(time.Second):
				case <-ctx.Done():
					return
				}
			}
		}
	})
}
