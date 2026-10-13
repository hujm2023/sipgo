package sip

import (
	"context"
	"crypto/tls"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The existing transport logger gives this fixture a gate after successful
// HandshakeContext, before the creation result is delivered to its caller.
type tlsCreationGate struct {
	entered, release chan struct{}
	once             sync.Once
	count            atomic.Int64
}

func (g *tlsCreationGate) Enabled(context.Context, slog.Level) bool { return true }
func (g *tlsCreationGate) Handle(_ context.Context, r slog.Record) error {
	if r.Message == "New connection" {
		g.count.Add(1)
		g.once.Do(func() { close(g.entered); <-g.release })
	}
	return nil
}
func (g *tlsCreationGate) WithAttrs([]slog.Attr) slog.Handler { return g }
func (g *tlsCreationGate) WithGroup(string) slog.Handler      { return g }

func newGatedTLSTransport(t *testing.T) (*TransportTLS, Addr, *tlsCreationGate, chan struct{}, chan struct{}) {
	t.Helper()
	cfg := testSelfSignedTLSConfig(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { ln.Close() })
	send := make(chan struct{})
	peerDone := make(chan struct{})
	go func() {
		defer close(peerDone)
		raw, e := ln.Accept()
		if e != nil {
			return
		}
		defer raw.Close()
		conn := tls.Server(raw, cfg)
		if conn.Handshake() != nil {
			return
		}
		<-send
		_, _ = io.WriteString(conn, "SIP/2.0 200 OK\r\nVia: SIP/2.0/TLS 127.0.0.1;branch=z9hG4bK-owned\r\nFrom: <sip:a@127.0.0.1>;tag=a\r\nTo: <sip:b@127.0.0.1>;tag=b\r\nCall-ID: owned-reader\r\nCSeq: 1 OPTIONS\r\nContent-Length: 0\r\n\r\n")
		_, _ = io.Copy(io.Discard, conn)
	}()
	g := &tlsCreationGate{entered: make(chan struct{}), release: make(chan struct{})}
	tr := &TransportTLS{TransportTCP: &TransportTCP{log: slog.New(g), connectionReuse: true}}
	tr.init(NewParser(), &tls.Config{InsecureSkipVerify: true})
	return tr, Addr{IP: net.IPv4(127, 0, 0, 1), Port: ln.Addr().(*net.TCPAddr).Port}, g, send, peerDone
}

func TestTLSCreationCanceledCallerKeepsOwnedReader(t *testing.T) {
	tr, addr, g, send, peerDone := newGatedTLSTransport(t)
	received := make(chan struct{})
	finishHandler := make(chan struct{})
	var receiveOnce, releaseOnce sync.Once
	defer func() { releaseOnce.Do(func() { close(finishHandler) }); _ = tr.Close() }()
	handler := func(Message) { receiveOnce.Do(func() { close(received) }); <-finishHandler }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	creator := make(chan error, 1)
	go func() { _, err := tr.CreateConnection(ctx, Addr{}, addr, handler); creator <- err }()
	<-g.entered
	cancel()
	require.ErrorIs(t, <-creator, context.Canceled)
	type result struct {
		conn Connection
		err  error
	}
	waiter := make(chan result, 1)
	go func() {
		c, e := tr.CreateConnection(context.Background(), Addr{}, addr, handler)
		waiter <- result{c, e}
	}()
	close(g.release)
	got := <-waiter
	require.NoError(t, got.err)
	defer got.conn.TryClose()
	close(send)
	observed := false
	select {
	case <-received:
		observed = true
	case <-time.After(300 * time.Millisecond):
		t.Error("successful shared TLS connection has no response reader after creator cancellation")
	}
	// Cache eviction cannot remove the socket's lifecycle owner.
	tr.pool.DeleteMultiple([]string{addr.String(), got.conn.LocalAddr().String()})
	closed := make(chan error, 1)
	go func() { closed <- tr.Close() }()
	if observed {
		select {
		case err := <-closed:
			t.Errorf("transport close returned before reader exited: %v", err)
		case <-time.After(20 * time.Millisecond):
		}
	}
	releaseOnce.Do(func() { close(finishHandler) })
	select {
	case err := <-closed:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("transport close did not join reader")
	}
	<-peerDone
	require.Equal(t, int64(1), g.count.Load(), "one creation owns exactly one reader")
	require.Zero(t, tr.pool.Size())
}

func TestTLSCreationCannotPublishAfterPoolClose(t *testing.T) {
	tr, addr, g, send, peerDone := newGatedTLSTransport(t)
	creator := make(chan error, 1)
	go func() {
		_, err := tr.CreateConnection(context.Background(), Addr{}, addr, func(Message) {})
		creator <- err
	}()
	<-g.entered
	closed := make(chan error, 1)
	go func() { closed <- tr.Close() }()
	closeReturned := false
	select {
	case err := <-closed:
		closeReturned = true
		require.NoError(t, err)
		t.Error("pool close did not wait for its admitted creator")
	case <-time.After(20 * time.Millisecond):
	}
	close(g.release)
	close(send)
	require.ErrorIs(t, <-creator, net.ErrClosed)
	if !closeReturned {
		require.NoError(t, <-closed)
	}
	require.Zero(t, tr.pool.Size(), "closed pool accepted a late publish")
	_ = tr.Close()
	<-peerDone
}
