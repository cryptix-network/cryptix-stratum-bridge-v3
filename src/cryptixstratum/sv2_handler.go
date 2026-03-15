package cryptixstratum

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync/atomic"

	"github.com/cryptix-network/cryptix-stratum-bridge-v3/src/gostratum"
	"github.com/pkg/errors"
	"go.uber.org/zap"
)

type sv2Handler struct {
	logger              *zap.SugaredLogger
	shareHandler        *shareHandler
	minShareDiff        float64
	soloMining          bool
	strictProtocolCheck bool
	nextChannelID       atomic.Uint32
}

func newSV2Handler(logger *zap.SugaredLogger, shareHandler *shareHandler, minShareDiff float64, soloMining bool, strictProtocolCheck bool) *sv2Handler {
	h := &sv2Handler{
		logger:              logger,
		shareHandler:        shareHandler,
		minShareDiff:        minShareDiff,
		soloMining:          soloMining,
		strictProtocolCheck: strictProtocolCheck,
	}
	h.nextChannelID.Store(0)
	return h
}

func (h *sv2Handler) CanHandleFirstByte(firstByte byte) bool {
	if h.strictProtocolCheck {
		return true
	}
	// First frame from miners uses extension 0x0000, so first byte is 0x00.
	return firstByte == 0x00
}

func (h *sv2Handler) HandleBinaryClient(ctx *gostratum.StratumContext, connection net.Conn) error {
	defer ctx.Disconnect()
	setupComplete := false

	for {
		frame, err := readSV2Frame(connection)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return gostratum.ErrorDisconnected
			}
			return errors.Wrap(err, "failed reading sv2 frame")
		}

		switch frame.MessageType {
		case sv2MsgSetupConnection:
			remoteApp, err := parseSV2SetupConnectionPayload(frame.Payload)
			if err != nil {
				_ = writeSV2Frame(ctx, sv2ExtStandard, sv2MsgSetupConnectionError, encodeSV2SetupConnectionErrorPayload(0, "malformed setup connection"))
				return errors.Wrap(err, "invalid setup connection payload")
			}
			if remoteApp != "" {
				ctx.RemoteApp = remoteApp
			}
			if err := writeSV2Frame(ctx, sv2ExtStandard, sv2MsgSetupConnectionSuccess, encodeSV2SetupConnectionSuccessPayload(2, 0)); err != nil {
				return err
			}
			setupComplete = true

		case sv2MsgOpenStandardMiningChannel:
			requestID, identity, err := parseSV2OpenStandardChannelPayload(frame.Payload)
			if err != nil {
				_ = writeSV2Frame(ctx, sv2ExtStandard, sv2MsgOpenMiningChannelError, encodeSV2OpenChannelErrorPayload(readSV2RequestID(frame.Payload), "malformed open channel"))
				return errors.Wrap(err, "invalid open channel payload")
			}
			if !setupComplete {
				if err := writeSV2Frame(ctx, sv2ExtStandard, sv2MsgOpenMiningChannelError, encodeSV2OpenChannelErrorPayload(requestID, "setup not complete")); err != nil {
					return err
				}
				return fmt.Errorf("received open channel before setup")
			}

			walletAddr, workerName, err := gostratum.ParseAuthorizeIdentity(identity)
			if err != nil {
				RecordWorkerError(ctx.WalletAddr, ErrInvalidAddressFmt)
				if err := writeSV2Frame(ctx, sv2ExtStandard, sv2MsgOpenMiningChannelError, encodeSV2OpenChannelErrorPayload(requestID, "invalid wallet")); err != nil {
					return err
				}
				return errors.Wrap(err, "sv2 open channel rejected due to invalid wallet")
			}

			ctx.WalletAddr = walletAddr
			ctx.WorkerName = workerName
			ctx.Logger = ctx.Logger.With(zap.String("worker", ctx.WorkerName), zap.String("addr", ctx.WalletAddr))

			channelID := h.nextChannelID.Add(1)
			if channelID == 0 {
				channelID = h.nextChannelID.Add(1)
			}
			ctx.EnableSV2(channelID)

			state := GetMiningState(ctx)
			state.InitializeIfNeeded(ctx.RemoteApp, h.minShareDiff)
			h.shareHandler.setClientVardiff(ctx, h.minShareDiff)
			diff := state.GetStratumDiffValue()
			if diff <= 0 {
				diff = h.minShareDiff
				state.SetStratumDiff(diff)
			}
			target := diffToSV2TargetLE(diff)
			if err := writeSV2Frame(ctx, sv2ExtStandard, sv2MsgOpenMiningChannelSuccess, encodeSV2OpenChannelSuccessPayload(requestID, channelID, target, nil)); err != nil {
				return err
			}
			h.shareHandler.getCreateStats(ctx)
			ctx.Logger.Info("client authorized via stratum v2")

		case sv2MsgSubmitSharesStandard:
			channelID, sequence, jobID, nonce, ntime, err := parseSV2SubmitSharePayload(frame.Payload)
			if err != nil {
				_ = writeSV2Frame(ctx, sv2ExtChannel, sv2MsgSubmitSharesError, encodeSV2SubmitErrorPayload(0, 0, "malformed submit"))
				return errors.Wrap(err, "invalid submit payload")
			}
			if !ctx.IsSV2() || channelID != ctx.SV2ChannelID() {
				if err := writeSV2Frame(ctx, sv2ExtChannel, sv2MsgSubmitSharesError, encodeSV2SubmitErrorPayload(channelID, sequence, "wrong channel")); err != nil {
					return err
				}
				continue
			}

			packedNonce := (uint64(ntime) << 32) | uint64(nonce)
			status, submitErr := h.shareHandler.HandleSubmitSV2(ctx, jobID, packedNonce, h.soloMining)
			if submitErr != nil {
				ctx.Logger.Error("sv2 submit failed", zap.Error(submitErr))
				status = SubmitStatusBad
			}
			if status == SubmitStatusAccepted {
				if err := writeSV2Frame(ctx, sv2ExtChannel, sv2MsgSubmitSharesSuccess, encodeSV2SubmitSuccessPayload(channelID, sequence)); err != nil {
					return err
				}
				continue
			}

			code := sv2SubmitStatusCode(status)
			if err := writeSV2Frame(ctx, sv2ExtChannel, sv2MsgSubmitSharesError, encodeSV2SubmitErrorPayload(channelID, sequence, code)); err != nil {
				return err
			}

		default:
			h.logger.Debugf("ignoring unsupported sv2 message type %d (ext=0x%04x)", frame.MessageType, frame.ExtensionType)
		}
	}
}

func readSV2RequestID(payload []byte) uint32 {
	if len(payload) < 4 {
		return 0
	}
	return binary.LittleEndian.Uint32(payload[:4])
}

func sv2SubmitStatusCode(status SubmitStatus) string {
	switch status {
	case SubmitStatusStale:
		return "stale share"
	case SubmitStatusDuplicate:
		return "duplicate share"
	case SubmitStatusLowDiff:
		return "low difficulty"
	default:
		return "invalid share"
	}
}
