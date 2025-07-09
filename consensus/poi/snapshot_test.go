package poi

import (
    "encoding/json"
    "os"
    "sync"
)

var snapshotLock sync.Mutex

const snapshotFile = "poi_snapshot.json"

type Snapshot struct {
    Reputation map[string]float64            `json:"reputation"`
    Performance map[string]PerformanceMetrics `json:"performance"`
}

func SaveSnapshot() error {
    snapshotLock.Lock()
    defer snapshotLock.Unlock()

    data := Snapshot{
        Reputation: repStore,
        Performance: perfStore,
    }

    file, err := os.Create(snapshotFile)
    if err != nil {
        return err
    }
    defer file.Close()

    encoder := json.NewEncoder(file)
    return encoder.Encode(data)
}

func LoadSnapshot() error {
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

    repStore = data.Reputation
    perfStore = data.Performance
    return nil
}
