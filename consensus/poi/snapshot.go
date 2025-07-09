package poi

import (
	"encoding/json"
	"os"
	"sync"

	"github.com/ethereum/go-ethereum/common"
)

var snapshotLock sync.Mutex

const snapshotFile = "poi_snapshot.json"

type PerformanceMetrics float64

type Snapshot struct {
	Reputation  map[string]float64            `json:"reputation"`
	Performance map[string]PerformanceMetrics `json:"performance"`
}

func (e *PoIEngine) SaveSnapshot() error {
	snapshotLock.Lock()
	defer snapshotLock.Unlock()

	reputation := make(map[string]float64)
	performance := make(map[string]PerformanceMetrics)
	for addr, score := range e.repStore {
		reputation[addr.Hex()] = score
	}
	for addr, perf := range e.perfStore {
		performance[addr.Hex()] = PerformanceMetrics(perf)
	}

	data := Snapshot{
		Reputation:  reputation,
		Performance: performance,
	}

	file, err := os.Create(snapshotFile)
	if err != nil {
		return err
	}
	defer file.Close()

	encoder := json.NewEncoder(file)
	return encoder.Encode(data)
}

func (e *PoIEngine) LoadSnapshot() error {
	snapshotLock.Lock()
	defer snapshotLock.Unlock()

	file, err := os.Open(snapshotFile)
	if err != nil {
		return err
	}
	defer file.Close()

	var data Snapshot
	decoder := json.NewDecoder(file)
	err = decoder.Decode(&data)
	if err != nil {
		return err
	}

	e.repStore = make(map[common.Address]float64)
	e.perfStore = make(map[common.Address]float64)

	for str, score := range data.Reputation {
		e.repStore[common.HexToAddress(str)] = score
	}
	for str, perf := range data.Performance {
		e.perfStore[common.HexToAddress(str)] = float64(perf)
	}

	return nil
}