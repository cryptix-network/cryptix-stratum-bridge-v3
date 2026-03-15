package gostratum

import (
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

type MockConnection struct {
	id            string
	inChan        chan []byte
	outChan       chan []byte
	closed        atomic.Bool
	closeOnce     sync.Once
	readDeadline  atomic.Int64
	writeDeadline atomic.Int64
}

var channelCounter int32

func NewMockConnection() *MockConnection {
	return &MockConnection{
		id:      fmt.Sprintf("mc_%d", atomic.AddInt32(&channelCounter, 1)),
		inChan:  make(chan []byte),
		outChan: make(chan []byte),
	}
}

func (mc *MockConnection) AsyncWriteTestDataToReadBuffer(s string) {
	go func() {
		if mc.closed.Load() {
			return
		}
		defer func() { _ = recover() }()
		mc.inChan <- []byte(s)
	}()
}

func (mc *MockConnection) ReadTestDataFromBuffer(handler func([]byte)) {
	read, ok := <-mc.outChan
	if !ok {
		return
	}
	handler(read)
}

func (mc *MockConnection) AsyncReadTestDataFromBuffer(handler func([]byte)) {
	go func() {
		read, ok := <-mc.outChan
		if !ok {
			return
		}
		handler(read)
	}()
}

func (mc *MockConnection) Read(b []byte) (int, error) {
	if mc.closed.Load() {
		return 0, io.EOF
	}

	readDeadline := mc.readDeadline.Load()
	timeout := time.Until(time.Unix(0, readDeadline))
	if readDeadline > 0 {
		if timeout <= 0 {
			return 0, os.ErrDeadlineExceeded
		}
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		select {
		case data, ok := <-mc.inChan:
			if !ok {
				return 0, io.EOF
			}
			return copy(b, data), nil
		case <-timer.C:
			return 0, os.ErrDeadlineExceeded
		}
	}

	data, ok := <-mc.inChan
	if !ok {
		return 0, io.EOF
	}
	return copy(b, data), nil
}

func (mc *MockConnection) Write(b []byte) (int, error) {
	if mc.closed.Load() {
		return 0, io.EOF
	}

	writeDeadline := mc.writeDeadline.Load()
	timeout := time.Until(time.Unix(0, writeDeadline))
	if writeDeadline > 0 {
		if timeout <= 0 {
			return 0, os.ErrDeadlineExceeded
		}
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		select {
		case mc.outChan <- b:
			return len(b), nil
		case <-timer.C:
			return 0, os.ErrDeadlineExceeded
		}
	}

	mc.outChan <- b
	return len(b), nil
}

func (mc *MockConnection) Close() error {
	mc.closeOnce.Do(func() {
		mc.closed.Store(true)
		close(mc.inChan)
		close(mc.outChan)
	})
	return nil
}

type MockAddr struct {
	id string
}

func (ma MockAddr) Network() string { return "mock" }
func (ma MockAddr) String() string  { return ma.id }

func (mc *MockConnection) LocalAddr() net.Addr {
	return MockAddr{id: mc.id}
}

func (mc *MockConnection) RemoteAddr() net.Addr {
	return MockAddr{id: mc.id}
}

func (mc *MockConnection) SetDeadline(t time.Time) error {
	mc.SetReadDeadline(t)
	mc.SetWriteDeadline(t)
	return nil
}

func (mc *MockConnection) SetReadDeadline(t time.Time) error {
	if t.IsZero() {
		mc.readDeadline.Store(0)
		return nil
	}
	mc.readDeadline.Store(t.UnixNano())
	return nil
}

func (mc *MockConnection) SetWriteDeadline(t time.Time) error {
	if t.IsZero() {
		mc.writeDeadline.Store(0)
		return nil
	}
	mc.writeDeadline.Store(t.UnixNano())
	return nil
}
