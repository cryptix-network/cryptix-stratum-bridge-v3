package cryptixstratum

import (
	"fmt"
	"log"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	stdatomic "sync/atomic"
	"time"

	"github.com/cryptix-network/cryptix-stratum-bridge-v3/src/gostratum"
	"github.com/cryptix-network/cryptixd/app/appmessage"
	"github.com/cryptix-network/cryptixd/domain/consensus/model/externalapi"
	"github.com/cryptix-network/cryptixd/domain/consensus/utils/consensushashing"
	"github.com/cryptix-network/cryptixd/domain/consensus/utils/pow"
	"github.com/cryptix-network/cryptixd/infrastructure/network/rpcclient"
	"github.com/pkg/errors"
	"go.uber.org/atomic"
	"go.uber.org/zap"
)

const defaultVarDiffRetargetInterval = 10 * time.Second

type WorkStats struct {
	BlocksFound        atomic.Int64
	SharesFound        atomic.Int64
	SharesDiff         atomic.Float64
	StaleShares        atomic.Int64
	InvalidShares      atomic.Int64
	WorkerName         string
	StartTime          time.Time
	LastShare          time.Time
	VarDiffStartTime   time.Time
	VarDiffSharesFound atomic.Int64
	VarDiffWindow      int
	MinDiff            atomic.Float64
}

type shareHandler struct {
	cryptix      *rpcclient.RPCClient
	soloDiffBits stdatomic.Uint64
	stats        map[string]*WorkStats
	statsLock    sync.RWMutex
	overall      WorkStats
	tipBlueScore stdatomic.Uint64
	dupeLock     sync.Mutex
	dupeShares   map[string]*shareHistory
}

type shareHistory struct {
	order []string
	set   map[string]struct{}
}

func newShareHandler(cryptix *rpcclient.RPCClient) *shareHandler {
	return &shareHandler{
		cryptix:    cryptix,
		stats:      map[string]*WorkStats{},
		statsLock:  sync.RWMutex{},
		dupeShares: map[string]*shareHistory{},
	}
}

func (sh *shareHandler) getCreateStats(ctx *gostratum.StratumContext) *WorkStats {
	sh.statsLock.Lock()
	defer sh.statsLock.Unlock()

	key := workerStatsKey(ctx)
	displayName := workerStatsDisplayName(ctx)
	stats, found := sh.stats[key]
	if !found && key != ctx.RemoteAddr {
		// Stats may have been created before authorize, under remote address.
		if migrated, exists := sh.stats[ctx.RemoteAddr]; exists {
			stats = migrated
			found = true
			delete(sh.stats, ctx.RemoteAddr)
			sh.stats[key] = stats
		}
	}

	if !found {
		stats = &WorkStats{}
		now := time.Now()
		stats.LastShare = now
		stats.WorkerName = displayName
		stats.StartTime = now
		sh.stats[key] = stats

		// TODO: not sure this is the best place, nor whether we shouldn't be
		// resetting on disconnect
		InitWorkerCounters(ctx)
	} else if stats.WorkerName != displayName {
		stats.WorkerName = displayName
	}
	return stats
}

type submitInfo struct {
	block    *appmessage.RPCBlock
	state    *MiningState
	jobID    int
	noncestr string
	nonceVal uint64
}

func validateSubmit(ctx *gostratum.StratumContext, event gostratum.JsonRpcEvent) (*submitInfo, error) {
	if len(event.Params) < 3 {
		RecordWorkerError(ctx.WalletAddr, ErrBadDataFromMiner)
		return nil, fmt.Errorf("malformed event, expected at least 2 params")
	}
	jobIdStr, ok := event.Params[1].(string)
	if !ok {
		RecordWorkerError(ctx.WalletAddr, ErrBadDataFromMiner)
		return nil, fmt.Errorf("unexpected type for param 1: %+v", event.Params...)
	}
	jobId, err := strconv.ParseInt(jobIdStr, 10, 0)
	if err != nil {
		RecordWorkerError(ctx.WalletAddr, ErrBadDataFromMiner)
		return nil, errors.Wrap(err, "job id is not parsable as an number")
	}
	state := GetMiningState(ctx)
	block, exists := state.GetJob(int(jobId))
	if !exists {
		RecordWorkerError(ctx.WalletAddr, ErrMissingJob)
		return nil, errors.Wrap(ErrStaleShare, "job does not exist")
	}
	noncestr, ok := event.Params[2].(string)
	if !ok {
		RecordWorkerError(ctx.WalletAddr, ErrBadDataFromMiner)
		return nil, fmt.Errorf("unexpected type for param 2: %+v", event.Params...)
	}
	return &submitInfo{
		state:    state,
		block:    block,
		jobID:    int(jobId),
		noncestr: strings.Replace(noncestr, "0x", "", 1),
	}, nil
}

var (
	ErrStaleShare = fmt.Errorf("stale share")
	ErrDupeShare  = fmt.Errorf("duplicate share")
)

type SubmitStatus int

const (
	SubmitStatusAccepted SubmitStatus = iota
	SubmitStatusStale
	SubmitStatusDuplicate
	SubmitStatusLowDiff
	SubmitStatusBad
)

// the max difference between tip blue score and job blue score that we'll accept
// anything greater than this is considered a stale
const workWindow = 8
const maxTrackedSharesPerWorker = 2048

func workerStatsKey(ctx *gostratum.StratumContext) string {
	if ctx.WorkerName != "" && ctx.WorkerName != gostratum.AnonymousWorkerName {
		return ctx.WorkerName
	}
	if ctx.WalletAddr != "" {
		return ctx.WalletAddr + "#" + gostratum.AnonymousWorkerName
	}
	return ctx.RemoteAddr
}

func workerStatsDisplayName(ctx *gostratum.StratumContext) string {
	if ctx.WorkerName == "" {
		return gostratum.AnonymousWorkerName
	}
	return ctx.WorkerName
}

func shareKey(si *submitInfo) string {
	return fmt.Sprintf("%d:%s", si.jobID, si.noncestr)
}

func (sh *shareHandler) trackDupeShare(ctx *gostratum.StratumContext, si *submitInfo) bool {
	worker := workerStatsKey(ctx)
	key := shareKey(si)

	sh.dupeLock.Lock()
	defer sh.dupeLock.Unlock()

	history, ok := sh.dupeShares[worker]
	if !ok {
		history = &shareHistory{
			order: make([]string, 0, maxTrackedSharesPerWorker),
			set:   make(map[string]struct{}, maxTrackedSharesPerWorker),
		}
		sh.dupeShares[worker] = history
	}

	if _, exists := history.set[key]; exists {
		return true
	}

	history.set[key] = struct{}{}
	history.order = append(history.order, key)
	if len(history.order) > maxTrackedSharesPerWorker {
		evicted := history.order[0]
		history.order = history.order[1:]
		delete(history.set, evicted)
	}

	return false
}

func (sh *shareHandler) checkStales(ctx *gostratum.StratumContext, si *submitInfo) error {
	shareBlueScore := si.block.Header.BlueScore
	for {
		tip := sh.tipBlueScore.Load()
		if shareBlueScore > tip {
			if sh.tipBlueScore.CompareAndSwap(tip, shareBlueScore) {
				break
			}
			continue
		}
		if tip-shareBlueScore > workWindow {
			return errors.Wrapf(ErrStaleShare, "blueScore %d vs %d", shareBlueScore, tip)
		}
		break
	}

	if sh.trackDupeShare(ctx, si) {
		return ErrDupeShare
	}

	return nil
}

func (sh *shareHandler) setSoloDiff(diff float64) {
	sh.soloDiffBits.Store(math.Float64bits(diff))
}

func (sh *shareHandler) getSoloDiff() float64 {
	return math.Float64frombits(sh.soloDiffBits.Load())
}

func (sh *shareHandler) clearClientDupeHistory(ctx *gostratum.StratumContext) {
	sh.dupeLock.Lock()
	defer sh.dupeLock.Unlock()
	delete(sh.dupeShares, ctx.RemoteAddr)
	delete(sh.dupeShares, ctx.WorkerName)
	delete(sh.dupeShares, workerStatsKey(ctx))
}

func (sh *shareHandler) HandleSubmit(ctx *gostratum.StratumContext, event gostratum.JsonRpcEvent, soloMining bool) error {
	submitInfo, err := validateSubmit(ctx, event)
	if err != nil {
		if errors.Is(err, ErrStaleShare) {
			stats := sh.getCreateStats(ctx)
			stats.StaleShares.Add(1)
			sh.overall.StaleShares.Add(1)
			RecordStaleShare(ctx)
			return sh.replySubmitStatusV1(ctx, event.Id, SubmitStatusStale)
		}
		ctx.Logger.Warn("malformed submit payload", zap.Error(err))
		return sh.replySubmitStatusV1(ctx, event.Id, SubmitStatusBad)
	}

	submitInfo.nonceVal, err = parseV1SubmitNonce(ctx, submitInfo.noncestr)
	if err != nil {
		RecordWorkerError(ctx.WalletAddr, ErrBadDataFromMiner)
		ctx.Logger.Warn("invalid submit nonce", zap.Error(err))
		return sh.replySubmitStatusV1(ctx, event.Id, SubmitStatusBad)
	}
	submitInfo.noncestr = fmt.Sprintf("%016x", submitInfo.nonceVal)

	status, err := sh.processSubmit(ctx, submitInfo, soloMining)
	if err != nil {
		return err
	}
	return sh.replySubmitStatusV1(ctx, event.Id, status)
}

func (sh *shareHandler) HandleSubmitSV2(ctx *gostratum.StratumContext, jobID uint32, nonce uint64, soloMining bool) (SubmitStatus, error) {
	state := GetMiningState(ctx)
	block, exists := state.GetJob(int(jobID))
	if !exists {
		RecordWorkerError(ctx.WalletAddr, ErrMissingJob)
		stats := sh.getCreateStats(ctx)
		stats.StaleShares.Add(1)
		sh.overall.StaleShares.Add(1)
		RecordStaleShare(ctx)
		return SubmitStatusStale, nil
	}

	submitInfo := &submitInfo{
		block:    block,
		state:    state,
		jobID:    int(jobID),
		noncestr: fmt.Sprintf("%016x", nonce),
		nonceVal: nonce,
	}
	return sh.processSubmit(ctx, submitInfo, soloMining)
}

func parseV1SubmitNonce(ctx *gostratum.StratumContext, nonceStr string) (uint64, error) {
	normalized := nonceStr

	// add extranonce to submitted nonce if the miner only sent extranonce2
	if ctx.Extranonce != "" {
		extranonce2Len := 16 - len(ctx.Extranonce)
		if extranonce2Len > 0 && len(normalized) <= extranonce2Len {
			normalized = ctx.Extranonce + fmt.Sprintf("%0*s", extranonce2Len, normalized)
		}
	}

	nonceVal, err := strconv.ParseUint(normalized, 16, 64)
	if err != nil {
		return 0, errors.Wrap(err, "failed parsing noncestr")
	}
	return nonceVal, nil
}

func (sh *shareHandler) replySubmitStatusV1(ctx *gostratum.StratumContext, eventID any, status SubmitStatus) error {
	switch status {
	case SubmitStatusAccepted:
		return ctx.Reply(gostratum.JsonRpcResponse{
			Id:     eventID,
			Result: true,
		})
	case SubmitStatusStale:
		return ctx.ReplyStaleShare(eventID)
	case SubmitStatusDuplicate:
		return ctx.ReplyDupeShare(eventID)
	case SubmitStatusLowDiff:
		return ctx.ReplyLowDiffShare(eventID)
	default:
		return ctx.ReplyBadShare(eventID)
	}
}

func (sh *shareHandler) processSubmit(ctx *gostratum.StratumContext, submitInfo *submitInfo, soloMining bool) (SubmitStatus, error) {
	stats := sh.getCreateStats(ctx)
	if err := sh.checkStales(ctx, submitInfo); err != nil {
		if errors.Is(err, ErrDupeShare) {
			ctx.Logger.Warn("duplicate share: " + submitInfo.noncestr)
			RecordDupeShare(ctx)
			stats.InvalidShares.Add(1)
			sh.overall.InvalidShares.Add(1)
			return SubmitStatusDuplicate, nil
		}
		if errors.Is(err, ErrStaleShare) {
			ctx.Logger.Warn("stale share")
			stats.StaleShares.Add(1)
			sh.overall.StaleShares.Add(1)
			RecordStaleShare(ctx)
			return SubmitStatusStale, nil
		}
		ctx.Logger.Error("unknown error during stale/duplicate checks", zap.Error(err))
		return SubmitStatusBad, nil
	}

	converted, err := appmessage.RPCBlockToDomainBlock(submitInfo.block)
	if err != nil {
		return SubmitStatusBad, fmt.Errorf("failed to cast block to mutable block: %+v", err)
	}
	mutableHeader := converted.Header.ToMutable()
	mutableHeader.SetNonce(submitInfo.nonceVal)

	powState := pow.NewState(mutableHeader)
	powValue := powState.CalculateProofOfWorkValue()
	shareHashValue, shareTarget, _ := submitInfo.state.GetStratumDiffSnapshot()
	if shareTarget == nil {
		RecordWorkerError(ctx.WalletAddr, ErrMissingJob)
		return SubmitStatusBad, fmt.Errorf("mining state is not initialized for client")
	}

	// The block hash must be less or equal than the claimed target.
	if powValue.Cmp(&powState.Target) <= 0 {
		status, err := sh.submitBlock(ctx, converted, submitInfo.nonceVal)
		if err != nil {
			return status, err
		}
		if status != SubmitStatusAccepted {
			return status, nil
		}
	} else if powValue.Cmp(shareTarget) >= 0 {
		if soloMining {
			ctx.Logger.Warn("weak block")
		} else {
			ctx.Logger.Warn("weak share")
		}
		stats.InvalidShares.Add(1)
		sh.overall.InvalidShares.Add(1)
		RecordWeakShare(ctx)
		return SubmitStatusLowDiff, nil
	}

	stats.SharesFound.Add(1)
	stats.VarDiffSharesFound.Add(1)
	stats.SharesDiff.Add(shareHashValue)
	stats.LastShare = time.Now()
	sh.overall.SharesFound.Add(1)
	RecordShareFound(ctx, shareHashValue)
	return SubmitStatusAccepted, nil
}

func (sh *shareHandler) submitBlock(ctx *gostratum.StratumContext, block *externalapi.DomainBlock, nonce uint64) (SubmitStatus, error) {
	mutable := block.Header.ToMutable()
	mutable.SetNonce(nonce)
	block = &externalapi.DomainBlock{
		Header:       mutable.ToImmutable(),
		Transactions: block.Transactions,
	}
	_, err := sh.cryptix.SubmitBlock(block)
	blockhash := consensushashing.BlockHash(block)
	// print after the submit to get it submitted faster
	ctx.Logger.Info(fmt.Sprintf("Submitted block %s", blockhash))

	if err != nil {
		// :'(
		if strings.Contains(err.Error(), "ErrDuplicateBlock") {
			ctx.Logger.Warn("block rejected, stale")
			stats := sh.getCreateStats(ctx)
			stats.StaleShares.Add(1)
			sh.overall.StaleShares.Add(1)
			RecordStaleShare(ctx)
			return SubmitStatusStale, nil
		}
		ctx.Logger.Warn("block rejected, unknown issue (probably bad pow)", zap.Error(err))
		stats := sh.getCreateStats(ctx)
		stats.InvalidShares.Add(1)
		sh.overall.InvalidShares.Add(1)
		RecordInvalidShare(ctx)
		return SubmitStatusBad, nil
	}

	// :)
	ctx.Logger.Info(fmt.Sprintf("block accepted %s", blockhash))
	stats := sh.getCreateStats(ctx)
	stats.BlocksFound.Add(1)
	sh.overall.BlocksFound.Add(1)
	RecordBlockFound(ctx, block.Header.Nonce(), block.Header.BlueScore())

	return SubmitStatusAccepted, nil
}

func (sh *shareHandler) startStatsThread() error {
	start := time.Now()
	for {
		// console formatting is terrible. Good luck whever touches anything
		time.Sleep(10 * time.Second)
		sh.statsLock.RLock()
		str := "\n===============================================================================\n"
		str += "  worker name   |  avg hashrate  |   acc/stl/inv  |    blocks    |    uptime   \n"
		str += "-------------------------------------------------------------------------------\n"
		var lines []string
		totalRate := float64(0)
		for _, v := range sh.stats {
			rate := GetAverageHashrateGHs(v)
			totalRate += rate
			rateStr := stringifyHashrate(rate)
			ratioStr := fmt.Sprintf("%d/%d/%d", v.SharesFound.Load(), v.StaleShares.Load(), v.InvalidShares.Load())
			lines = append(lines, fmt.Sprintf(" %-15s| %14.14s | %14.14s | %12d | %11s",
				v.WorkerName, rateStr, ratioStr, v.BlocksFound.Load(), time.Since(v.StartTime).Round(time.Second)))
		}
		sort.Strings(lines)
		str += strings.Join(lines, "\n")
		rateStr := stringifyHashrate(totalRate)
		ratioStr := fmt.Sprintf("%d/%d/%d", sh.overall.SharesFound.Load(), sh.overall.StaleShares.Load(), sh.overall.InvalidShares.Load())
		str += "\n-------------------------------------------------------------------------------\n"
		str += fmt.Sprintf("                | %14.14s | %14.14s | %12d | %11s",
			rateStr, ratioStr, sh.overall.BlocksFound.Load(), time.Since(start).Round(time.Second))
		str += "\n-------------------------------------------------------------------------------\n"
		str += " Est. Network Hashrate: " + stringifyHashrate(DiffToHash(sh.getSoloDiff()))
		str += "\n======================================================== cryptix_bridge_" + version + " ===\n"
		sh.statsLock.RUnlock()
		log.Println(str)
	}
}

func GetAverageHashrateGHs(stats *WorkStats) float64 {
	elapsed := time.Since(stats.StartTime).Seconds()
	if elapsed <= 0 {
		return 0
	}
	return stats.SharesDiff.Load() / elapsed
}

func stringifyHashrate(ghs float64) string {
	unitStrings := [...]string{"", "K", "M", "G", "T", "P", "E", "Z", "Y"}
	hr := ghs * 1_000_000_000 // convert from GH/s to H/s
	unitIdx := 0
	for hr >= 1000 && unitIdx < len(unitStrings)-1 {
		hr /= 1000
		unitIdx++
	}
	return fmt.Sprintf("%0.2f%sH/s", hr, unitStrings[unitIdx])
}

func (sh *shareHandler) startVardiffThread(expectedShareRate uint, logStats bool, retargetInterval time.Duration) error {
	// 15 shares/min allows a ~95% confidence assumption of:
	//   < 100% variation after 1m
	//   < 50% variation after 3m
	//   < 25% variation after 10m
	//   < 15% variation after 30m
	//   < 10% variation after 1h
	//   < 5% variation after 4h
	var windows = [...]uint{1, 3, 10, 30, 60, 240, 0}
	var tolerances = [...]float64{0.5, 0.5, 0.25, 0.15, 0.1, 0.05, 0.05}
	if expectedShareRate == 0 {
		expectedShareRate = 1
	}
	if retargetInterval <= 0 {
		retargetInterval = defaultVarDiffRetargetInterval
	}
	if retargetInterval < time.Second {
		retargetInterval = time.Second
	}

	for {
		time.Sleep(retargetInterval)
		sh.statsLock.Lock()

		stats := "\n=== vardiff ===================================================================\n\n"
		stats += "  worker name  |    diff     |  window  |  elapsed   |    shares   |   rate    \n"
		stats += "-------------------------------------------------------------------------------\n"

		var statsLines []string
		var toleranceErrs []string

		for _, v := range sh.stats {
			if v.VarDiffStartTime.IsZero() {
				// no vardiff sent to client
				continue
			}

			worker := v.WorkerName
			diff := v.MinDiff.Load()
			shares := v.VarDiffSharesFound.Load()
			duration := time.Since(v.VarDiffStartTime).Minutes()
			if duration <= 0 {
				continue
			}
			shareRate := float64(shares) / duration
			shareRateRatio := shareRate / float64(expectedShareRate)
			window := windows[v.VarDiffWindow]
			tolerance := tolerances[v.VarDiffWindow]

			statsLines = append(statsLines, fmt.Sprintf(" %-14s| %11.2f | %8d | %10.2f | %11d | %9.2f", worker, diff, window, duration, shares, shareRate))

			// check final stage first, as this is where majority of time spent
			if window == 0 {
				if math.Abs(1-shareRateRatio) >= tolerance {
					// final stage submission rate OOB
					toleranceErrs = append(toleranceErrs, fmt.Sprintf("%s final share rate (%f) exceeded tolerance (+/- %d%%)", worker, shareRate, int(tolerance*100)))
					updateVarDiff(v, diff*shareRateRatio)
				}
				continue
			}

			// check all previously cleared windows
			i := 1
			for i < v.VarDiffWindow {
				if math.Abs(1-shareRateRatio) >= tolerances[i] {
					// breached tolerance of previously cleared window
					toleranceErrs = append(toleranceErrs, fmt.Sprintf("%s share rate (%f) exceeded tolerance (+/- %d%%) for %dm window", worker, shareRate, int(tolerances[i]*100), windows[i]))
					updateVarDiff(v, diff*shareRateRatio)
					break
				}
				i++
			}
			if i < v.VarDiffWindow {
				// should only happen if we broke previous loop
				continue
			}

			// check for current window max exception
			if float64(shares) >= float64(window*expectedShareRate)*(1+tolerance) {
				// submission rate > window max
				toleranceErrs = append(toleranceErrs, fmt.Sprintf("%s share rate (%f) exceeded upper tolerance (+ %d%%) for %dm window", worker, shareRate, int(tolerance*100), window))
				updateVarDiff(v, diff*shareRateRatio)
				continue
			}

			// check whether we've exceeded window length
			if duration >= float64(window) {
				// check for current window min exception
				if float64(shares) <= float64(window*expectedShareRate)*(1-tolerance) {
					// submission rate < window min
					toleranceErrs = append(toleranceErrs, fmt.Sprintf("%s share rate (%f) exceeded lower tolerance (- %d%%) for %dm window", worker, shareRate, int(tolerance*100), window))
					updateVarDiff(v, diff*math.Max(shareRateRatio, 0.1))
					continue
				}

				v.VarDiffWindow++
			}
		}
		sort.Strings(statsLines)
		stats += strings.Join(statsLines, "\n")
		stats += "\n\n======================================================== cryptix_bridge_" + version + " ===\n"
		stats += strings.Join(toleranceErrs, "\n")
		if logStats {
			log.Println(stats)
		}
		sh.statsLock.Unlock()
	}
}

// update vardiff with new mindiff, reset counters, and disable tracker until
// client handler restarts it while sending diff on next block
func updateVarDiff(stats *WorkStats, minDiff float64) float64 {
	stats.VarDiffStartTime = time.Time{}
	stats.VarDiffWindow = 0
	previousMinDiff := stats.MinDiff.Load()
	stats.MinDiff.Store(minDiff)
	return previousMinDiff
}

// (re)start vardiff tracker
func startVarDiff(stats *WorkStats) {
	if stats.VarDiffStartTime.IsZero() {
		stats.VarDiffSharesFound.Store(0)
		stats.VarDiffStartTime = time.Now()
	}
}

func (sh *shareHandler) startClientVardiff(ctx *gostratum.StratumContext) {
	stats := sh.getCreateStats(ctx)
	sh.statsLock.Lock()
	defer sh.statsLock.Unlock()
	startVarDiff(stats)
}

func (sh *shareHandler) getClientVardiff(ctx *gostratum.StratumContext) float64 {
	stats := sh.getCreateStats(ctx)
	sh.statsLock.RLock()
	defer sh.statsLock.RUnlock()
	return stats.MinDiff.Load()
}

func (sh *shareHandler) setClientVardiff(ctx *gostratum.StratumContext, minDiff float64) float64 {
	stats := sh.getCreateStats(ctx)
	sh.statsLock.Lock()
	defer sh.statsLock.Unlock()
	previousMinDiff := updateVarDiff(stats, math.Max(minDiff, 0.00001))
	startVarDiff(stats)
	return previousMinDiff
}
