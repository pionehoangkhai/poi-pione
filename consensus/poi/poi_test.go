package poi

import (
    "context"
    "math/big"
    "sync"

    "github.com/ethereum/go-ethereum/common"
    "github.com/ethereum/go-ethereum/core/types"
    "github.com/ethereum/go-ethereum/core/state"
    "github.com/ethereum/go-ethereum/params"
)

// ==== INTERFACES ====

type Backend interface {
    ChainID() *big.Int
}

type ChainReader interface{}

// ==== ENGINE STRUCT ====

type PoI struct {
    config  *params.ChainConfig
    backend Backend
}

func New(config *params.ChainConfig, backend Backend) *PoI {
    return &PoI{
        config:  config,
        backend: backend,
    }
}

// ==== REQUIRED METHODS FOR consensus.Engine ====

func (p *PoI) Author(header *types.Header) (common.Address, error) {
    return header.Coinbase, nil
}

func (p *PoI) VerifyHeader(chain ChainReader, header *types.Header, seal bool) error {
    return nil
}

func (p *PoI) Prepare(chain ChainReader, header *types.Header) error {
    return nil
}

func (p *PoI) Finalize(chain ChainReader, header *types.Header, state *state.StateDB,
    txs []*types.Transaction, uncles []*types.Header, receipts []*types.Receipt) (*types.Block, error) {

    candidates := GetCandidateNodes()
    bestNode := SelectValidator(candidates)
    header.Coinbase = bestNode.Address

    return types.NewBlock(header, txs, uncles, receipts), nil
}

func (p *PoI) Seal(chain ChainReader, block *types.Block, results chan<- *types.Block, stop <-chan struct{}) error {
    results <- block
    return nil
}

// ==== REPUTATION MODULE ====

var repStore = map[string]float64{}
var repLock sync.RWMutex

func UpdateReputation(nodeID string, delta float64) {
    repLock.Lock()
    defer repLock.Unlock()
    repStore[nodeID] += delta
}

func GetReputation(nodeID string) float64 {
    repLock.RLock()
    defer repLock.RUnlock()
    return repStore[nodeID]
}

// ==== PERFORMANCE MODULE ====

type PerformanceMetrics struct {
    Latency      float64
    Throughput   float64
    Availability float64
    Bandwidth    float64
}

var perfStore = map[string]PerformanceMetrics{}
var perfLock sync.RWMutex

func UpdatePerformance(nodeID string, m PerformanceMetrics) {
    perfLock.Lock()
    defer perfLock.Unlock()
    perfStore[nodeID] = m
}

func GetPerformance(nodeID string) PerformanceMetrics {
    perfLock.RLock()
    defer perfLock.RUnlock()
    return perfStore[nodeID]
}

// ==== SCORING MODULE ====

type Node struct {
    ID      string
    Address common.Address
}

func CalculatePoIScore(nodeID string, alpha, beta float64) float64 {
    rep := GetReputation(nodeID)
    perf := GetPerformance(nodeID)

    perfScore := 0.25*perf.Latency + 0.25*perf.Throughput +
        0.25*perf.Availability + 0.25*perf.Bandwidth
    return alpha*rep + beta*perfScore
}

func SelectValidator(nodes []Node) Node {
    var best Node
    var bestScore float64

    for _, n := range nodes {
        score := CalculatePoIScore(n.ID, 0.6, 0.4)
        if score > bestScore {
            best = n
            bestScore = score
        }
    }
    return best
}

func GetCandidateNodes() []Node {
    return []Node{
        {"node1", common.HexToAddress("0x1111111111111111111111111111111111111111")},
        {"node2", common.HexToAddress("0x2222222222222222222222222222222222222222")},
    }
}