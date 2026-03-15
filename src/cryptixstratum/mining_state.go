package cryptixstratum

import (
	"math/big"
	"sync"
	"time"

	"github.com/cryptix-network/cryptix-stratum-bridge-v3/src/gostratum"
	"github.com/cryptix-network/cryptixd/app/appmessage"
)

const maxjobs = 32

type MiningState struct {
	Jobs        map[int]*appmessage.RPCBlock
	JobLock     sync.Mutex
	StateLock   sync.RWMutex
	jobCounter  int
	bigDiff     big.Int
	initialized bool
	useBigJob   bool
	connectTime time.Time
	stratumDiff *cryptixDiff
}

func MiningStateGenerator() any {
	return &MiningState{
		Jobs:        map[int]*appmessage.RPCBlock{},
		JobLock:     sync.Mutex{},
		StateLock:   sync.RWMutex{},
		connectTime: time.Now(),
	}
}

func GetMiningState(ctx *gostratum.StratumContext) *MiningState {
	return ctx.State.(*MiningState)
}

func (ms *MiningState) AddJob(job *appmessage.RPCBlock) int {
	ms.JobLock.Lock()
	defer ms.JobLock.Unlock()
	ms.jobCounter++
	idx := ms.jobCounter
	ms.Jobs[idx%maxjobs] = job
	return idx
}

func (ms *MiningState) GetJob(id int) (*appmessage.RPCBlock, bool) {
	ms.JobLock.Lock()
	defer ms.JobLock.Unlock()
	job, exists := ms.Jobs[id%maxjobs]
	return job, exists
}

func (ms *MiningState) InitializeIfNeeded(remoteApp string, minShareDiff float64) bool {
	ms.StateLock.Lock()
	defer ms.StateLock.Unlock()
	if ms.initialized {
		return false
	}

	ms.initialized = true
	ms.useBigJob = bigJobRegex.MatchString(remoteApp)
	ms.stratumDiff = newCryptixDiff()
	ms.stratumDiff.setDiffValue(minShareDiff)
	return true
}

func (ms *MiningState) SetNetworkTarget(bits uint64) float64 {
	ms.StateLock.Lock()
	defer ms.StateLock.Unlock()
	ms.bigDiff = CalculateTarget(bits)
	return TargetToDiff(&ms.bigDiff)
}

func (ms *MiningState) SetStratumDiff(diffValue float64) float64 {
	ms.StateLock.Lock()
	defer ms.StateLock.Unlock()
	if ms.stratumDiff == nil {
		ms.stratumDiff = newCryptixDiff()
	}
	previous := ms.stratumDiff.diffValue
	ms.stratumDiff.setDiffValue(diffValue)
	return previous
}

func (ms *MiningState) GetStratumDiffValue() float64 {
	ms.StateLock.RLock()
	defer ms.StateLock.RUnlock()
	if ms.stratumDiff == nil {
		return 0
	}
	return ms.stratumDiff.diffValue
}

func (ms *MiningState) GetUseBigJob() bool {
	ms.StateLock.RLock()
	defer ms.StateLock.RUnlock()
	return ms.useBigJob
}

func (ms *MiningState) GetStratumDiffSnapshot() (float64, *big.Int, float64) {
	ms.StateLock.RLock()
	defer ms.StateLock.RUnlock()
	if ms.stratumDiff == nil {
		return 0, nil, 0
	}

	target := new(big.Int).Set(ms.stratumDiff.targetValue)
	return ms.stratumDiff.hashValue, target, ms.stratumDiff.diffValue
}
