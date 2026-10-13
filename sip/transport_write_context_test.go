package sip

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"math/big"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// stalledTCPPeer starts a real TCP listener whose accepted connections are
// never read from. The peer read buffer is kept tiny so a modest payload
// already blocks the writer on the other side.
func stalledTCPPeer(t *testing.T) string {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { ln.Close() })

	// Accepted connections must stay referenced, otherwise the runtime
	// finalizer closes them and the "stalled" peer turns into a closed one.
	var mu sync.Mutex
	accepted := make([]net.Conn, 0, 4)
	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		for _, c := range accepted {
			_ = c.Close()
		}
	})

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			if tc, ok := c.(*net.TCPConn); ok {
				_ = tc.SetReadBuffer(4096)
			}
			mu.Lock()
			accepted = append(accepted, c)
			mu.Unlock()
			// Held open on purpose: this peer never reads.
		}
	}()

	return ln.Addr().String()
}

func testSelfSignedTLSConfig(t *testing.T) *tls.Config {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "localhost"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)

	return &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
	}
}

// stalledTLSPeer starts a real TLS listener that completes the handshake and
// then never reads.
func stalledTLSPeer(t *testing.T) string {
	t.Helper()

	cfg := testSelfSignedTLSConfig(t)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { ln.Close() })

	var mu sync.Mutex
	accepted := make([]net.Conn, 0, 4)
	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		for _, c := range accepted {
			_ = c.Close()
		}
	})

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			if tc, ok := c.(*net.TCPConn); ok {
				_ = tc.SetReadBuffer(4096)
			}

			tlsConn := tls.Server(c, cfg)
			mu.Lock()
			accepted = append(accepted, tlsConn)
			mu.Unlock()

			go func() {
				// Complete the handshake so the client's dial returns, then
				// deliberately never read again.
				_ = tlsConn.Handshake()
			}()
		}
	}()

	return ln.Addr().String()
}

func TestTCPConnectionWriteContextUnblocksStalledWrite(t *testing.T) {
	addr := stalledTCPPeer(t)

	raw, err := net.Dial("tcp", addr)
	require.NoError(t, err)
	require.NoError(t, raw.(*net.TCPConn).SetWriteBuffer(4096))

	conn := &TCPConnection{Conn: raw, refcount: 1}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	payload := bytes.Repeat([]byte("x"), 1<<20)

	start := time.Now()
	n, err := conn.writeContext(ctx, payload)
	elapsed := time.Since(start)

	require.Error(t, err)
	require.ErrorIs(t, err, ErrWriteStalled)
	require.Less(t, n, len(payload), "write must not have completed")
	require.Less(t, elapsed, 3*time.Second, "write must return on the budget, not on the peer")

	// The stalled connection is hard-closed, so it can not be reused.
	_, err = conn.writeContext(context.Background(), []byte("PING"))
	require.Error(t, err)
}

func TestTLSConnectionWriteContextUnblocksStalledWrite(t *testing.T) {
	addr := stalledTLSPeer(t)

	raw, err := net.Dial("tcp", addr)
	require.NoError(t, err)
	require.NoError(t, raw.(*net.TCPConn).SetWriteBuffer(4096))

	tlsConn := tls.Client(raw, &tls.Config{InsecureSkipVerify: true})
	require.NoError(t, tlsConn.HandshakeContext(context.Background()))

	conn := &TCPConnection{Conn: tlsConn, refcount: 1}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	payload := bytes.Repeat([]byte("y"), 1<<20)

	start := time.Now()
	_, err = conn.writeContext(ctx, payload)
	elapsed := time.Since(start)

	require.Error(t, err)
	require.ErrorIs(t, err, ErrWriteStalled)
	require.Less(t, elapsed, 3*time.Second)
}

// TestTCPConnectionQueuedWriteCancelKeepsConnection proves that giving up while
// the write is still queued behind another writer does not touch a healthy
// connection: a following bounded write still succeeds and reaches the peer.
func TestTCPConnectionQueuedWriteCancelKeepsConnection(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()

	conn := &TCPConnection{Conn: client, refcount: 1}

	// Simulate a writer that already owns the connection write token.
	token := conn.writeToken()
	<-token

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := conn.writeContext(ctx, []byte("QUEUED"))
	elapsed := time.Since(start)

	require.Error(t, err)
	require.ErrorIs(t, err, ErrWriteCanceled)
	require.NotErrorIs(t, err, ErrWriteStalled)
	require.Less(t, elapsed, 2*time.Second)

	// Release the token: the connection must still be healthy.
	token <- struct{}{}

	payload := []byte("HEALTHY")
	received := make(chan []byte, 1)
	go func() {
		buf := make([]byte, len(payload))
		if _, err := io.ReadFull(server, buf); err != nil {
			received <- nil
			return
		}
		received <- buf
	}()

	wctx, wcancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer wcancel()

	n, err := conn.writeContext(wctx, payload)
	require.NoError(t, err)
	require.Equal(t, len(payload), n)
	require.Equal(t, payload, <-received)
}

// TestTCPConnectionWriteContextHonorsBudgetAndKeepsHealthyConnection covers the
// success path: a write that fits the budget leaves the connection usable.
func TestTCPConnectionWriteContextHonorsBudgetAndKeepsHealthyConnection(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()

	conn := &TCPConnection{Conn: client, refcount: 1}
	conn.setWriteBudget(2 * time.Second)

	payload := []byte("BUDGETED")
	received := make(chan []byte, 1)
	go func() {
		buf := make([]byte, len(payload))
		if _, err := io.ReadFull(server, buf); err != nil {
			received <- nil
			return
		}
		received <- buf
	}()

	start := time.Now()
	// No context at all: only the connection budget applies.
	n, err := conn.Write(payload)
	require.NoError(t, err)
	require.Equal(t, len(payload), n)
	require.Less(t, time.Since(start), 2*time.Second)
	require.Equal(t, payload, <-received)
}

// TestTCPConnectionWriteAlreadyExpiredContextKeepsConnection proves that a ctx
// that is already over budget before the write starts never closes the
// connection.
func TestTCPConnectionWriteAlreadyExpiredContextKeepsConnection(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()

	conn := &TCPConnection{Conn: client, refcount: 1}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := conn.writeContext(ctx, []byte("NOPE"))
	require.ErrorIs(t, err, ErrWriteCanceled)

	payload := []byte("STILL-OK")
	go func() {
		buf := make([]byte, len(payload))
		_, _ = io.ReadFull(server, buf)
	}()

	wctx, wcancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer wcancel()
	n, err := conn.writeContext(wctx, payload)
	require.NoError(t, err)
	require.Equal(t, len(payload), n)
}

// nonCancelableConn is a Connection that does not implement ContextWriter.
type nonCancelableConn struct {
	writes int
}

func (c *nonCancelableConn) LocalAddr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 5060}
}
func (c *nonCancelableConn) WriteMsg(msg Message) error { c.writes++; return nil }
func (c *nonCancelableConn) Ref(i int) int              { return 0 }
func (c *nonCancelableConn) TryClose() (int, error)     { return 0, nil }
func (c *nonCancelableConn) Close() error               { return nil }

func TestTransportLayerBoundedWriteRejectsNonCancelableConnWhenProtected(t *testing.T) {
	l := &TransportLayer{reliableWriteTimeout: 2 * time.Second}
	req := NewRequest(OPTIONS, Uri{Host: "127.0.0.1", Port: 5060})
	req.SetTransport("TCP")

	conn := &nonCancelableConn{}

	err := l.writeConnMessage(context.Background(), conn, req, "TCP")
	require.ErrorIs(t, err, ErrWriteNotCancelable)
	require.Zero(t, conn.writes, "a rejected bounded write must not fall back to an unbounded write")

	// Without a configured protection the legacy write path is preserved.
	l.reliableWriteTimeout = 0
	require.NoError(t, l.writeConnMessage(context.Background(), conn, req, "TCP"))
	require.Equal(t, 1, conn.writes)

	// UDP is out of the protected set even with the protection enabled.
	l.reliableWriteTimeout = 2 * time.Second
	req.SetTransport("UDP")
	require.NoError(t, l.writeConnMessage(context.Background(), conn, req, "UDP"))
	require.Equal(t, 2, conn.writes)
}

func TestIsProtectedReliable(t *testing.T) {
	require.True(t, isProtectedReliable("TCP"))
	require.True(t, isProtectedReliable("tcp"))
	require.True(t, isProtectedReliable("TLS"))
	require.True(t, isProtectedReliable("tls"))
	require.False(t, isProtectedReliable("UDP"))
	require.False(t, isProtectedReliable("WS"))
	require.False(t, isProtectedReliable("WSS"))
}
