// Copyright 2021-2022, Offchain Labs, Inc.
// For license information, see https://github.com/nitro/blob/master/LICENSE

package staker

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pkg/errors"
	flag "github.com/spf13/pflag"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/metrics"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/offchainlabs/nitro/arbutil"
	"github.com/offchainlabs/nitro/util/containers"
	"github.com/offchainlabs/nitro/util/stopwaiter"
	"github.com/offchainlabs/nitro/validator"
)

var (
	validatorPendingValidationsGauge  = metrics.NewRegisteredGauge("arb/validator/validations/pending", nil)
	validatorValidValidationsCounter  = metrics.NewRegisteredCounter("arb/validator/validations/valid", nil)
	validatorFailedValidationsCounter = metrics.NewRegisteredCounter("arb/validator/validations/failed", nil)
	validatorMsgCountCurrentBatch     = metrics.NewRegisteredGauge("arb/validator/msg_count_current_batch", nil)
	validatorMsgCountValidatedGauge   = metrics.NewRegisteredGauge("arb/validator/msg_count_validated", nil)
)

type BlockValidator struct {
	stopwaiter.StopWaiter
	*StatelessBlockValidator

	reorgMutex sync.RWMutex

	chainCaughtUp bool

	// can only be accessed from creation thread or if holding reorg-write
	nextCreateBatch         []byte
	nextCreateBatchMsgCount arbutil.MessageIndex
	nextCreateBatchReread   bool
	nextCreateStartGS       validator.GoGlobalState
	nextCreatePrevDelayed   uint64

	// only used by record loop or holding reorg-write
	prepared           arbutil.MessageIndex
	nextRecordPrepared containers.PromiseInterface[arbutil.MessageIndex]

	// can only be accessed from from validation thread or if holding reorg-write
	lastValidGS        validator.GoGlobalState
	valLoopPos         arbutil.MessageIndex
	validInfoPrintTime time.Time

	// can be read by anyone holding reorg-read
	// written by appropriate thread or reorg-write
	createdA    uint64
	recordSentA uint64
	validatedA  uint64
	validations containers.SyncMap[arbutil.MessageIndex, *validationStatus]

	config BlockValidatorConfigFetcher

	createNodesChan         chan struct{}
	sendRecordChan          chan struct{}
	progressValidationsChan chan struct{}

	// for testing only
	testingProgressMadeChan chan struct{}

	fatalErr chan<- error
}

type BlockValidatorConfig struct {
	Enable                   bool                          `koanf:"enable"`
	URL                      string                        `koanf:"url"`
	JWTSecret                string                        `koanf:"jwtsecret"`
	ValidationPoll           time.Duration                 `koanf:"check-validations-poll" reload:"hot"`
	PrerecordedBlocks        uint64                        `koanf:"prerecorded-blocks" reload:"hot"`
	ForwardBlocks            uint64                        `koanf:"forward-blocks" reload:"hot"`
	CurrentModuleRoot        string                        `koanf:"current-module-root"`         // TODO(magic) requires reinitialization on hot reload
	PendingUpgradeModuleRoot string                        `koanf:"pending-upgrade-module-root"` // TODO(magic) requires StatelessBlockValidator recreation on hot reload
	FailureIsFatal           bool                          `koanf:"failure-is-fatal" reload:"hot"`
	Dangerous                BlockValidatorDangerousConfig `koanf:"dangerous"`
}

type BlockValidatorDangerousConfig struct {
	ResetBlockValidation bool `koanf:"reset-block-validation"`
}

type BlockValidatorConfigFetcher func() *BlockValidatorConfig

func BlockValidatorConfigAddOptions(prefix string, f *flag.FlagSet) {
	f.Bool(prefix+".enable", DefaultBlockValidatorConfig.Enable, "enable block-by-block validation")
	f.String(prefix+".url", DefaultBlockValidatorConfig.URL, "url for valiation")
	f.String(prefix+".jwtsecret", DefaultBlockValidatorConfig.JWTSecret, "path to file with jwtsecret for validation - empty disables jwt, 'self' uses the server's jwt")
	f.Duration(prefix+".check-validations-poll", DefaultBlockValidatorConfig.ValidationPoll, "poll time to check validations")
	f.Uint64(prefix+".forward-blocks", DefaultBlockValidatorConfig.ForwardBlocks, "prepare entries for up to that many blocks ahead of validation (small footprint)")
	f.Uint64(prefix+".prerecorded-blocks", DefaultBlockValidatorConfig.PrerecordedBlocks, "record that many blocks ahead of validation (larger footprint)")
	f.String(prefix+".current-module-root", DefaultBlockValidatorConfig.CurrentModuleRoot, "current wasm module root ('current' read from chain, 'latest' from machines/latest dir, or provide hash)")
	f.String(prefix+".pending-upgrade-module-root", DefaultBlockValidatorConfig.PendingUpgradeModuleRoot, "pending upgrade wasm module root to additionally validate (hash, 'latest' or empty)")
	f.Bool(prefix+".failure-is-fatal", DefaultBlockValidatorConfig.FailureIsFatal, "failing a validation is treated as a fatal error")
	BlockValidatorDangerousConfigAddOptions(prefix+".dangerous", f)
}

func BlockValidatorDangerousConfigAddOptions(prefix string, f *flag.FlagSet) {
	f.Bool(prefix+".reset-block-validation", DefaultBlockValidatorDangerousConfig.ResetBlockValidation, "resets block-by-block validation, starting again at genesis")
}

var DefaultBlockValidatorConfig = BlockValidatorConfig{
	Enable:                   false,
	URL:                      "ws://127.0.0.1:8549/",
	JWTSecret:                "self",
	ValidationPoll:           time.Second,
	ForwardBlocks:            1024,
	PrerecordedBlocks:        128,
	CurrentModuleRoot:        "current",
	PendingUpgradeModuleRoot: "latest",
	FailureIsFatal:           true,
	Dangerous:                DefaultBlockValidatorDangerousConfig,
}

var TestBlockValidatorConfig = BlockValidatorConfig{
	Enable:                   false,
	URL:                      "",
	JWTSecret:                "",
	ValidationPoll:           100 * time.Millisecond,
	ForwardBlocks:            128,
	PrerecordedBlocks:        64,
	CurrentModuleRoot:        "latest",
	PendingUpgradeModuleRoot: "latest",
	FailureIsFatal:           true,
	Dangerous:                DefaultBlockValidatorDangerousConfig,
}

var DefaultBlockValidatorDangerousConfig = BlockValidatorDangerousConfig{
	ResetBlockValidation: false,
}

type valStatusField uint32

const (
	Created valStatusField = iota
	RecordSent
	RecordFailed
	Prepared
	SendingValidation
	ValidationSent
)

type validationStatus struct {
	Status uint32                    // atomic: value is one of validationStatus*
	Cancel func()                    // non-atomic: only read/written to with reorg mutex
	Entry  *validationEntry          // non-atomic: only read if Status >= validationStatusPrepared
	Runs   []validator.ValidationRun // if status >= ValidationSent
}

func (s *validationStatus) getStatus() valStatusField {
	uintStat := atomic.LoadUint32(&s.Status)
	return valStatusField(uintStat)
}

func (s *validationStatus) replaceStatus(old, new valStatusField) bool {
	return atomic.CompareAndSwapUint32(&s.Status, uint32(old), uint32(new))
}

func NewBlockValidator(
	statelessBlockValidator *StatelessBlockValidator,
	inbox InboxTrackerInterface,
	streamer TransactionStreamerInterface,
	config BlockValidatorConfigFetcher,
	fatalErr chan<- error,
) (*BlockValidator, error) {
	ret := &BlockValidator{
		StatelessBlockValidator: statelessBlockValidator,
		createNodesChan:         make(chan struct{}, 1),
		sendRecordChan:          make(chan struct{}, 1),
		progressValidationsChan: make(chan struct{}, 1),
		config:                  config,
		fatalErr:                fatalErr,
	}
	if !config().Dangerous.ResetBlockValidation {
		validated, err := ret.ReadLastValidatedInfo()
		if err != nil {
			return nil, err
		}
		if validated != nil {
			ret.lastValidGS = validated.GlobalState
		}
	}
	streamer.SetBlockValidator(ret)
	inbox.SetBlockValidator(ret)
	return ret, nil
}

func atomicStorePos(addr *uint64, val arbutil.MessageIndex) {
	atomic.StoreUint64(addr, uint64(val))
}

func atomicLoadPos(addr *uint64) arbutil.MessageIndex {
	return arbutil.MessageIndex(atomic.LoadUint64(addr))
}

func (v *BlockValidator) created() arbutil.MessageIndex {
	return atomicLoadPos(&v.createdA)
}

func (v *BlockValidator) recordSent() arbutil.MessageIndex {
	return atomicLoadPos(&v.recordSentA)
}

func (v *BlockValidator) validated() arbutil.MessageIndex {
	return atomicLoadPos(&v.validatedA)
}

func (v *BlockValidator) Validated(t *testing.T) arbutil.MessageIndex {
	return v.validated()
}

func (v *BlockValidator) possiblyFatal(err error) {
	if v.Stopped() {
		return
	}
	if err == nil {
		return
	}
	log.Error("Error during validation", "err", err)
	if v.config().FailureIsFatal {
		select {
		case v.fatalErr <- err:
		default:
		}
	}
}

func nonBlockingTriger(channel chan struct{}) {
	select {
	case channel <- struct{}{}:
	default:
	}
}

// called from NewBlockValidator, doesn't need to catch locks
func (v *BlockValidator) ReadLastValidatedInfo() (*GlobalStateValidatedInfo, error) {
	exists, err := v.db.Has(lastGlobalStateValidatedInfoKey)
	if err != nil {
		return nil, err
	}
	var validated GlobalStateValidatedInfo
	if !exists {
		return nil, nil
	}
	gsBytes, err := v.db.Get(lastGlobalStateValidatedInfoKey)
	if err != nil {
		return nil, err
	}
	err = rlp.DecodeBytes(gsBytes, &validated)
	if err != nil {
		return nil, err
	}
	return &validated, nil
}

var ErrGlobalStateNotInChain = errors.New("globalstate not in chain")

// false if chain not caught up to globalstate
// error is ErrGlobalStateNotInChain if globalstate not in chain (and chain caught up)
func GlobalStateToMsgCount(tracker InboxTrackerInterface, streamer TransactionStreamerInterface, gs validator.GoGlobalState) (bool, arbutil.MessageIndex, error) {
	batchCount, err := tracker.GetBatchCount()
	if err != nil {
		return false, 0, err
	}
	if batchCount <= gs.Batch {
		return false, 0, nil
	}
	var prevBatchMsgCount arbutil.MessageIndex
	if gs.Batch > 0 {
		prevBatchMsgCount, err = tracker.GetBatchMessageCount(gs.Batch - 1)
		if err != nil {
			return false, 0, err
		}
	}
	count := prevBatchMsgCount
	if gs.PosInBatch > 0 {
		curBatchMsgCount, err := tracker.GetBatchMessageCount(gs.Batch)
		if err != nil {
			return false, 0, fmt.Errorf("%w: getBatchMsgCount %d batchCount %d", err, gs.Batch, batchCount)
		}
		count += arbutil.MessageIndex(gs.PosInBatch)
		if curBatchMsgCount < count {
			return false, 0, fmt.Errorf("%w: batch %d posInBatch %d, maxPosInBatch %d", ErrGlobalStateNotInChain, gs.Batch, gs.PosInBatch, curBatchMsgCount-prevBatchMsgCount)
		}
	}
	processed, err := streamer.GetProcessedMessageCount()
	if err != nil {
		return false, 0, err
	}
	if processed < count {
		return false, 0, nil
	}
	res, err := streamer.ResultAtCount(count)
	if err != nil {
		return false, 0, err
	}
	if res.BlockHash != gs.BlockHash || res.SendRoot != gs.SendRoot {
		return false, 0, fmt.Errorf("%w: count %d hash %v expected %v, sendroot %v expected %v", ErrGlobalStateNotInChain, count, gs.BlockHash, res.BlockHash, gs.SendRoot, res.SendRoot)
	}
	return true, count, nil
}

func (v *BlockValidator) checkValidatedGSCaughUp(ctx context.Context) (bool, error) {
	v.reorgMutex.Lock()
	defer v.reorgMutex.Unlock()
	if v.chainCaughtUp {
		return true, nil
	}
	if v.lastValidGS.Batch == 0 {
		return false, errors.New("lastValid not initialized. cannot validate genesis")
	}
	caughtUp, count, err := GlobalStateToMsgCount(v.inboxTracker, v.streamer, v.lastValidGS)
	if err != nil {
		return false, err
	}
	if !caughtUp {
		return false, nil
	}
	msg, err := v.streamer.GetMessage(count - 1)
	if err != nil {
		return false, err
	}
	v.nextCreateBatchReread = true
	v.nextCreateStartGS = v.lastValidGS
	v.nextCreatePrevDelayed = msg.DelayedMessagesRead
	atomicStorePos(&v.createdA, count)
	atomicStorePos(&v.recordSentA, count)
	atomicStorePos(&v.validatedA, count)
	validatorMsgCountValidatedGauge.Update(int64(count))
	v.chainCaughtUp = true
	return true, nil
}

func (v *BlockValidator) sendRecord(s *validationStatus) error {
	if !v.Started() {
		return nil
	}
	if !s.replaceStatus(Created, RecordSent) {
		return errors.Errorf("failed status check for send record. Status: %v", s.getStatus())
	}
	v.LaunchThread(func(ctx context.Context) {
		err := v.ValidationEntryRecord(ctx, s.Entry)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			s.replaceStatus(RecordSent, RecordFailed) // after that - could be removed from validations map
			log.Error("Error while recording", "err", err, "status", s.getStatus())
			return
		}
		if !s.replaceStatus(RecordSent, Prepared) {
			log.Error("Fault trying to update validation with recording", "entry", s.Entry, "status", s.getStatus())
			return
		}
		nonBlockingTriger(v.progressValidationsChan)
	})
	return nil
}

//nolint:gosec
func (v *BlockValidator) writeToFile(validationEntry *validationEntry, moduleRoot common.Hash) error {
	input, err := validationEntry.ToInput()
	if err != nil {
		return err
	}
	_, err = v.execSpawner.WriteToFile(input, validationEntry.End, moduleRoot).Await(v.GetContext())
	return err
}

func (v *BlockValidator) SetCurrentWasmModuleRoot(hash common.Hash) error {
	v.moduleMutex.Lock()
	defer v.moduleMutex.Unlock()

	if (hash == common.Hash{}) {
		return errors.New("trying to set zero as wasmModuleRoot")
	}
	if hash == v.currentWasmModuleRoot {
		return nil
	}
	if (v.currentWasmModuleRoot == common.Hash{}) {
		v.currentWasmModuleRoot = hash
		return nil
	}
	if v.pendingWasmModuleRoot == hash {
		log.Info("Block validator: detected progressing to pending machine", "hash", hash)
		v.currentWasmModuleRoot = hash
		return nil
	}
	if v.config().CurrentModuleRoot != "current" {
		return nil
	}
	return fmt.Errorf(
		"unexpected wasmModuleRoot! cannot validate! found %v , current %v, pending %v",
		hash, v.currentWasmModuleRoot, v.pendingWasmModuleRoot,
	)
}

func (v *BlockValidator) readBatch(ctx context.Context, batchNum uint64) (bool, []byte, arbutil.MessageIndex, error) {
	batchCount, err := v.inboxTracker.GetBatchCount()
	if err != nil {
		return false, nil, 0, err
	}
	if batchCount < batchNum {
		return false, nil, 0, nil
	}
	batchMsgCount, err := v.inboxTracker.GetBatchMessageCount(batchNum)
	if err != nil {
		return false, nil, 0, err
	}
	batch, err := v.inboxReader.GetSequencerMessageBytes(batchNum).Await(ctx)
	if err != nil {
		return false, nil, 0, err
	}
	return true, batch, batchMsgCount, nil
}

func (v *BlockValidator) createNextValidationEntry(ctx context.Context) (bool, error) {
	v.reorgMutex.RLock()
	defer v.reorgMutex.RUnlock()
	pos := v.created()
	if pos > v.validated()+arbutil.MessageIndex(v.config().ForwardBlocks) {
		return false, nil
	}
	streamerMsgCount, err := v.streamer.GetProcessedMessageCount()
	if err != nil {
		return false, err
	}
	if pos >= streamerMsgCount {
		return false, nil
	}
	msg, err := v.streamer.GetMessage(pos)
	if err != nil {
		return false, err
	}
	endRes, err := v.streamer.ResultAtCount(pos + 1)
	if err != nil {
		return false, err
	}
	if v.nextCreateStartGS.PosInBatch == 0 || v.nextCreateBatchReread {
		// new batch
		found, batch, count, err := v.readBatch(ctx, v.nextCreateStartGS.Batch)
		if !found {
			return false, err
		}
		v.nextCreateBatch = batch
		v.nextCreateBatchMsgCount = count
		validatorMsgCountCurrentBatch.Update(int64(count))
		v.nextCreateBatchReread = false
	}
	endGS := validator.GoGlobalState{
		BlockHash: endRes.BlockHash,
		SendRoot:  endRes.SendRoot,
	}
	if pos+1 < v.nextCreateBatchMsgCount {
		endGS.Batch = v.nextCreateStartGS.Batch
		endGS.PosInBatch = v.nextCreateStartGS.PosInBatch + 1
	} else if pos+1 == v.nextCreateBatchMsgCount {
		endGS.Batch = v.nextCreateStartGS.Batch + 1
		endGS.PosInBatch = 0
	} else {
		return false, fmt.Errorf("illegal batch msg count %d pos %d batch %d", v.nextCreateBatchMsgCount, pos, endGS.Batch)
	}
	entry, err := newValidationEntry(pos, v.nextCreateStartGS, endGS, msg, v.nextCreateBatch, v.nextCreatePrevDelayed)
	if err != nil {
		return false, err
	}
	status := &validationStatus{
		Status: uint32(Created),
		Entry:  entry,
	}
	v.validations.Store(pos, status)
	v.nextCreateStartGS = endGS
	v.nextCreatePrevDelayed = msg.DelayedMessagesRead
	atomicStorePos(&v.createdA, pos+1)
	return true, nil
}

func (v *BlockValidator) iterativeValidationEntryCreator(ctx context.Context, ignored struct{}) time.Duration {
	moreWork, err := v.createNextValidationEntry(ctx)
	if err != nil {
		processed, processedErr := v.streamer.GetProcessedMessageCount()
		log.Error("error trying to create validation node", "err", err, "created", v.created()+1, "processed", processed, "processedErr", processedErr)
	}
	if moreWork {
		return 0
	}
	return v.config().ValidationPoll
}

func (v *BlockValidator) sendNextRecordPrepare() error {
	if v.nextRecordPrepared != nil {
		if v.nextRecordPrepared.Ready() {
			prepared, err := v.nextRecordPrepared.Current()
			if err != nil {
				return err
			}
			if prepared > v.prepared {
				v.prepared = prepared
			}
			v.nextRecordPrepared = nil
		} else {
			return nil
		}
	}
	nextPrepared := v.validated() + arbutil.MessageIndex(v.config().PrerecordedBlocks)
	created := v.created()
	if nextPrepared > created {
		nextPrepared = created
	}
	if v.prepared >= nextPrepared {
		return nil
	}
	nextPromise := stopwaiter.LaunchPromiseThread[arbutil.MessageIndex](&v.StopWaiterSafe, func(ctx context.Context) (arbutil.MessageIndex, error) {
		_, err := v.recorder.PrepareForRecord(v.prepared, nextPrepared-1).Await(ctx)
		if err != nil {
			return 0, err
		}
		nonBlockingTriger(v.sendRecordChan)
		return nextPrepared, nil
	})
	v.nextRecordPrepared = nextPromise
	return nil
}

func (v *BlockValidator) sendNextRecordRequest(ctx context.Context) (bool, error) {
	v.reorgMutex.RLock()
	defer v.reorgMutex.RUnlock()
	err := v.sendNextRecordPrepare()
	if err != nil {
		return false, err
	}
	pos := v.recordSent()
	if pos >= v.prepared {
		return false, nil
	}
	validationStatus, found := v.validations.Load(pos)
	if !found {
		return false, fmt.Errorf("not found entry for pos %d", pos)
	}
	currentStatus := validationStatus.getStatus()
	if currentStatus != Created {
		return false, fmt.Errorf("bad status trying to send recordings for pos %d status: %v", pos, currentStatus)
	}
	err = v.sendRecord(validationStatus)
	if err != nil {
		return false, err
	}
	atomicStorePos(&v.recordSentA, pos+1)
	return true, nil
}

func (v *BlockValidator) iterativeValidationEntryRecorder(ctx context.Context, ignored struct{}) time.Duration {
	moreWork, err := v.sendNextRecordRequest(ctx)
	if err != nil {
		log.Error("error trying to record for validation node", "err", err)
	}
	if moreWork {
		return 0
	}
	return v.config().ValidationPoll
}

func (v *BlockValidator) maybePrintNewlyValid() {
	if time.Since(v.validInfoPrintTime) > time.Second {
		log.Info("result validated", "count", v.validated(), "blockHash", v.lastValidGS.BlockHash)
		v.validInfoPrintTime = time.Now()
	} else {
		log.Trace("result validated", "count", v.validated(), "blockHash", v.lastValidGS.BlockHash)
	}
}

// return val:
// *MessageIndex - pointer to bad entry if there is one (requires reorg)
func (v *BlockValidator) advanceValidations(ctx context.Context) (*arbutil.MessageIndex, error) {
	v.reorgMutex.RLock()
	defer v.reorgMutex.RUnlock()

	wasmRoots := v.GetModuleRootsToValidate()
	room := 100 // even if there is more room then that it's fine
	for _, spawner := range v.validationSpawners {
		here := spawner.Room() / len(wasmRoots)
		if here <= 0 {
			room = 0
		}
		if here < room {
			room = here
		}
	}
	pos := v.validated() - 1 // to reverse the first +1 in the loop
validatiosLoop:
	for {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		v.valLoopPos = pos + 1
		v.reorgMutex.RUnlock()
		v.reorgMutex.RLock()
		pos = v.valLoopPos
		if pos >= v.recordSent() {
			return nil, nil
		}
		validationStatus, found := v.validations.Load(pos)
		if !found {
			return nil, fmt.Errorf("not found entry for pos %d", pos)
		}
		currentStatus := validationStatus.getStatus()
		if currentStatus == RecordFailed {
			// retry
			log.Warn("Recording for validation failed, retrying..", "pos", pos)
			return &pos, nil
		}
		if currentStatus == ValidationSent && pos == v.validated() {
			if validationStatus.Entry.Start != v.lastValidGS {
				log.Warn("Validation entry has wrong start state", "pos", pos, "start", validationStatus.Entry.Start, "expected", v.lastValidGS)
				validationStatus.Cancel()
				return &pos, nil
			}
			var wasmRoots []common.Hash
			for _, run := range validationStatus.Runs {
				if !run.Ready() {
					continue validatiosLoop
				}
				wasmRoots = append(wasmRoots, run.WasmModuleRoot())
				runEnd, err := run.Current()
				if err == nil && runEnd != validationStatus.Entry.End {
					err = fmt.Errorf("validation failed: expected %v got %v", validationStatus.Entry.End, runEnd)
					writeErr := v.writeToFile(validationStatus.Entry, run.WasmModuleRoot())
					if writeErr != nil {
						log.Warn("failed to write debug results file", "err", writeErr)
					}
				}
				if err != nil {
					validatorFailedValidationsCounter.Inc(1)
					v.possiblyFatal(err)
					return &pos, nil // if not fatal - retry
				}
				validatorValidValidationsCounter.Inc(1)
			}
			v.lastValidGS = validationStatus.Entry.End
			go v.recorder.MarkValid(pos, v.lastValidGS.BlockHash)
			err := v.writeLastValidatedToDb(validationStatus.Entry.End, wasmRoots)
			if err != nil {
				log.Error("failed writing new validated to database", "pos", pos, "err", err)
			}
			atomicStorePos(&v.validatedA, pos+1)
			nonBlockingTriger(v.createNodesChan)
			nonBlockingTriger(v.sendRecordChan)
			validatorMsgCountValidatedGauge.Update(int64(pos + 1))
			if v.testingProgressMadeChan != nil {
				nonBlockingTriger(v.testingProgressMadeChan)
			}
			v.maybePrintNewlyValid()
			continue
		}
		if room == 0 {
			return nil, nil
		}
		if currentStatus == Prepared {
			replaced := validationStatus.replaceStatus(Prepared, SendingValidation)
			if !replaced {
				v.possiblyFatal(errors.New("failed to set SendingValidation status"))
			}
			v.LaunchThread(func(ctx context.Context) {
				validationCtx, cancel := context.WithCancel(ctx)
				defer cancel()
				validationStatus.Cancel = cancel
				input, err := validationStatus.Entry.ToInput()
				if err != nil && validationCtx.Err() == nil {
					v.possiblyFatal(fmt.Errorf("%w: error preparing validation", err))
					return
				}
				validatorPendingValidationsGauge.Inc(1)
				defer validatorPendingValidationsGauge.Dec(1)
				var runs []validator.ValidationRun
				for _, moduleRoot := range wasmRoots {
					for _, spawner := range v.validationSpawners {
						run := spawner.Launch(input, moduleRoot)
						runs = append(runs, run)
					}
				}
				validationStatus.Runs = runs
				replaced := validationStatus.replaceStatus(SendingValidation, ValidationSent)
				if !replaced {
					v.possiblyFatal(errors.New("failed to set status to ValidationSent"))
				}
				// validationStatus might be removed from under us
				// trigger validation progress when done
				for _, run := range runs {
					_, err := run.Await(ctx)
					if err != nil {
						return
					}
				}
				nonBlockingTriger(v.progressValidationsChan)
			})
			room--
		}
	}
}

func (v *BlockValidator) iterativeValidationProgress(ctx context.Context, ignored struct{}) time.Duration {
	reorg, err := v.advanceValidations(ctx)
	if err != nil {
		log.Error("error trying to record for validation node", "err", err)
	} else if reorg != nil {
		err := v.Reorg(ctx, *reorg)
		if err != nil {
			log.Error("error trying to rorg validation", "pos", *reorg-1, "err", err)
			v.possiblyFatal(err)
		}
	}
	return v.config().ValidationPoll
}

var ErrValidationCanceled = errors.New("validation of block cancelled")

func (v *BlockValidator) writeLastValidatedToDb(gs validator.GoGlobalState, wasmRoots []common.Hash) error {
	info := GlobalStateValidatedInfo{
		GlobalState: gs,
		WasmRoots:   wasmRoots,
	}
	encoded, err := rlp.EncodeToBytes(info)
	if err != nil {
		return err
	}
	err = v.db.Put(lastGlobalStateValidatedInfoKey, encoded)
	if err != nil {
		return err
	}
	return nil
}

func (v *BlockValidator) AssumeValid(globalState validator.GoGlobalState) error {
	if v.Started() {
		return errors.Errorf("cannot handle AssumeValid while running")
	}

	// don't do anything if we already validated past that
	if v.lastValidGS.Batch > globalState.Batch {
		return nil
	}
	if v.lastValidGS.Batch == globalState.Batch && v.lastValidGS.PosInBatch > globalState.PosInBatch {
		return nil
	}

	v.lastValidGS = globalState
	return nil
}

// Because batches and blocks are handled at separate layers in the node,
// and because block generation from messages is asynchronous,
// this call is different than Reorg, which is currently called later.
func (v *BlockValidator) ReorgToBatchCount(count uint64) {
	v.reorgMutex.Lock()
	defer v.reorgMutex.Unlock()
	if v.nextCreateStartGS.Batch >= count {
		v.nextCreateBatchReread = true
	}
}

func (v *BlockValidator) Reorg(ctx context.Context, count arbutil.MessageIndex) error {
	v.reorgMutex.Lock()
	defer v.reorgMutex.Unlock()
	if count <= 1 {
		return errors.New("cannot reorg out genesis")
	}
	if !v.chainCaughtUp {
		return nil
	}
	if v.created() < count {
		return nil
	}
	_, endPosition, err := v.GlobalStatePositionsAtCount(count)
	if err != nil {
		v.possiblyFatal(err)
		return err
	}
	res, err := v.streamer.ResultAtCount(count)
	if err != nil {
		v.possiblyFatal(err)
		return err
	}
	msg, err := v.streamer.GetMessage(count - 1)
	if err != nil {
		v.possiblyFatal(err)
		return err
	}
	for iPos := count; iPos < v.created(); iPos++ {
		status, found := v.validations.Load(iPos)
		if found && status != nil && status.Cancel != nil {
			status.Cancel()
		}
		v.validations.Delete(iPos)
	}
	v.nextCreateStartGS = buildGlobalState(*res, endPosition)
	v.nextCreatePrevDelayed = msg.DelayedMessagesRead
	v.nextCreateBatchReread = true
	countUint64 := uint64(count)
	v.createdA = countUint64
	// under the reorg mutex we don't need atomic access
	if v.recordSentA > countUint64 {
		v.recordSentA = countUint64
	}
	if v.validatedA > countUint64 {
		v.validatedA = countUint64
		validatorMsgCountValidatedGauge.Update(int64(countUint64))
		v.lastValidGS = v.nextCreateStartGS
		err := v.writeLastValidatedToDb(v.lastValidGS, []common.Hash{}) // we don't know which wasm roots were validated
		if err != nil {
			log.Error("failed writing valid state after reorg", "err", err)
		}
	}
	if v.prepared > count {
		v.prepared = count
	}
	nonBlockingTriger(v.createNodesChan)
	return nil
}

// Initialize must be called after SetCurrentWasmModuleRoot sets the current one
func (v *BlockValidator) Initialize(ctx context.Context) error {
	config := v.config()
	currentModuleRoot := config.CurrentModuleRoot
	switch currentModuleRoot {
	case "latest":
		latest, err := v.execSpawner.LatestWasmModuleRoot().Await(ctx)
		if err != nil {
			return err
		}
		v.currentWasmModuleRoot = latest
	case "current":
		if (v.currentWasmModuleRoot == common.Hash{}) {
			return errors.New("wasmModuleRoot set to 'current' - but info not set from chain")
		}
	default:
		v.currentWasmModuleRoot = common.HexToHash(currentModuleRoot)
		if (v.currentWasmModuleRoot == common.Hash{}) {
			return errors.New("current-module-root config value illegal")
		}
	}
	log.Info("BlockValidator initialized", "current", v.currentWasmModuleRoot, "pending", v.pendingWasmModuleRoot)
	return nil
}

func (v *BlockValidator) LaunchWorkthreadsWhenCaughtUp(ctx context.Context) {
	for {
		caughtUp, err := v.checkValidatedGSCaughUp(ctx)
		if err != nil {
			log.Error("validator got error waiting for chain to catch up", "err", err)
		}
		if caughtUp {
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(v.config().ValidationPoll):
		}
	}
	err := stopwaiter.CallIterativelyWith[struct{}](&v.StopWaiterSafe, v.iterativeValidationEntryCreator, v.createNodesChan)
	if err != nil {
		v.possiblyFatal(err)
	}
	err = stopwaiter.CallIterativelyWith[struct{}](&v.StopWaiterSafe, v.iterativeValidationEntryRecorder, v.sendRecordChan)
	if err != nil {
		v.possiblyFatal(err)
	}
	err = stopwaiter.CallIterativelyWith[struct{}](&v.StopWaiterSafe, v.iterativeValidationProgress, v.progressValidationsChan)
	if err != nil {
		v.possiblyFatal(err)
	}
}

func (v *BlockValidator) Start(ctxIn context.Context) error {
	v.StopWaiter.Start(ctxIn, v)
	// genesis block is impossible to validate unless genesis state is empty
	v.reorgMutex.Lock()
	defer v.reorgMutex.Unlock()
	if v.lastValidGS.Batch == 0 {
		genesis, err := v.streamer.ResultAtCount(1)
		if err != nil {
			return err
		}
		v.lastValidGS = validator.GoGlobalState{
			BlockHash:  genesis.BlockHash,
			SendRoot:   genesis.SendRoot,
			Batch:      1,
			PosInBatch: 0,
		}
	}
	v.LaunchThread(v.LaunchWorkthreadsWhenCaughtUp)
	return nil
}

func (v *BlockValidator) StopAndWait() {
	v.StopWaiter.StopAndWait()
}

// WaitForPos can only be used from One thread
func (v *BlockValidator) WaitForPos(t *testing.T, ctx context.Context, pos arbutil.MessageIndex, timeout time.Duration) bool {
	trigerchan := make(chan struct{})
	v.testingProgressMadeChan = trigerchan
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	lastLoop := false
	for {
		if v.validated() > pos {
			return true
		}
		if lastLoop {
			return false
		}
		select {
		case <-timer.C:
			lastLoop = true
		case <-trigerchan:
		case <-ctx.Done():
			lastLoop = true
		}
	}
}
