package sipgo

import (
	"context"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/hujm2023/sipgo/sip"
	"github.com/stretchr/testify/require"
)

// TestListenAndServeUDPRegisteredHook verifies the new registration signal:
// it fires after bind-ready, once the listener is reusable in the pool, and a
// channel without a consumer must not stall the receive loop.
func TestListenAndServeUDPRegisteredHook(t *testing.T) {
	ua, err := NewUA()
	require.NoError(t, err)
	defer ua.Close()
	srv, err := NewServer(ua)
	require.NoError(t, err)

	srv.OnOptions(func(req *sip.Request, tx sip.ServerTransaction) {
		_ = tx.Respond(sip.NewResponseFromRequest(req, 200, "OK", nil))
	})

	bindReady := make(chan string, 1)
	registered := make(chan string, 1)
	ctx := context.WithValue(context.Background(), ListenReadyCtxKey, ListenReadyFuncCtxValue(func(network, addr string) {
		select {
		case <-registered:
			t.Errorf("bind-ready fired after registered")
		default:
		}
		bindReady <- addr
	}))
	ctx = context.WithValue(ctx, ListenRegisteredCtxKey, ListenReadyFuncCtxValue(func(network, addr string) {
		registered <- addr
	}))

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- srv.ListenAndServe(ctx, "udp", "127.0.0.1:0")
	}()

	var boundAddr string
	select {
	case boundAddr = <-bindReady:
	case <-time.After(2 * time.Second):
		t.Fatal("bind-ready not fired")
	}
	select {
	case regAddr := <-registered:
		require.Equal(t, boundAddr, regAddr)
	case <-time.After(2 * time.Second):
		t.Fatal("registered not fired")
	}

	// At registered time the bound connection is reusable from the pool.
	conn, err := ua.TransportLayer().GetConnection("udp", boundAddr)
	require.NoError(t, err)
	_, err = conn.TryClose()
	require.NoError(t, err)

	cancel()
	select {
	case err := <-serveErr:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("ListenAndServe did not return after context cancel")
	}
}

func TestListenAndServeUDPRegisteredChannelWithoutConsumer(t *testing.T) {
	ua, err := NewUA()
	require.NoError(t, err)
	defer ua.Close()
	srv, err := NewServer(ua)
	require.NoError(t, err)

	gotRequest := make(chan struct{}, 1)
	srv.OnOptions(func(req *sip.Request, tx sip.ServerTransaction) {
		gotRequest <- struct{}{}
		_ = tx.Respond(sip.NewResponseFromRequest(req, 200, "OK", nil))
	})

	// Unbuffered, and nobody ever reads it. Delivery must be dropped instead
	// of blocking the serving goroutine between pool registration and reading.
	registered := make(ListenReadyCtxValue)
	ctx := context.WithValue(context.Background(), ListenRegisteredCtxKey, registered)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- srv.ListenAndServe(ctx, "udp", "127.0.0.1:0")
	}()

	// Discover the kernel-selected port from the transport layer rather than
	// from the hook that this test deliberately leaves unconsumed.
	actualAddr := waitForRegisteredUDPPort(t, ua, serveErr)

	peerConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	require.NoError(t, err)
	defer peerConn.Close()
	dst, err := net.ResolveUDPAddr("udp", actualAddr)
	require.NoError(t, err)
	_, err = peerConn.WriteTo([]byte("OPTIONS sip:example.com SIP/2.0\r\n"+
		"Via: SIP/2.0/UDP 127.0.0.1:1\r\n"+
		"From: <sip:a@b>;tag=f\r\n"+
		"To: <sip:c@d>\r\n"+
		"Call-ID: no-consumer\r\n"+
		"CSeq: 1 OPTIONS\r\n"+
		"Content-Length: 0\r\n\r\n"), dst)
	require.NoError(t, err)

	select {
	case <-gotRequest:
	case <-time.After(2 * time.Second):
		t.Fatal("receive loop stalled because registered channel had no consumer")
	}

	cancel()
	select {
	case <-serveErr:
	case <-time.After(2 * time.Second):
		t.Fatal("ListenAndServe did not return after context cancel")
	}
}

func waitForRegisteredUDPPort(t *testing.T, ua *UserAgent, serveErr <-chan error) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-serveErr:
			t.Fatalf("ListenAndServe exited early: %v", err)
		default:
		}
		if ports := ua.TransportLayer().ListenPorts("udp"); len(ports) == 1 && ports[0] != 0 {
			return net.JoinHostPort("127.0.0.1", strconv.Itoa(ports[0]))
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("listener UDP port never registered")
	return ""
}
