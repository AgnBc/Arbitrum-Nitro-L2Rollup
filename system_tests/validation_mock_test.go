package arbtest

import (
	"bytes"
	"context"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/offchainlabs/nitro/arbnode"
	"github.com/offchainlabs/nitro/arbos/arbostypes"
	"github.com/offchainlabs/nitro/arbutil"
	"github.com/offchainlabs/nitro/execution"
	"github.com/offchainlabs/nitro/staker"
	"github.com/offchainlabs/nitro/util/containers"
	"github.com/offchainlabs/nitro/util/rpcclient"
	"github.com/offchainlabs/nitro/validator"
	"github.com/offchainlabs/nitro/validator/server_api"
	"github.com/offchainlabs/nitro/validator/server_arb"
)

type mockSpawner struct {
	ExecSpawned []uint64
}

var blockHashKey = common.HexToHash("0x11223344")
var sendRootKey = common.HexToHash("0x55667788")
var batchNumKey = common.HexToHash("0x99aabbcc")
var posInBatchKey = common.HexToHash("0xddeeff")

func globalstateFromTestPreimages(preimages map[common.Hash][]byte) validator.GoGlobalState {
	return validator.GoGlobalState{
		Batch:      new(big.Int).SetBytes(preimages[batchNumKey]).Uint64(),
		PosInBatch: new(big.Int).SetBytes(preimages[posInBatchKey]).Uint64(),
		BlockHash:  common.BytesToHash(preimages[blockHashKey]),
		SendRoot:   common.BytesToHash(preimages[sendRootKey]),
	}
}

func globalstateToTestPreimages(gs validator.GoGlobalState) map[common.Hash][]byte {
	preimages := make(map[common.Hash][]byte)
	preimages[batchNumKey] = new(big.Int).SetUint64(gs.Batch).Bytes()
	preimages[posInBatchKey] = new(big.Int).SetUint64(gs.PosInBatch).Bytes()
	preimages[blockHashKey] = gs.BlockHash[:]
	preimages[sendRootKey] = gs.SendRoot[:]
	return preimages
}

func (s *mockSpawner) Launch(entry *validator.ValidationInput, moduleRoot common.Hash) validator.ValidationRun {
	run := &mockValRun{
		Promise: containers.NewPromise[validator.GoGlobalState](nil),
		root:    moduleRoot,
	}
	run.Produce(globalstateFromTestPreimages(entry.Preimages))
	return run
}

var mockWasmModuleRoot common.Hash = common.HexToHash("0xa5a5a5")

func (s *mockSpawner) Start(context.Context) error { return nil }
func (s *mockSpawner) Stop()                       {}
func (s *mockSpawner) Name() string                { return "mock" }
func (s *mockSpawner) Room() int                   { return 4 }

func (s *mockSpawner) CreateExecutionRun(wasmModuleRoot common.Hash, input *validator.ValidationInput) containers.PromiseInterface[validator.ExecutionRun] {
	promise := containers.NewPromise[validator.ExecutionRun](nil)
	s.ExecSpawned = append(s.ExecSpawned, input.Id)
	if wasmModuleRoot != mockWasmModuleRoot {
		promise.ProduceError(errors.New("unsupported root"))
		return &promise
	}
	promise.Produce(&mockExecRun{
		startState: input.StartState,
		endState:   globalstateFromTestPreimages(input.Preimages),
	})
	return &promise
}

func (s *mockSpawner) LatestWasmModuleRoot() containers.PromiseInterface[common.Hash] {
	promise := containers.NewPromise[common.Hash](nil)
	promise.Produce(mockWasmModuleRoot)
	return &promise
}

func (s *mockSpawner) WriteToFile(input *validator.ValidationInput, expOut validator.GoGlobalState, moduleRoot common.Hash) containers.PromiseInterface[struct{}] {
	promise := containers.NewPromise[struct{}](nil)
	promise.Produce(struct{}{})
	return &promise
}

type mockValRun struct {
	containers.Promise[validator.GoGlobalState]
	root common.Hash
}

func (v *mockValRun) WasmModuleRoot() common.Hash { return v.root }

const mockExecLastPos uint64 = 100

type mockExecRun struct {
	startState validator.GoGlobalState
	endState   validator.GoGlobalState
}

func (r *mockExecRun) GetStepAt(position uint64) containers.PromiseInterface[*validator.MachineStepResult] {
	res := containers.NewPromise[*validator.MachineStepResult](nil)
	status := validator.MachineStatusRunning
	resState := r.startState
	if position >= mockExecLastPos {
		position = mockExecLastPos
		status = validator.MachineStatusFinished
		resState = r.endState
	}
	res.Produce(&validator.MachineStepResult{
		Hash:        crypto.Keccak256Hash(new(big.Int).SetUint64(position).Bytes()),
		Position:    position,
		Status:      status,
		GlobalState: resState,
	})
	return &res
}

func (r *mockExecRun) GetLastStep() containers.PromiseInterface[*validator.MachineStepResult] {
	return r.GetStepAt(mockExecLastPos)
}

var mockProof []byte = []byte("friendly jab at competitors")

func (r *mockExecRun) GetProofAt(uint64) containers.PromiseInterface[[]byte] {
	res := containers.NewPromise[[]byte](nil)
	res.Produce(mockProof)
	return &res
}

func (r *mockExecRun) PrepareRange(uint64, uint64) {}
func (r *mockExecRun) Close()                      {}

func createMockValidationNode(t *testing.T, ctx context.Context, config *server_arb.ArbitratorSpawnerConfig) (*mockSpawner, *node.Node) {
	stackConf := node.DefaultConfig
	stackConf.HTTPPort = 0
	stackConf.DataDir = ""
	stackConf.WSHost = "127.0.0.1"
	stackConf.WSPort = 0
	stackConf.WSModules = []string{server_api.Namespace}
	stackConf.P2P.NoDiscovery = true
	stackConf.P2P.ListenAddr = ""

	stack, err := node.New(&stackConf)
	Require(t, err)

	if config == nil {
		config = &server_arb.DefaultArbitratorSpawnerConfig
	}
	configFetcher := func() *server_arb.ArbitratorSpawnerConfig { return config }
	spawner := &mockSpawner{}
	serverAPI := server_api.NewExecutionServerAPI(spawner, spawner, configFetcher)

	valAPIs := []rpc.API{{
		Namespace:     server_api.Namespace,
		Version:       "1.0",
		Service:       serverAPI,
		Public:        true,
		Authenticated: false,
	}}
	stack.RegisterAPIs(valAPIs)

	err = stack.Start()
	Require(t, err)

	serverAPI.Start(ctx)

	go func() {
		<-ctx.Done()
		stack.Close()
		serverAPI.StopOnly()
	}()

	return spawner, stack
}

// mostly tests translation to/from json and running over network
func TestValidationServerAPI(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, validationDefault := createMockValidationNode(t, ctx, nil)
	rpcConfig := rpcclient.ClientConfig{
		URL: validationDefault.WSEndpoint(),
	}
	client := server_api.NewExecutionClient(&rpcConfig)
	err := client.Start(ctx)
	Require(t, err)

	wasmRoot, err := client.LatestWasmModuleRoot().Await(ctx)
	Require(t, err)

	if wasmRoot != mockWasmModuleRoot {
		t.Error("unexpected mock wasmModuleRoot")
	}

	hash1 := common.HexToHash("0x11223344556677889900aabbccddeeff")
	hash2 := common.HexToHash("0x11111111122222223333333444444444")

	startState := validator.GoGlobalState{
		BlockHash:  hash1,
		SendRoot:   hash2,
		Batch:      300,
		PosInBatch: 3000,
	}
	endState := validator.GoGlobalState{
		BlockHash:  hash2,
		SendRoot:   hash1,
		Batch:      3000,
		PosInBatch: 300,
	}

	valInput := validator.ValidationInput{
		StartState: startState,
		Preimages:  globalstateToTestPreimages(endState),
	}
	valRun := client.Launch(&valInput, wasmRoot)
	res, err := valRun.Await(ctx)
	Require(t, err)
	if res != endState {
		t.Error("unexpected mock validation run")
	}
	execRun, err := client.CreateExecutionRun(wasmRoot, &valInput).Await(ctx)
	Require(t, err)
	step0 := execRun.GetStepAt(0)
	step0Res, err := step0.Await(ctx)
	Require(t, err)
	if step0Res.GlobalState != startState {
		t.Error("unexpected globalstate on execution start")
	}
	lasteStep := execRun.GetLastStep()
	lasteStepRes, err := lasteStep.Await(ctx)
	Require(t, err)
	if lasteStepRes.GlobalState != endState {
		t.Error("unexpected globalstate on execution end")
	}
	proofPromise := execRun.GetProofAt(0)
	proof, err := proofPromise.Await(ctx)
	Require(t, err)
	if !bytes.Equal(proof, mockProof) {
		t.Error("mock proof not expected")
	}
}

func TestExecutionKeepAlive(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, validationDefault := createMockValidationNode(t, ctx, nil)
	shortTimeoutConfig := server_arb.DefaultArbitratorSpawnerConfig
	shortTimeoutConfig.ExecRunTimeout = time.Second
	_, validationShortTO := createMockValidationNode(t, ctx, &shortTimeoutConfig)
	rpcConfig := rpcclient.ClientConfig{
		URL: validationDefault.WSEndpoint(),
	}
	clientDefault := server_api.NewExecutionClient(&rpcConfig)
	err := clientDefault.Start(ctx)
	Require(t, err)
	rpcConfigShort := rpcclient.ClientConfig{
		URL: validationShortTO.WSEndpoint(),
	}
	clientShortTO := server_api.NewExecutionClient(&rpcConfigShort)
	err = clientShortTO.Start(ctx)
	Require(t, err)

	wasmRoot, err := clientDefault.LatestWasmModuleRoot().Await(ctx)
	Require(t, err)

	valInput := validator.ValidationInput{}
	runDefault, err := clientDefault.CreateExecutionRun(wasmRoot, &valInput).Await(ctx)
	Require(t, err)
	runShortTO, err := clientShortTO.CreateExecutionRun(wasmRoot, &valInput).Await(ctx)
	Require(t, err)
	<-time.After(time.Second * 10)
	stepDefault := runDefault.GetStepAt(0)
	stepTO := runShortTO.GetStepAt(0)

	_, err = stepDefault.Await(ctx)
	Require(t, err)
	_, err = stepTO.Await(ctx)
	if err == nil {
		t.Error("getStep should have timed out but didn't")
	}
}

type mockBlockRecorder struct {
	validator *staker.StatelessBlockValidator
	streamer  *arbnode.TransactionStreamer
}

func (m *mockBlockRecorder) RecordBlockCreation(
	pos arbutil.MessageIndex,
	msg *arbostypes.MessageWithMetadata,
) containers.PromiseInterface[*execution.RecordResult] {
	_, globalpos, err := m.validator.GlobalStatePositionsAtCount(pos + 1)
	if err != nil {
		return containers.NewReadyPromise[*execution.RecordResult](nil, err)
	}
	res, err := m.streamer.ResultAtCount(pos + 1)
	if err != nil {
		return containers.NewReadyPromise[*execution.RecordResult](nil, err)
	}
	globalState := validator.GoGlobalState{
		Batch:      globalpos.BatchNumber,
		PosInBatch: globalpos.PosInBatch,
		BlockHash:  res.BlockHash,
		SendRoot:   res.SendRoot,
	}
	recording := &execution.RecordResult{
		Pos:       pos,
		BlockHash: res.BlockHash,
		Preimages: globalstateToTestPreimages(globalState),
	}
	return containers.NewReadyPromise[*execution.RecordResult](recording, nil)

}

func (m *mockBlockRecorder) MarkValid(pos arbutil.MessageIndex, resultHash common.Hash) {}
func (m *mockBlockRecorder) PrepareForRecord(start, end arbutil.MessageIndex) containers.PromiseInterface[struct{}] {
	return containers.NewReadyPromise[struct{}](struct{}{}, nil)
}

func newMockRecorder(validator *staker.StatelessBlockValidator, streamer *arbnode.TransactionStreamer) *mockBlockRecorder {
	return &mockBlockRecorder{validator, streamer}
}
