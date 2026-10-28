package dnsserver

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"time"

	"github.com/miekg/dns"
)

const maxPendingQueries = 16
const connectionIdleTimeout = 60 * time.Second
const responseWriteTimeout = 10 * time.Second

type ContextHandler interface {
	ServeDNSContext(context.Context, dns.ResponseWriter, *dns.Msg)
}

type activeConnection struct {
	connection net.Conn
	cancel     context.CancelFunc
}

type responseWriter struct {
	connection net.Conn
	cancel     context.CancelFunc
	writeMu    sync.Mutex
}

func ServeTCP(ctx context.Context, listener net.Listener, handler ContextHandler) error {
	if ctx == nil {
		ctx = context.Background()
	}
	serverCtx, cancelServer := context.WithCancel(ctx)
	var connections sync.Map
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		<-serverCtx.Done()
		_ = listener.Close()
		connections.Range(func(_, value any) bool {
			active := value.(activeConnection)
			active.cancel()
			_ = active.connection.Close()
			return true
		})
	}()

	var wait sync.WaitGroup
	defer func() {
		cancelServer()
		_ = listener.Close()
		<-watcherDone
		wait.Wait()
	}()

	for {
		connection, err := listener.Accept()
		if err != nil {
			if serverCtx.Err() != nil {
				return nil
			}
			return err
		}
		connectionCtx, cancelConnection := context.WithCancel(serverCtx)
		active := activeConnection{connection: connection, cancel: cancelConnection}
		connections.Store(connection, active)
		if serverCtx.Err() != nil {
			cancelConnection()
			_ = connection.Close()
			connections.Delete(connection)
			continue
		}
		wait.Add(1)
		go func() {
			defer wait.Done()
			defer connections.Delete(connection)
			defer cancelConnection()
			defer connection.Close()
			serveConnection(connectionCtx, cancelConnection, connection, handler)
		}()
	}
}

func serveConnection(ctx context.Context, cancel context.CancelFunc, connection net.Conn, handler ContextHandler) {
	queries := make(chan *dns.Msg, maxPendingQueries)
	if err := connection.SetReadDeadline(time.Now().Add(connectionIdleTimeout)); err != nil {
		cancel()
		return
	}
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		readQueries(ctx, cancel, connection, queries)
	}()
	defer func() {
		cancel()
		_ = connection.Close()
		<-readerDone
	}()
	writer := &responseWriter{connection: connection, cancel: cancel}
	for {
		if ctx.Err() != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case request, ok := <-queries:
			if !ok {
				return
			}
			handler.ServeDNSContext(ctx, writer, request)
			if err := connection.SetReadDeadline(time.Now().Add(connectionIdleTimeout)); err != nil {
				cancel()
				return
			}
		}
	}
}

func readQueries(ctx context.Context, cancel context.CancelFunc, connection net.Conn, queries chan<- *dns.Msg) {
	defer close(queries)
	for {
		request, err := readMessage(connection)
		if err != nil {
			cancel()
			return
		}
		if err := connection.SetReadDeadline(time.Time{}); err != nil {
			cancel()
			return
		}
		select {
		case queries <- request:
		case <-ctx.Done():
			return
		default:
			cancel()
			return
		}
	}
}

func readMessage(reader io.Reader) (*dns.Msg, error) {
	var prefix [2]byte
	if _, err := io.ReadFull(reader, prefix[:]); err != nil {
		return nil, err
	}
	length := int(binary.BigEndian.Uint16(prefix[:]))
	if length < 12 {
		return nil, io.ErrUnexpectedEOF
	}
	packet := make([]byte, length)
	if _, err := io.ReadFull(reader, packet); err != nil {
		return nil, err
	}
	request := new(dns.Msg)
	if err := request.Unpack(packet); err != nil {
		return nil, err
	}
	return request, nil
}

func (w *responseWriter) LocalAddr() net.Addr { return w.connection.LocalAddr() }

func (w *responseWriter) RemoteAddr() net.Addr { return w.connection.RemoteAddr() }

func (w *responseWriter) WriteMsg(message *dns.Msg) error {
	packet, err := message.Pack()
	if err != nil {
		return err
	}
	_, err = w.Write(packet)
	return err
}

func (w *responseWriter) Write(packet []byte) (int, error) {
	if len(packet) > 65535 {
		return 0, errors.New("DNS TCP message exceeds maximum length")
	}
	frame := make([]byte, 2+len(packet))
	binary.BigEndian.PutUint16(frame[:2], uint16(len(packet)))
	copy(frame[2:], packet)
	w.writeMu.Lock()
	defer w.writeMu.Unlock()
	if err := w.connection.SetWriteDeadline(time.Now().Add(responseWriteTimeout)); err != nil {
		return 0, err
	}
	defer w.connection.SetWriteDeadline(time.Time{})
	if err := writeFull(w.connection, frame); err != nil {
		return 0, err
	}
	return len(packet), nil
}

func writeFull(writer io.Writer, packet []byte) error {
	for len(packet) > 0 {
		written, err := writer.Write(packet)
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
		packet = packet[written:]
	}
	return nil
}

func (w *responseWriter) Close() error {
	w.cancel()
	return w.connection.Close()
}

func (w *responseWriter) TsigStatus() error { return nil }

func (w *responseWriter) TsigTimersOnly(bool) {}

func (w *responseWriter) Hijack() {}
