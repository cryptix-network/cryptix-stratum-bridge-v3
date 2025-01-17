package cryptixstratum

import (
	"context"
	"fmt"
	"time"

	"github.com/cryptix-network/cryptix-stratum-bridge-v3/src/gostratum"
	"github.com/cryptix-network/cryptixd/app/appmessage"
	"github.com/cryptix-network/cryptixd/infrastructure/network/rpcclient"
	"github.com/pkg/errors"
	"go.uber.org/zap"
)

type cryptixApi struct {
	address       string
	blockWaitTime time.Duration
	logger        *zap.SugaredLogger
	cryptix       *rpcclient.RPCClient
	connected     bool
}

func NewcryptixAPI(address string, blockWaitTime time.Duration, logger *zap.SugaredLogger) (*cryptixApi, error) {
	client, err := rpcclient.NewRPCClient(address)
	if err != nil {
		return nil, err
	}

	return &cryptixApi{
		address:       address,
		blockWaitTime: blockWaitTime,
		logger:        logger.With(zap.String("component", "cryptixapi:"+address)),
		cryptix:       client,
		connected:     true,
	}, nil
}

func (py *cryptixApi) Start(ctx context.Context, blockCb func()) {
	py.waitForSync(true)
	go py.startBlockTemplateListener(ctx, blockCb)
	go py.startStatsThread(ctx)
}

func (py *cryptixApi) startStatsThread(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	for {
		select {
		case <-ctx.Done():
			py.logger.Warn("context cancelled, stopping stats thread")
			return
		case <-ticker.C:
			dagResponse, err := py.cryptix.GetBlockDAGInfo()
			if err != nil {
				py.logger.Warn("failed to get network hashrate from cryptix, prom stats will be out of date", zap.Error(err))
				continue
			}
			response, err := py.cryptix.EstimateNetworkHashesPerSecond(dagResponse.TipHashes[0], 1000)
			if err != nil {
				py.logger.Warn("failed to get network hashrate from cryptix, prom stats will be out of date", zap.Error(err))
				continue
			}
			RecordNetworkStats(response.NetworkHashesPerSecond, dagResponse.BlockCount, dagResponse.Difficulty)
		}
	}
}

func (py *cryptixApi) reconnect() error {
	if py.cryptix != nil {
		return py.cryptix.Reconnect()
	}

	client, err := rpcclient.NewRPCClient(py.address)
	if err != nil {
		return err
	}
	py.cryptix = client
	return nil
}

func (s *cryptixApi) waitForSync(verbose bool) error {
	if verbose {
		s.logger.Info("checking cryptix sync state")
	}
	for {
		clientInfo, err := s.cryptix.GetInfo()
		if err != nil {
			return errors.Wrapf(err, "error fetching server info from cryptix @ %s", s.address)
		}
		if clientInfo.IsSynced {
			break
		}
		s.logger.Warn("CryptiX is not synced, waiting for sync before starting bridge")
		time.Sleep(5 * time.Second)
	}
	if verbose {
		s.logger.Info("cryptix synced, starting server")
	}
	return nil
}

func (s *cryptixApi) startBlockTemplateListener(ctx context.Context, blockReadyCb func()) {
	blockReadyChan := make(chan bool)
	err := s.cryptix.RegisterForNewBlockTemplateNotifications(func(_ *appmessage.NewBlockTemplateNotificationMessage) {
		blockReadyChan <- true
	})
	if err != nil {
		s.logger.Error("fatal: failed to register for block notifications from cryptix")
	}

	ticker := time.NewTicker(s.blockWaitTime)
	for {
		if err := s.waitForSync(false); err != nil {
			s.logger.Error("error checking cryptix sync state, attempting reconnect: ", err)
			if err := s.reconnect(); err != nil {
				s.logger.Error("error reconnecting to cryptix, waiting before retry: ", err)
				time.Sleep(5 * time.Second)
			}
		}
		select {
		case <-ctx.Done():
			s.logger.Warn("context cancelled, stopping block update listener")
			return
		case <-blockReadyChan:
			blockReadyCb()
			ticker.Reset(s.blockWaitTime)
		case <-ticker.C: // timeout, manually check for new blocks
			blockReadyCb()
		}
	}
}

func (py *cryptixApi) GetBlockTemplate(
	client *gostratum.StratumContext) (*appmessage.GetBlockTemplateResponseMessage, error) {
	template, err := py.cryptix.GetBlockTemplate(client.WalletAddr,
		fmt.Sprintf(`'%s' via Cryptix/cryptix-stratum-bridge_%s`, client.RemoteApp, version))
	if err != nil {
		return nil, errors.Wrap(err, "failed fetching new block template from cryptix")
	}
	return template, nil
}
