package sipgo

import (
	"bytes"
	"context"
	"crypto/tls"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/hujm2023/sipgo/sip"
	"github.com/stretchr/testify/require"
)

// stalledTLSPeerForUA starts a real TLS listener that completes the handshake
// and then never reads, keeping accepted connections referenced.
func stalledTLSPeerForUA(t *testing.T) string {
	t.Helper()

	cfg := testServerTlsConfig(t)

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

			go func() { _ = tlsConn.Handshake() }()
		}
	}()

	return ln.Addr().String()
}

func newReliableWriteUA(t *testing.T, timeout time.Duration, opts ...UserAgentOption) (*Client, *UserAgent) {
	t.Helper()

	all := append([]UserAgentOption{
		WithUserAgentHostname("127.0.0.1"),
		WithUserAgentReliableWriteTimeout(timeout),
		WithUserAgenTLSConfig(&tls.Config{InsecureSkipVerify: true}),
	}, opts...)

	ua, err := NewUA(all...)
	require.NoError(t, err)
	t.Cleanup(func() { ua.Close() })

	cli, err := NewClient(ua)
	require.NoError(t, err)
	return cli, ua
}

func TestUserAgentReliableWriteTimeoutRejectsNonPositive(t *testing.T) {
	for _, d := range []time.Duration{0, -time.Second} {
		_, err := NewUA(WithUserAgentReliableWriteTimeout(d))
		require.ErrorIs(t, err, ErrUserAgentReliableWriteTimeout)
	}
}

// TestUserAgentReliableWriteTimeoutBoundsLegacyWrite is the core of the UA
// protection: writes that do not carry a caller context (the legacy
// WriteRequest entry point, and by construction the automatic
// ACK/retransmission writes of the transaction FSMs) are bounded by the
// connection owner.
func TestUserAgentReliableWriteTimeoutBoundsLegacyWrite(t *testing.T) {
	addr := stalledPeerListener(t)
	cli, _ := newReliableWriteUA(t, 300*time.Millisecond)

	req := buildTCPRequest(t, addr, bytes.Repeat([]byte("b"), 4<<20))

	start := time.Now()
	err := cli.WriteRequest(req) // no context at all
	elapsed := time.Since(start)

	require.Error(t, err)
	require.ErrorIs(t, err, sip.ErrWriteStalled)
	require.Less(t, elapsed, 3*time.Second, "the connection budget must end the write")
}

// TestUserAgentReliableWriteTimeoutDisabledByDefault is the compatibility
// guard: without the option nothing changes and the legacy unbounded write
// still blocks.
func TestUserAgentReliableWriteTimeoutDisabledByDefault(t *testing.T) {
	addr, closePeer := stalledPeer(t)

	ua, err := NewUA(WithUserAgentHostname("127.0.0.1"))
	require.NoError(t, err)
	t.Cleanup(func() { ua.Close() })
	cli, err := NewClient(ua)
	require.NoError(t, err)

	req := buildTCPRequest(t, addr, bytes.Repeat([]byte("b"), 4<<20))

	done := make(chan error, 1)
	go func() { done <- cli.WriteRequest(req) }()

	select {
	case err := <-done:
		t.Fatalf("write must stay unbounded by default, but returned: %v", err)
	case <-time.After(700 * time.Millisecond):
	}

	// Release the peer so the test finishes and the blocked write returns.
	closePeer()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("write did not return after the peer was released")
	}
}

// TestUserAgentReliableWriteTimeoutClampsExplicitContext proves the connection
// budget also tightens a caller context that is later than the budget.
func TestUserAgentReliableWriteTimeoutClampsExplicitContext(t *testing.T) {
	addr := stalledPeerListener(t)
	cli, _ := newReliableWriteUA(t, 300*time.Millisecond)

	req := buildTCPRequest(t, addr, bytes.Repeat([]byte("b"), 4<<20))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	start := time.Now()
	err := cli.WriteRequestContext(ctx, req)
	elapsed := time.Since(start)

	require.Error(t, err)
	require.ErrorIs(t, err, sip.ErrWriteStalled)
	require.Less(t, elapsed, 3*time.Second)
}

// TestUserAgentReliableWriteTimeoutCoversInboundResponse proves the budget is
// installed on accepted (inbound) connections too, using the legacy Respond
// path so the protection can only come from the connection owner.
func TestUserAgentReliableWriteTimeoutCoversInboundResponse(t *testing.T) {
	result := make(chan respondResult, 1)

	_, addr := newStalledServer(t, []UserAgentOption{WithUserAgentReliableWriteTimeout(300 * time.Millisecond)}, respondLegacy, result,
		func(req *sip.Request, tx *sip.ServerTx) *sip.Response {
			res := sip.NewResponseFromRequest(req, 486, "Busy Here", nil)
			res.SetBody(bytes.Repeat([]byte("r"), 4<<20))
			return res
		})

	dialStalledRawClient(t, addr)

	select {
	case got := <-result:
		require.Error(t, got.err)
		require.ErrorIs(t, got.err, sip.ErrWriteStalled)
		require.Less(t, got.elapsed, 3*time.Second)

		select {
		case <-got.tx.Done():
		case <-time.After(3 * time.Second):
			t.Fatal("server transaction did not terminate after the bounded write")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("handler did not report the response write result")
	}
}

// TestUserAgentReliableWriteTimeoutCoversTLS proves TLS keeps its real protocol
// and still gets the same connection level budget.
func TestUserAgentReliableWriteTimeoutCoversTLS(t *testing.T) {
	addr := stalledTLSPeerForUA(t)
	cli, _ := newReliableWriteUA(t, 300*time.Millisecond)

	host, port, err := sip.ParseAddr(addr)
	require.NoError(t, err)

	req := sip.NewRequest(sip.OPTIONS, sip.Uri{User: "bob", Host: host, Port: port})
	req.SetTransport("TLS")
	req.SetDestination(addr)
	req.SetBody(bytes.Repeat([]byte("t"), 4<<20))

	start := time.Now()
	err = cli.WriteRequest(req)
	elapsed := time.Since(start)

	require.Error(t, err)
	require.ErrorIs(t, err, sip.ErrWriteStalled)
	require.Less(t, elapsed, 3*time.Second)
}

// TestReliableWriteProtectionKeepsNormalTCPFlow makes sure enabling the
// protection does not change successful transactions or their timers.
func TestReliableWriteProtectionKeepsNormalTCPFlow(t *testing.T) {
	ua, err := NewUA(
		WithUserAgentHostname("127.0.0.1"),
		WithUserAgentReliableWriteTimeout(2*time.Second),
	)
	require.NoError(t, err)
	defer ua.Close()

	srv, err := NewServer(ua)
	require.NoError(t, err)
	srv.OnOptions(func(req *sip.Request, tx sip.ServerTransaction) {
		_ = tx.Respond(sip.NewResponseFromRequest(req, 200, "OK", nil))
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()
	go func() { _ = srv.ServeTCP(ln) }()

	cli, err := NewClient(ua)
	require.NoError(t, err)

	host, port, err := sip.ParseAddr(ln.Addr().String())
	require.NoError(t, err)

	req := sip.NewRequest(sip.OPTIONS, sip.Uri{User: "bob", Host: host, Port: port})
	req.SetTransport("TCP")
	req.SetDestination(ln.Addr().String())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	res, err := cli.Do(ctx, req)
	require.NoError(t, err)
	require.Equal(t, 200, res.StatusCode)

	// Protocol timers are not part of the write protection.
	require.Equal(t, 32*time.Second, sip.Timer_B)
	require.Equal(t, 32*time.Second, sip.Timer_D)
}
