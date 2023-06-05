// Copyright 2021-2022, Offchain Labs, Inc.
// For license information, see https://github.com/nitro/blob/master/LICENSE

package server_arb

import (
	"context"
	"fmt"
	"github.com/OffchainLabs/challenge-protocol-v2/util"
	"github.com/ethereum/go-ethereum/common"
	"sync"

	"github.com/offchainlabs/nitro/util/containers"
	"github.com/offchainlabs/nitro/util/stopwaiter"
	"github.com/offchainlabs/nitro/validator"
)

type executionRun struct {
	stopwaiter.StopWaiter
	cache *MachineCache
	close sync.Once
}

// NewExecutionChallengeBackend creates a backend with the given arguments.
// Note: machineCache may be nil, but if present, it must not have a restricted range.
func NewExecutionRun(
	ctxIn context.Context,
	initialMachineGetter func(context.Context) (MachineInterface, error),
	config *MachineCacheConfig,
) (*executionRun, error) {
	exec := &executionRun{}
	exec.Start(ctxIn, exec)
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
		var machine MachineInterface
		var err error
		if position == ^uint64(0) {
			machine, err = e.cache.GetFinalMachine(ctx)
		} else {
			// todo cache last machine
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
	})
}

func (e *executionRun) GetBigStepCommitmentUpTo(toBigStep uint64, numOpcodesPerBigStep uint64) containers.PromiseInterface[util.HistoryCommitment] {
	return stopwaiter.LaunchPromiseThread[util.HistoryCommitment](e, func(ctx context.Context) (util.HistoryCommitment, error) {
		var stateRoots []common.Hash
		for i := uint64(0); i < toBigStep; i++ {
			position := i * numOpcodesPerBigStep
			var machine MachineInterface
			var err error
			if position == ^uint64(0) {
				machine, err = e.cache.GetFinalMachine(ctx)
			} else {
				// todo cache last machine
				machine, err = e.cache.GetMachineAt(ctx, position)
			}
			if err != nil {
				return util.HistoryCommitment{}, err
			}
			machineStep := machine.GetStepCount()
			if position != machineStep {
				machineRunning := machine.IsRunning()
				if machineRunning || machineStep > position {
					return util.HistoryCommitment{}, fmt.Errorf("machine is in wrong position want: %d, got: %d", position, machine.GetStepCount())
				}

			}
			stateRoots = append(stateRoots, machine.GetGlobalState().Hash())
		}
		return util.NewHistoryCommitment(toBigStep, stateRoots)
	})
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
