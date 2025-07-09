// Package poi implements the RPC API for the PoI consensus engine.
package poi

import (
	"context"
	"errors"

	"github.com/ethereum/go-ethereum/common"
)

type API struct {
	engine *PoIEngine
}

func NewAPI(engine *PoIEngine) *API {
	return &API{engine: engine}
}

type PoIInfo struct {
	NodeID            common.Address `json:"nodeID"`
	Reputation        float64        `json:"reputation"`
	Performance       float64        `json:"performance"`
	Score             float64        `json:"score"`
	BlocksProduced    uint64         `json:"blocksProduced"`
	ConsecutiveBlocks uint64         `json:"consecutiveBlocks"`
	CooldownUntil     uint64         `json:"cooldownUntil"`
	LastSeenBlock     uint64         `json:"lastSeenBlock"`
}

func (api *API) GetReputation(ctx context.Context, addr common.Address) (float64, error) {
	return api.engine.getReputation(addr), nil
}

func (api *API) GetPerformance(ctx context.Context, addr common.Address) (float64, error) {
	return api.engine.getPerformance(addr), nil
}

func (api *API) GetPoIScore(ctx context.Context, addr common.Address) (float64, error) {
	rep := api.engine.getReputation(addr)
	perf := api.engine.getPerformance(addr)
	return api.engine.config.Alpha*rep + api.engine.config.Beta*perf, nil
}

func (api *API) GetNodeInfo(ctx context.Context, addr common.Address) (*PoIInfo, error) {
	state := api.engine.validatorStates[addr]
	if state == nil {
		return nil, errors.New("validator not found")
	}
	rep := api.engine.getReputation(addr)
	perf := api.engine.getPerformance(addr)
	score := api.engine.config.Alpha*rep + api.engine.config.Beta*perf
	return &PoIInfo{
		NodeID:            addr,
		Reputation:        rep,
		Performance:       perf,
		Score:             score,
		BlocksProduced:    state.BlocksProduced,
		ConsecutiveBlocks: state.ConsecutiveBlocks,
		CooldownUntil:     state.CooldownUntilBlock,
		LastSeenBlock:     state.LastSeenBlock,
	}, nil
}
