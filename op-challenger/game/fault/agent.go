package fault

import (
	"context"
	"errors"
	"fmt"

	"github.com/ethereum-optimism/optimism/op-challenger/game/fault/solver"
	"github.com/ethereum-optimism/optimism/op-challenger/game/fault/types"
	gameTypes "github.com/ethereum-optimism/optimism/op-challenger/game/types"
	"github.com/ethereum-optimism/optimism/op-challenger/metrics"
	"github.com/ethereum/go-ethereum/log"
)

// Responder takes a response action & executes.
// For full op-challenger this means executing the transaction on chain.
type Responder interface {
	CallResolve(ctx context.Context) (gameTypes.GameStatus, error)
	Resolve(ctx context.Context) error
	PerformAction(ctx context.Context, action types.Action) error
}

type ClaimLoader interface {
	FetchClaims(ctx context.Context) ([]types.Claim, error)
}

type Agent struct {
	metrics                 metrics.Metricer
	solver                  *solver.GameSolver
	loader                  ClaimLoader
	responder               Responder
	updater                 types.OracleUpdater
	maxDepth                int
	agreeWithProposedOutput bool
	log                     log.Logger
}

func NewAgent(m metrics.Metricer, loader ClaimLoader, maxDepth int, trace types.TraceProvider, responder Responder, updater types.OracleUpdater, agreeWithProposedOutput bool, log log.Logger) *Agent {
	return &Agent{
		metrics:                 m,
		solver:                  solver.NewGameSolver(maxDepth, trace),
		loader:                  loader,
		responder:               responder,
		updater:                 updater,
		maxDepth:                maxDepth,
		agreeWithProposedOutput: agreeWithProposedOutput,
		log:                     log,
	}
}

// Act iterates the game & performs all of the next actions.
func (a *Agent) Act(ctx context.Context) error {
	if a.tryResolve(ctx) {
		return nil
	}
	game, err := a.newGameFromContracts(ctx)
	if err != nil {
		return fmt.Errorf("create game from contracts: %w", err)
	}

	// Calculate the actions to take
	actions, err := a.solver.CalculateNextActions(ctx, game)
	if err != nil {
		log.Error("Failed to calculate all required moves", "err", err)
	}

	// Perform the actions
	for _, action := range actions {
		log := a.log.New("action", action.Type, "is_attack", action.IsAttack, "parent", action.ParentIdx, "value", action.Value)

		if action.OracleData != nil {
			a.log.Info("Updating oracle data", "oracleKey", action.OracleData.OracleKey, "oracleData", action.OracleData.OracleData)
			if err := a.updater.UpdateOracle(ctx, action.OracleData); err != nil {
				return fmt.Errorf("failed to load oracle data: %w", err)
			}
		}

		switch action.Type {
		case types.ActionTypeMove:
			a.metrics.RecordGameMove()
		case types.ActionTypeStep:
			a.metrics.RecordGameStep()
		}
		log.Info("Performing action")
		err := a.responder.PerformAction(ctx, action)
		if err != nil {
			log.Error("Action failed", "err", err)
		}
	}
	return nil
}

// shouldResolve returns true if the agent should resolve the game.
// This method will return false if the game is still in progress.
func (a *Agent) shouldResolve(status gameTypes.GameStatus) bool {
	expected := gameTypes.GameStatusDefenderWon
	if a.agreeWithProposedOutput {
		expected = gameTypes.GameStatusChallengerWon
	}
	if expected != status {
		a.log.Warn("Game will be lost", "expected", expected, "actual", status)
	}
	return expected == status
}

// tryResolve resolves the game if it is in a winning state
// Returns true if the game is resolvable (regardless of whether it was actually resolved)
func (a *Agent) tryResolve(ctx context.Context) bool {
	status, err := a.responder.CallResolve(ctx)
	if err != nil || status == gameTypes.GameStatusInProgress {
		return false
	}
	if !a.shouldResolve(status) {
		return true
	}
	a.log.Info("Resolving game")
	if err := a.responder.Resolve(ctx); err != nil {
		a.log.Error("Failed to resolve the game", "err", err)
	}
	return true
}

// newGameFromContracts initializes a new game state from the state in the contract
func (a *Agent) newGameFromContracts(ctx context.Context) (types.Game, error) {
	claims, err := a.loader.FetchClaims(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch claims: %w", err)
	}
	if len(claims) == 0 {
		return nil, errors.New("no claims")
	}
	game := types.NewGameState(a.agreeWithProposedOutput, claims[0], uint64(a.maxDepth))
	if err := game.PutAll(claims[1:]); err != nil {
		return nil, fmt.Errorf("failed to load claims into the local state: %w", err)
	}
	return game, nil
}
