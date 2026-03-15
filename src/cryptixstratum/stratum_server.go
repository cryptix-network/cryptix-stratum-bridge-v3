package cryptixstratum

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/cryptix-network/cryptix-stratum-bridge-v3/src/gostratum"
	"github.com/mattn/go-colorable"
	"github.com/pkg/errors"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

const version = "v1.2.3"
const minBlockWaitTime = 500 * time.Millisecond

type BridgeConfig struct {
	StratumPort       string        `yaml:"stratum_port"`
	StratumV2Enabled  bool          `yaml:"stratum_v2_enabled"`
	StratumV2Port     string        `yaml:"stratum_v2_port"`
	RPCServer         string        `yaml:"cryptix_address"`
	PromPort          string        `yaml:"prom_port"`
	PrintStats        bool          `yaml:"print_stats"`
	UseLogFile        bool          `yaml:"log_to_file"`
	HealthCheckPort   string        `yaml:"health_check_port"`
	SoloMining        bool          `yaml:"solo_mining"`
	BlockWaitTime     time.Duration `yaml:"block_wait_time"`
	MinShareDiff      float64       `yaml:"min_share_diff"`
	VarDiff           bool          `yaml:"var_diff"`
	SharesPerMin      uint          `yaml:"shares_per_min"`
	VarDiffStats      bool          `yaml:"var_diff_stats"`
	VarDiffRetarget   time.Duration `yaml:"var_diff_retarget_time"`
	ExtranonceSize    uint          `yaml:"extranonce_size"`
	StratumV2Fallback bool          `yaml:"stratum_v2_fallback_to_v1"`
}

func configureZap(cfg BridgeConfig) (*zap.SugaredLogger, func()) {
	pe := zap.NewProductionEncoderConfig()
	pe.EncodeTime = zapcore.RFC3339TimeEncoder
	fileEncoder := zapcore.NewJSONEncoder(pe)
	consoleEncoder := zapcore.NewConsoleEncoder(pe)

	if !cfg.UseLogFile {
		return zap.New(zapcore.NewCore(consoleEncoder,
			zapcore.AddSync(colorable.NewColorableStdout()), zap.InfoLevel)).Sugar(), func() {}
	}

	// log file fun
	logFile, err := os.OpenFile("bridge.log", os.O_CREATE|os.O_TRUNC|os.O_RDWR, 0666)
	if err != nil {
		panic(err)
	}
	core := zapcore.NewTee(
		zapcore.NewCore(fileEncoder, zapcore.AddSync(logFile), zap.InfoLevel),
		zapcore.NewCore(consoleEncoder, zapcore.AddSync(colorable.NewColorableStdout()), zap.InfoLevel),
	)
	return zap.New(core).Sugar(), func() { logFile.Close() }
}

func ListenAndServe(cfg BridgeConfig) error {
	logger, logCleanup := configureZap(cfg)
	defer logCleanup()

	if cfg.PromPort != "" {
		StartPromServer(logger, cfg.PromPort)
	}

	blockWaitTime := cfg.BlockWaitTime
	if blockWaitTime < minBlockWaitTime {
		blockWaitTime = minBlockWaitTime
	}
	pyApi, err := NewcryptixAPI(cfg.RPCServer, blockWaitTime, logger)
	if err != nil {
		return err
	}

	if cfg.HealthCheckPort != "" {
		logger.Info("enabling health check on port " + cfg.HealthCheckPort)
		healthMux := http.NewServeMux()
		healthMux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
		go func() {
			if err := http.ListenAndServe(cfg.HealthCheckPort, healthMux); err != nil {
				logger.Warn("health check server stopped", zap.Error(err))
			}
		}()
	}

	shareHandler := newShareHandler(pyApi.cryptix)
	minDiff := cfg.MinShareDiff
	if minDiff < 0.0001 {
		minDiff = 0.0001
	}
	extranonceSize := cfg.ExtranonceSize
	if extranonceSize > 3 {
		extranonceSize = 3
	}
	clientHandler := newClientListener(logger, shareHandler, minDiff, int8(extranonceSize))
	handlers := gostratum.DefaultHandlers()
	// override the submit handler with an actual useful handler
	handlers[string(gostratum.StratumMethodSubmit)] =
		func(ctx *gostratum.StratumContext, event gostratum.JsonRpcEvent) error {
			if err := shareHandler.HandleSubmit(ctx, event, cfg.SoloMining); err != nil {
				ctx.Logger.Sugar().Error(err) // sink error
			}
			return nil
		}

	stratumConfig := gostratum.StratumListenerConfig{
		Port:           cfg.StratumPort,
		HandlerMap:     handlers,
		StateGenerator: MiningStateGenerator,
		ClientListener: clientHandler,
		Logger:         logger.Desugar(),
	}

	var stratumV2Config *gostratum.StratumListenerConfig
	if cfg.StratumV2Enabled {
		v2Port := cfg.StratumV2Port
		if v2Port == "" {
			v2Port = ":5556"
		}
		if v2Port == cfg.StratumPort {
			return fmt.Errorf("stratum_v2_port (%s) must be different from stratum_port (%s)", v2Port, cfg.StratumPort)
		}
		logger.Infof("enabling stratum v2 listener on %s (v1 remains on %s)", v2Port, cfg.StratumPort)
		sv2cfg := gostratum.StratumListenerConfig{
			Port:           v2Port,
			HandlerMap:     handlers,
			StateGenerator: MiningStateGenerator,
			ClientListener: clientHandler,
			BinaryHandler:  newSV2Handler(logger, shareHandler, minDiff, cfg.SoloMining, !cfg.StratumV2Fallback),
			Logger:         logger.Desugar(),
		}
		stratumV2Config = &sv2cfg
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pyApi.Start(ctx, func() {
		clientHandler.NewBlockAvailable(pyApi, cfg.SoloMining)
	})

	if cfg.VarDiff || cfg.SoloMining {
		go shareHandler.startVardiffThread(cfg.SharesPerMin, cfg.VarDiffStats, cfg.VarDiffRetarget)
	}

	if cfg.PrintStats {
		go shareHandler.startStatsThread()
	}

	listeners := []*gostratum.StratumListener{
		gostratum.NewListener(stratumConfig),
	}
	if stratumV2Config != nil {
		listeners = append(listeners, gostratum.NewListener(*stratumV2Config))
	}

	errChan := make(chan error, len(listeners))
	for _, listener := range listeners {
		go func(l *gostratum.StratumListener) {
			errChan <- l.Listen(ctx)
		}(listener)
	}

	for i := 0; i < len(listeners); i++ {
		err := <-errChan
		if err == nil || errors.Is(err, context.Canceled) {
			continue
		}
		cancel()
		return err
	}
	return context.Canceled
}
