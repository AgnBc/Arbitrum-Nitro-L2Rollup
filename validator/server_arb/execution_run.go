// Copyright 2021-2022, Offchain Labs, Inc.
// For license information, see https://github.com/nitro/blob/master/LICENSE

package server_arb

import (
	"context"
	"fmt"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/log"

	state_hashes "github.com/OffchainLabs/bold/state-commitments/state-hashes"

	"github.com/offchainlabs/nitro/util/containers"
	"github.com/offchainlabs/nitro/util/stopwaiter"
	"github.com/offchainlabs/nitro/validator"
	"github.com/offchainlabs/nitro/validator/server_common"
)

type executionRun struct {
	stopwaiter.StopWaiter
	cache                *MachineCache
	initialMachineGetter func(context.Context, ...server_common.MachineLoaderOpt) (MachineInterface, error)
	config               *MachineCacheConfig
	close                sync.Once
}

// NewExecutionChallengeBackend creates a backend with the given arguments.
// Note: machineCache may be nil, but if present, it must not have a restricted range.
func NewExecutionRun(
	ctxIn context.Context,
	initialMachineGetter func(context.Context, ...server_common.MachineLoaderOpt) (MachineInterface, error),
	config *MachineCacheConfig,
) (*executionRun, error) {
	exec := &executionRun{}
	exec.Start(ctxIn, exec)
	exec.initialMachineGetter = initialMachineGetter
	exec.config = config
	exec.cache = NewMachineCache(exec.GetContext(), initialMachineGetter, config)
	return exec, nil
}

func (e *executionRun) Close() {
	go e.close.Do(func() {
		e.StopAndWait()
		if e.cache != nil {
			e.cache.Destroy(e.GetParentContext())
		}
	})
}

func (e *executionRun) PrepareRange(start uint64, end uint64) containers.PromiseInterface[struct{}] {
	return stopwaiter.LaunchPromiseThread[struct{}](e, func(ctx context.Context) (struct{}, error) {
		err := e.cache.SetRange(ctx, start, end)
		return struct{}{}, err
	})
}

func (e *executionRun) GetStepAt(position uint64) containers.PromiseInterface[*validator.MachineStepResult] {
	return stopwaiter.LaunchPromiseThread[*validator.MachineStepResult](e, func(ctx context.Context) (*validator.MachineStepResult, error) {
		return e.intermediateGetStepAt(ctx, position)
	})
}

func (e *executionRun) GetLeavesWithStepSize(machineStartIndex, stepSize, numDesiredLeaves uint64) containers.PromiseInterface[*state_hashes.StateHashes] {
	return stopwaiter.LaunchPromiseThread[*state_hashes.StateHashes](e, func(ctx context.Context) (*state_hashes.StateHashes, error) {
		if stepSize == 1 {
			e.cache = NewMachineCache(e.GetContext(), e.initialMachineGetter, e.config, server_common.WithAlwaysMerkleize())
			log.Info("Enabling Merkleization of machines for faster hashing. However, advancing to start index might take a while...")
		}
		log.Info(fmt.Sprintf("Starting BOLD machine computation at index %d", machineStartIndex))
		machine, err := e.cache.GetMachineAt(ctx, machineStartIndex)
		if err != nil {
			return nil, err
		}
		log.Info(fmt.Sprintf("Advanced machine to index %d, beginning hash computation", machineStartIndex))
		// If the machine is starting at index 0, we always want to start at the "Machine finished" global state status
		// to align with the state roots that the inbox machine will produce.
		var stateRoots []common.Hash

		if machineStartIndex == 0 {
			gs := machine.GetGlobalState()
			log.Info(fmt.Sprintf("Start global state for machine index 0: %+v", gs))
			hash := crypto.Keccak256Hash([]byte("Machine finished:"), gs.Hash().Bytes())
			stateRoots = append(stateRoots, hash)
		} else {
			// Otherwise, we simply append the machine hash at the specified start index.
			stateRoots = append(stateRoots, machine.Hash())
		}
		startHash := stateRoots[0]

		// If we only want 1 state root, we can return early.
		if numDesiredLeaves == 1 {
			return state_hashes.NewStateHashes(stateRoots, uint64(len(stateRoots))), nil
		}
		for numIterations := uint64(0); numIterations < numDesiredLeaves; numIterations++ {
			// The absolute opcode position the machine should be in after stepping.
			position := machineStartIndex + stepSize*(numIterations+1)

			// Advance the machine in step size increments.
			if err := machine.Step(ctx, stepSize); err != nil {
				return nil, fmt.Errorf("failed to step machine to position %d: %w", position, err)
			}

			progressPercent := (float64(numIterations+1) / float64(numDesiredLeaves)) * 100
			log.Info(
				fmt.Sprintf(
					"Computing subchallenge machine hashes progress: %.2f%% leaves gathered (%d/%d)",
					progressPercent,
					numIterations+1,
					numDesiredLeaves,
				),
				log.Ctx{
					"stepSize":          stepSize,
					"startHash":         startHash,
					"machineStartIndex": machineStartIndex,
					"numDesiredLeaves":  numDesiredLeaves,
				},
			)

			// If the machine reached the finished state, we can break out of the loop and append to
			// our state roots slice a finished machine hash.
			machineStep := machine.GetStepCount()
			if validator.MachineStatus(machine.Status()) == validator.MachineStatusFinished {
				gs := machine.GetGlobalState()
				hash := crypto.Keccak256Hash([]byte("Machine finished:"), gs.Hash().Bytes())
				stateRoots = append(stateRoots, hash)
				log.Info(
					"Machine finished execution, gathered all the necessary hashes",
					log.Ctx{
						"stepSize":            stepSize,
						"startHash":           startHash,
						"machineStartIndex":   machineStartIndex,
						"numDesiredLeaves":    numDesiredLeaves,
						"finishedHash":        hash,
						"finishedGlobalState": fmt.Sprintf("%+v", gs),
					},
				)
				break
			}
			// Otherwise, if the position and machine step mismatch and the machine is running, something went wrong.
			if position != machineStep {
				machineRunning := machine.IsRunning()
				if machineRunning || machineStep > position {
					return nil, fmt.Errorf("machine is in wrong position want: %d, got: %d", position, machineStep)
				}
			}
			stateRoots = append(stateRoots, machine.Hash())

		}

		// If the machine finished in less than the number of hashes we anticipate, we pad
		// to the expected value by repeating the last machine hash until the state roots are the correct
		// length.
		return state_hashes.NewStateHashes(stateRoots, numDesiredLeaves), nil
	})
}

func (e *executionRun) intermediateGetStepAt(ctx context.Context, position uint64) (*validator.MachineStepResult, error) {
	var machine MachineInterface
	var err error
	if position == ^uint64(0) {
		machine, err = e.cache.GetFinalMachine(ctx)
	} else {
		// todo cache last machina
		machine, err = e.cache.GetMachineAt(ctx, position)
	}
	if err != nil {
		return nil, err
	}
	machineStep := machine.GetStepCount()
	if position != machineStep {
		machineRunning := machine.IsRunning()
		if machineRunning || machineStep > position {
			return nil, fmt.Errorf("machine is in wrong position want: %d, got: %d", position, machine.GetStepCount())
		}

	}
	result := &validator.MachineStepResult{
		Position:    machineStep,
		Status:      validator.MachineStatus(machine.Status()),
		GlobalState: machine.GetGlobalState(),
		Hash:        machine.Hash(),
	}
	return result, nil
}

func (e *executionRun) GetProofAt(position uint64) containers.PromiseInterface[[]byte] {
	return stopwaiter.LaunchPromiseThread[[]byte](e, func(ctx context.Context) ([]byte, error) {
		machine, err := e.cache.GetMachineAt(ctx, position)
		if err != nil {
			return nil, err
		}
		return machine.ProveNextStep(), nil
	})
}

func (e *executionRun) GetLastStep() containers.PromiseInterface[*validator.MachineStepResult] {
	return e.GetStepAt(^uint64(0))
}

func (e *executionRun) CheckAlive(ctx context.Context) error {
	return nil
}
