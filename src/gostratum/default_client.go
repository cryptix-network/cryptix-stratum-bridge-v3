package gostratum

import (
	"fmt"
	"strings"

	"github.com/cryptix-network/cryptixd/util"
	"github.com/mattn/go-colorable"
	"github.com/pkg/errors"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

type StratumMethod string

const (
	StratumMethodSubscribe StratumMethod = "mining.subscribe"
	StratumMethodAuthorize StratumMethod = "mining.authorize"
	StratumMethodSubmit    StratumMethod = "mining.submit"
	AnonymousWorkerName    string        = "anonymous"
)

func DefaultLogger() *zap.Logger {
	cfg := zap.NewDevelopmentEncoderConfig()
	cfg.EncodeLevel = zapcore.CapitalColorLevelEncoder
	return zap.New(zapcore.NewCore(
		zapcore.NewConsoleEncoder(cfg),
		zapcore.AddSync(colorable.NewColorableStdout()),
		zapcore.DebugLevel,
	))
}

func DefaultConfig(logger *zap.Logger) StratumListenerConfig {
	return StratumListenerConfig{
		StateGenerator: func() any { return nil },
		HandlerMap:     DefaultHandlers(),
		Port:           ":5555",
		Logger:         logger,
	}
}

func DefaultHandlers() StratumHandlerMap {
	return StratumHandlerMap{
		string(StratumMethodSubscribe): HandleSubscribe,
		string(StratumMethodAuthorize): HandleAuthorize,
		string(StratumMethodSubmit):    HandleSubmit,
	}
}

func HandleAuthorize(ctx *StratumContext, event JsonRpcEvent) error {
	if len(event.Params) < 1 {
		return fmt.Errorf("malformed event from miner, expected param[1] to be address")
	}
	identity, ok := event.Params[0].(string)
	if !ok {
		return fmt.Errorf("malformed event from miner, expected param[1] to be address string")
	}
	address, workerName, err := ParseAuthorizeIdentity(identity)
	if err != nil {
		return fmt.Errorf("invalid wallet format %s: %w", address, err)
	}

	ctx.WalletAddr = address
	ctx.WorkerName = workerName
	ctx.Logger = ctx.Logger.With(zap.String("worker", ctx.WorkerName), zap.String("addr", ctx.WalletAddr))

	if err := ctx.Reply(NewResponse(event, true, nil)); err != nil {
		return errors.Wrap(err, "failed to send response to authorize")
	}
	if ctx.Extranonce != "" {
		SendExtranonce(ctx)
	}

	ctx.Logger.Info(fmt.Sprintf("client authorized, address: %s", ctx.WalletAddr))
	return nil
}

func ParseAuthorizeIdentity(identity string) (wallet string, workerName string, err error) {
	parts := strings.SplitN(identity, ".", 2)
	workerName = AnonymousWorkerName
	address := parts[0]
	if len(parts) == 2 {
		candidate := strings.TrimSpace(parts[1])
		if candidate != "" {
			workerName = candidate
		}
	}

	wallet, err = CleanWallet(address)
	if err != nil {
		return "", "", err
	}
	return wallet, workerName, nil
}

func HandleSubscribe(ctx *StratumContext, event JsonRpcEvent) error {
	if err := ctx.Reply(NewResponse(event,
		[]any{true, "CryptixStratum/1.0.0"}, nil)); err != nil {
		return errors.Wrap(err, "failed to send response to subscribe")
	}
	if len(event.Params) > 0 {
		app, ok := event.Params[0].(string)
		if ok {
			ctx.RemoteApp = app
		}
	}

	ctx.Logger.Info("client subscribed ", zap.Any("context", ctx))
	return nil
}

func HandleSubmit(ctx *StratumContext, event JsonRpcEvent) error {
	// stub
	ctx.Logger.Info("work submission")
	return nil
}

func SendExtranonce(ctx *StratumContext) {
	if err := ctx.Send(NewEvent("", "set_extranonce", []any{ctx.Extranonce})); err != nil {
		// should we doing anything further on failure
		ctx.Logger.Error(errors.Wrap(err, "failed to set extranonce").Error(), zap.Any("context", ctx))
	}
}

func CleanWallet(in string) (string, error) {
	candidate := strings.TrimSpace(in)
	if candidate == "" {
		return "", errors.New("unable to coerce wallet to valid cryptix address")
	}

	// Some miners append worker info with commas/spaces.
	if idx := strings.IndexAny(candidate, ",; \t\r\n"); idx >= 0 {
		candidate = candidate[:idx]
	}

	if strings.HasPrefix(candidate, "cryptix:") {
		if _, err := util.DecodeAddress(candidate, util.Bech32PrefixCryptix); err == nil {
			return candidate, nil
		}
		if payload := extractAddressPayload(strings.TrimPrefix(candidate, "cryptix:")); payload != "" {
			return "cryptix:" + payload, nil
		}
		return "", errors.New("unable to coerce wallet to valid cryptix address")
	}

	if strings.HasPrefix(candidate, "cryptixtest:") {
		if _, err := util.DecodeAddress(candidate, util.Bech32PrefixCryptixTest); err == nil {
			return candidate, nil
		}
		if payload := extractAddressPayload(strings.TrimPrefix(candidate, "cryptixtest:")); payload != "" {
			return "cryptixtest:" + payload, nil
		}
		return "", errors.New("unable to coerce wallet to valid cryptix address")
	}

	mainnet := "cryptix:" + candidate
	if _, err := util.DecodeAddress(mainnet, util.Bech32PrefixCryptix); err == nil {
		return mainnet, nil
	}
	if payload := extractAddressPayload(candidate); payload != "" {
		return "cryptix:" + payload, nil
	}

	testnet := "cryptixtest:" + candidate
	if _, err := util.DecodeAddress(testnet, util.Bech32PrefixCryptixTest); err == nil {
		return testnet, nil
	}

	return "", errors.New("unable to coerce wallet to valid cryptix address")
}

func extractAddressPayload(candidate string) string {
	for i, r := range candidate {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			continue
		}
		return candidate[:i]
	}
	return candidate
}
