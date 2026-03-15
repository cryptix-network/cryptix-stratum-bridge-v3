package gostratum

import (
	"bufio"
	"bytes"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/pkg/errors"
	"go.uber.org/zap"
)

func spawnClientListener(ctx *StratumContext, connection net.Conn, s *StratumListener) error {
	defer ctx.Disconnect()
	reader := bufio.NewReader(connection)

	for {
		line, err := readFromConnection(connection, reader)
		if errors.Is(err, os.ErrDeadlineExceeded) {
			continue // expected timeout
		}
		if ctx.Err() != nil {
			return ctx.Err() // context cancelled
		}
		if ctx.parentContext.Err() != nil {
			return ctx.parentContext.Err() // parent context cancelled
		}
		if err != nil { // actual error
			ctx.Logger.Error("error reading from socket", zap.Error(err))
			return err
		}

		if line == "" {
			continue
		}

		event, err := UnmarshalEvent(line)
		if err != nil {
			ctx.Logger.Error("error unmarshalling event", zap.String("raw", line))
			return err
		}
		if err := s.HandleEvent(ctx, event); err != nil {
			return err
		}
	}
}

const maxEventLineBytes = 1 << 20

func readFromConnection(connection net.Conn, reader *bufio.Reader) (string, error) {
	deadline := time.Now().Add(5 * time.Second).UTC()
	if err := connection.SetReadDeadline(deadline); err != nil {
		return "", err
	}

	line, err := reader.ReadBytes('\n')
	if err != nil {
		return "", errors.Wrapf(err, "error reading from connection")
	}

	if len(line) > maxEventLineBytes {
		return "", fmt.Errorf("incoming stratum message exceeds max length: %d bytes", len(line))
	}

	line = bytes.ReplaceAll(line, []byte("\x00"), nil)
	line = bytes.TrimSpace(line)
	return string(line), nil
}
