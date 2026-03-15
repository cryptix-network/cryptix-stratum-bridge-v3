package cryptixstratum

import (
	"fmt"
	"math"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cryptix-network/cryptix-stratum-bridge-v3/src/gostratum"
	"github.com/pkg/errors"
	"go.uber.org/zap"
)

var bigJobRegex = regexp.MustCompile(".*BzMiner.*")

const balanceDelay = time.Minute

type clientListener struct {
	logger           *zap.SugaredLogger
	shareHandler     *shareHandler
	clientLock       sync.RWMutex
	clients          map[int32]*gostratum.StratumContext
	lastBalanceCheck time.Time
	clientCounter    int32
	minShareDiff     float64
	extranonceSize   int8
	maxExtranonce    int32
	nextExtranonce   int32
}

func newClientListener(logger *zap.SugaredLogger, shareHandler *shareHandler, minShareDiff float64, extranonceSize int8) *clientListener {
	return &clientListener{
		logger:         logger,
		minShareDiff:   minShareDiff,
		extranonceSize: extranonceSize,
		maxExtranonce:  int32(math.Pow(2, (8*math.Min(float64(extranonceSize), 3))) - 1),
		nextExtranonce: 0,
		clientLock:     sync.RWMutex{},
		shareHandler:   shareHandler,
		clients:        make(map[int32]*gostratum.StratumContext),
	}
}

func (c *clientListener) OnConnect(ctx *gostratum.StratumContext) {
	var extranonce int32

	idx := atomic.AddInt32(&c.clientCounter, 1)
	ctx.Id = idx
	c.clientLock.Lock()
	if c.extranonceSize > 0 {
		extranonce = c.nextExtranonce
		if c.nextExtranonce < c.maxExtranonce {
			c.nextExtranonce++
		} else {
			c.nextExtranonce = 0
			c.logger.Warn("wrapped extranonce! new clients may be duplicating work...")
		}
	}
	c.clients[idx] = ctx
	c.clientLock.Unlock()
	ctx.Logger = ctx.Logger.With(zap.Int("client_id", int(ctx.Id)))

	if c.extranonceSize > 0 {
		ctx.Extranonce = fmt.Sprintf("%0*x", c.extranonceSize*2, extranonce)
	}
	go func() {

		time.Sleep(5 * time.Second)
		c.shareHandler.getCreateStats(ctx)
	}()
}

func (c *clientListener) OnDisconnect(ctx *gostratum.StratumContext) {
	c.clientLock.Lock()
	c.logger.Info("removing client ", ctx.Id)
	delete(c.clients, ctx.Id)
	c.logger.Info("removed client ", ctx.Id)
	c.clientLock.Unlock()
	c.shareHandler.clearClientDupeHistory(ctx)
	RecordDisconnect(ctx)
}

func (c *clientListener) NewBlockAvailable(kapi *cryptixApi, soloMining bool) {
	c.clientLock.RLock()
	clients := make([]*gostratum.StratumContext, 0, len(c.clients))
	addresses := make([]string, 0, len(c.clients))

	for _, cl := range c.clients {
		if !cl.Connected() {
			continue
		}
		if cl.WalletAddr == "" {
			// Wait until the miner has authorized before assigning work.
			continue
		}
		clients = append(clients, cl)
		addresses = append(addresses, cl.WalletAddr)
	}
	c.clientLock.RUnlock()

	for _, cl := range clients {
		go func(client *gostratum.StratumContext) {
			state := GetMiningState(client)
			originalAddr := client.WalletAddr
			c.shareHandler.getCreateStats(client)

			template, err := kapi.GetBlockTemplate(client)
			if err != nil {
				if strings.Contains(err.Error(), "Could not decode address") {
					RecordWorkerError(client.WalletAddr, ErrInvalidAddressFmt)
					client.Logger.Error(fmt.Sprintf("failed fetching new block template from cryptix, malformed address: %s", err))
					client.Disconnect()
				} else {
					RecordWorkerError(client.WalletAddr, ErrFailedBlockFetch)
					client.Logger.Error(fmt.Sprintf("failed fetching new block template from cryptix: %s", err))
				}
				return
			}

			netDiff := state.SetNetworkTarget(uint64(template.Block.Header.Bits))
			header, err := SerializeBlockHeader(template.Block)
			if err != nil {
				RecordWorkerError(client.WalletAddr, ErrBadDataFromMiner)
				client.Logger.Error(fmt.Sprintf("failed to serialize block header: %s", err))
				return
			}

			initialized := state.InitializeIfNeeded(client.RemoteApp, c.minShareDiff)
			if initialized {
				c.shareHandler.setClientVardiff(client, c.minShareDiff)
			}
			sendInitialDiff := initialized && !soloMining && !client.IsSV2()
			if sendInitialDiff {
				sendClientDiff(client, c.minShareDiff)
			}

			varDiff := netDiff
			c.shareHandler.setSoloDiff(netDiff)
			if !soloMining {
				varDiff = c.shareHandler.getClientVardiff(client)
			}

			currentDiff := state.GetStratumDiffValue()
			if varDiff == 0 {
				varDiff = currentDiff
			}
			if varDiff != currentDiff {
				if !soloMining {
					client.Logger.Info(fmt.Sprintf("changing diff from %.10f to %.10f", currentDiff, varDiff))
				}
				state.SetStratumDiff(varDiff)
				if client.IsSV2() {
					if err := sendSV2Target(client, varDiff); err != nil {
						if errors.Is(err, gostratum.ErrorDisconnected) {
							RecordWorkerError(client.WalletAddr, ErrDisconnected)
							return
						}
						RecordWorkerError(client.WalletAddr, ErrFailedSetDiff)
						client.Logger.Error(errors.Wrap(err, "failed sending sv2 difficulty target").Error())
						client.Disconnect()
						return
					}
				} else {
					sendClientDiff(client, varDiff)
				}
				c.shareHandler.startClientVardiff(client)
			} else if client.IsSV2() {
				if err := sendSV2Target(client, varDiff); err != nil {
					if errors.Is(err, gostratum.ErrorDisconnected) {
						RecordWorkerError(client.WalletAddr, ErrDisconnected)
						return
					}
					RecordWorkerError(client.WalletAddr, ErrFailedSetDiff)
					client.Logger.Error(errors.Wrap(err, "failed sending sv2 difficulty target").Error())
					client.Disconnect()
					return
				}
			}

			jobId := state.AddJob(template.Block)
			if client.IsSV2() {
				if err := sendSV2Job(client, uint32(jobId), header, uint32(template.Block.Header.Version), uint64(template.Block.Header.Timestamp), uint32(template.Block.Header.Bits)); err != nil {
					if errors.Is(err, gostratum.ErrorDisconnected) {
						RecordWorkerError(client.WalletAddr, ErrDisconnected)
						return
					}
					RecordWorkerError(client.WalletAddr, ErrFailedSendWork)
					client.Logger.Error(errors.Wrapf(err, "failed sending sv2 work packet %d", jobId).Error())
					client.Disconnect()
					return
				}
				RecordNewJob(client)
				client.WalletAddr = originalAddr
				return
			}

			jobParams := []any{fmt.Sprintf("%d", jobId)}
			if state.GetUseBigJob() {
				jobParams = append(jobParams, GenerateLargeJobParams(header, uint64(template.Block.Header.Timestamp)))
			} else {
				jobParams = append(jobParams, GenerateJobHeader(header))
				jobParams = append(jobParams, uint64(template.Block.Header.Timestamp))
			}

			if err := client.Send(gostratum.JsonRpcEvent{
				Version: "2.0",
				Method:  "mining.notify",
				Id:      jobId,
				Params:  jobParams,
			}); err != nil {
				if errors.Is(err, gostratum.ErrorDisconnected) {
					RecordWorkerError(client.WalletAddr, ErrDisconnected)
					return
				}
				RecordWorkerError(client.WalletAddr, ErrFailedSendWork)
				client.Logger.Error(errors.Wrapf(err, "failed sending work packet %d", jobId).Error())
			}

			RecordNewJob(client)
			client.WalletAddr = originalAddr
		}(cl)
	}

	c.clientLock.Lock()
	shouldCheckBalances := time.Since(c.lastBalanceCheck) > balanceDelay
	if shouldCheckBalances {
		c.lastBalanceCheck = time.Now()
	}
	c.clientLock.Unlock()

	if shouldCheckBalances && len(addresses) > 0 {
		go func() {
			balances, err := kapi.cryptix.GetBalancesByAddresses(addresses)
			if err != nil {
				c.logger.Warn("failed to get balances from cryptix, prom stats will be out of date", zap.Error(err))
				return
			}
			RecordBalances(balances)
		}()
	}
}

func sendClientDiff(client *gostratum.StratumContext, diff float64) {
	if err := client.Send(gostratum.JsonRpcEvent{
		Version: "2.0",
		Method:  "mining.set_difficulty",
		Params:  []any{diff},
	}); err != nil {
		RecordWorkerError(client.WalletAddr, ErrFailedSetDiff)
		client.Logger.Error(errors.Wrap(err, "failed sending difficulty").Error(), zap.Any("context", client))
		return
	}
}

func sendSV2Target(client *gostratum.StratumContext, diff float64) error {
	target := diffToSV2TargetLE(diff)
	return writeSV2Frame(client, sv2ExtChannel, sv2MsgSetTarget, encodeSV2SetTargetPayload(client.SV2ChannelID(), target))
}

func sendSV2Job(client *gostratum.StratumContext, jobID uint32, prePowHash []byte, version uint32, timestampMs uint64, nBits uint32) error {
	prePowHash32, err := bytesToFixed32(prePowHash)
	if err != nil {
		return err
	}

	if err := writeSV2Frame(client, sv2ExtChannel, sv2MsgNewMiningJob, encodeSV2NewMiningJobPayload(client.SV2ChannelID(), jobID, version, prePowHash32)); err != nil {
		return err
	}

	// cryptis-miner resolves all-zero prevhash using the remembered NewMiningJob pre-pow hash.
	var zeroPrevHash [32]byte
	minNTime := uint32(timestampMs / 1000)
	return writeSV2Frame(client, sv2ExtChannel, sv2MsgSetNewPrevHash, encodeSV2SetNewPrevHashPayload(client.SV2ChannelID(), jobID, zeroPrevHash, minNTime, nBits))
}
