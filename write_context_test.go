package sipgo

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/hujm2023/sipgo/sip"
	"github.com/stretchr/testify/require"
)

// stalledPeerListener accepts connections, gives them a tiny receive buffer and
// never reads from them, so a writer on the other side really blocks.
func stalledPeerListener(t *testing.T) string {
	t.Helper()
	addr, _ := stalledPeer(t)
	return addr
}

// stalledPeer is stalledPeerListener plus a closer that releases every accepted
// connection, used to finish a deliberately unbounded write.
func stalledPeer(t *testing.T) (string, func()) {
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
			// Held open on purpose: never read.
		}
	}()

	closeAccepted := func() {
		mu.Lock()
		defer mu.Unlock()
		for _, c := range accepted {
			_ = c.Close()
		}
	}

	return ln.Addr().String(), closeAccepted
}

// drainingPeerListener accepts connections and consumes everything, so writes
// always complete.
func drainingPeerListener(t *testing.T) string {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { ln.Close() })

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(io.Discard, c) }()
		}
	}()

	return ln.Addr().String()
}

// connTuningListener lets a test tune the server side of accepted connections.
type connTuningListener struct {
	net.Listener
	tune func(net.Conn)
}

func (l *connTuningListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	if l.tune != nil {
		l.tune(c)
	}
	return c, nil
}

func newWriteContextClient(t *testing.T) (*Client, *UserAgent) {
	t.Helper()

	ua, err := NewUA(WithUserAgentHostname("127.0.0.1"))
	require.NoError(t, err)
	t.Cleanup(func() { ua.Close() })

	cli, err := NewClient(ua)
	require.NoError(t, err)
	return cli, ua
}

func buildTCPRequest(t *testing.T, addr string, body []byte) *sip.Request {
	t.Helper()

	host, port, err := sip.ParseAddr(addr)
	require.NoError(t, err)

	req := sip.NewRequest(sip.OPTIONS, sip.Uri{User: "bob", Host: host, Port: port})
	req.SetTransport("TCP")
	req.SetDestination(addr)
	req.SetBody(body)
	return req
}

// TestWriteRequestContextUnblocksStalledTCPPeer proves the submission budget
// really returns while the socket write is blocked, and that the stalled
// connection is closed instead of being reused.
func TestWriteRequestContextUnblocksStalledTCPPeer(t *testing.T) {
	addr := stalledPeerListener(t)
	cli, ua := newWriteContextClient(t)

	req := buildTCPRequest(t, addr, bytes.Repeat([]byte("b"), 4<<20))

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := cli.WriteRequestContext(ctx, req)
	elapsed := time.Since(start)

	require.Error(t, err)
	require.ErrorIs(t, err, sip.ErrWriteStalled)
	require.Less(t, elapsed, 3*time.Second, "write must return on the budget")

	// The exact stalled connection must be evicted, so the next work dials a
	// fresh one instead of reusing a poisoned socket.
	conn, _ := ua.TransportLayer().GetConnection("tcp", addr)
	require.Nil(t, conn)
}

// TestWriteRequestContextSuccessKeepsHealthyConnection proves a bounded write
// that fits its budget leaves the connection pooled and reusable.
func TestWriteRequestContextSuccessKeepsHealthyConnection(t *testing.T) {
	addr := drainingPeerListener(t)
	cli, ua := newWriteContextClient(t)

	for i := 0; i < 2; i++ {
		req := buildTCPRequest(t, addr, []byte("hello"))
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		err := cli.WriteRequestContext(ctx, req)
		cancel()
		require.NoError(t, err)
	}

	conn, err := ua.TransportLayer().GetConnection("tcp", addr)
	require.NoError(t, err)
	require.NotNil(t, conn)
}

// TestTransactionRequestContextUnblocksStalledTCPPeer covers the transaction
// entry point: the initial request write inherits the caller context and the
// failed transaction does not leak.
func TestTransactionRequestContextUnblocksStalledTCPPeer(t *testing.T) {
	addr := stalledPeerListener(t)
	cli, _ := newWriteContextClient(t)

	req := sip.NewRequest(sip.INVITE, sip.Uri{User: "bob", Host: "127.0.0.1"})
	req.SetTransport("TCP")
	req.SetDestination(addr)
	req.SetBody(bytes.Repeat([]byte("c"), 4<<20))

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	start := time.Now()
	tx, err := cli.TransactionRequest(ctx, req)
	elapsed := time.Since(start)

	require.Error(t, err)
	require.Nil(t, tx)
	require.ErrorIs(t, err, sip.ErrWriteStalled)
	require.Less(t, elapsed, 3*time.Second)
}

type respondResult struct {
	err     error
	elapsed time.Duration
	tx      *sip.ServerTx
}

// serverRespondMode selects the response entry point used by the test handler.
type serverRespondMode int

const (
	// respondWithContext uses ServerTx.RespondContext with a short caller budget.
	respondWithContext serverRespondMode = iota
	// respondLegacy uses ServerTx.Respond, i.e. no caller context at all.
	respondLegacy
)

func newStalledServer(t *testing.T, uaOpts []UserAgentOption, mode serverRespondMode, result chan<- respondResult, respond func(req *sip.Request, tx *sip.ServerTx) *sip.Response) (*Server, string) {
	t.Helper()

	opts := append([]UserAgentOption{WithUserAgentHostname("127.0.0.1")}, uaOpts...)
	ua, err := NewUA(opts...)
	require.NoError(t, err)
	t.Cleanup(func() { ua.Close() })

	srv, err := NewServer(ua)
	require.NoError(t, err)

	srv.OnInvite(func(req *sip.Request, tx sip.ServerTransaction) {
		stx, ok := tx.(*sip.ServerTx)
		if !ok {
			return
		}

		res := respond(req, stx)

		start := time.Now()
		var err error
		if mode == respondLegacy {
			err = stx.Respond(res)
		} else {
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			defer cancel()
			err = stx.RespondContext(ctx, res)
		}
		result <- respondResult{err: err, elapsed: time.Since(start), tx: stx}
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { ln.Close() })

	// Keep the server side write buffer tiny so the response write really
	// blocks once the client stops reading.
	tln := &connTuningListener{Listener: ln, tune: func(c net.Conn) {
		if tc, ok := c.(*net.TCPConn); ok {
			_ = tc.SetWriteBuffer(4096)
		}
	}}

	go func() { _ = srv.ServeTCP(tln) }()

	return srv, ln.Addr().String()
}

func rawInvite() []byte {
	return []byte("INVITE sip:bob@127.0.0.1 SIP/2.0\r\n" +
		"Via: SIP/2.0/TCP 127.0.0.1:5062;branch=z9hG4bK-write-ctx-1\r\n" +
		"Max-Forwards: 70\r\n" +
		"To: <sip:bob@127.0.0.1>\r\n" +
		"From: <sip:alice@127.0.0.1>;tag=from-1\r\n" +
		"Call-ID: write-ctx-call-1\r\n" +
		"CSeq: 1 INVITE\r\n" +
		"Contact: <sip:alice@127.0.0.1:5062>\r\n" +
		"Content-Length: 0\r\n" +
		"\r\n")
}

// dialStalledRawClient connects to addr, keeps a tiny read buffer and sends a
// valid INVITE without ever reading the response.
func dialStalledRawClient(t *testing.T, addr string) net.Conn {
	t.Helper()

	raw, err := net.Dial("tcp", addr)
	require.NoError(t, err)
	t.Cleanup(func() { raw.Close() })

	require.NoError(t, raw.(*net.TCPConn).SetReadBuffer(4096))
	_, err = raw.Write(rawInvite())
	require.NoError(t, err)
	return raw
}

// TestServerTxRespondContextUnblocksStalledTCPPeer proves RespondContext still
// goes through the server transaction FSM, and that the budget really unblocks
// the response write and terminates the transaction.
func TestServerTxRespondContextUnblocksStalledTCPPeer(t *testing.T) {
	result := make(chan respondResult, 1)

	_, addr := newStalledServer(t, nil, respondWithContext, result, func(req *sip.Request, tx *sip.ServerTx) *sip.Response {
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
			t.Fatal("server transaction did not terminate after the stalled write")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("handler did not report the response write result")
	}
}

// TestServerTxRespondContextDeliversToReadingPeer is the positive path: the
// response written through the FSM with a live context reaches the peer.
func TestServerTxRespondContextDeliversToReadingPeer(t *testing.T) {
	result := make(chan respondResult, 1)

	_, addr := newStalledServer(t, nil, respondWithContext, result, func(req *sip.Request, tx *sip.ServerTx) *sip.Response {
		return sip.NewResponseFromRequest(req, 486, "Busy Here", nil)
	})

	raw, err := net.Dial("tcp", addr)
	require.NoError(t, err)
	defer raw.Close()
	_, err = raw.Write(rawInvite())
	require.NoError(t, err)

	buf := make([]byte, 1024)
	require.NoError(t, raw.SetReadDeadline(time.Now().Add(5*time.Second)))
	n, err := raw.Read(buf)
	require.NoError(t, err)
	require.Contains(t, string(buf[:n]), "SIP/2.0 486")

	select {
	case got := <-result:
		require.NoError(t, got.err)
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not report the response write result")
	}
}

// TestWriteRequestContextUDPUnchanged proves the UDP path keeps its legacy
// behavior: the datagram is delivered and the call succeeds.
func TestWriteRequestContextUDPUnchanged(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	defer pc.Close()

	received := make(chan []byte, 1)
	go func() {
		buf := make([]byte, 4096)
		n, _, err := pc.ReadFrom(buf)
		if err != nil {
			return
		}
		received <- append([]byte(nil), buf[:n]...)
	}()

	ua, err := NewUA(WithUserAgentHostname("127.0.0.1"))
	require.NoError(t, err)
	defer ua.Close()
	cli, err := NewClient(ua)
	require.NoError(t, err)

	host, port, err := sip.ParseAddr(pc.LocalAddr().String())
	require.NoError(t, err)

	req := sip.NewRequest(sip.OPTIONS, sip.Uri{User: "bob", Host: host, Port: port})
	req.SetTransport("UDP")
	req.SetDestination(pc.LocalAddr().String())

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	require.NoError(t, cli.WriteRequestContext(ctx, req))

	select {
	case data := <-received:
		require.Contains(t, string(data), "OPTIONS ")
	case <-time.After(3 * time.Second):
		t.Fatal("udp request was not delivered")
	}
}

// recordingConn is a sip.Connection that records messages and never blocks.
type recordingConn struct {
	mu   sync.Mutex
	msgs []sip.Message
}

func (c *recordingConn) LocalAddr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 5060}
}
func (c *recordingConn) WriteMsg(msg sip.Message) error {
	c.mu.Lock()
	c.msgs = append(c.msgs, msg)
	c.mu.Unlock()
	return nil
}
func (c *recordingConn) Ref(i int) int          { return 0 }
func (c *recordingConn) TryClose() (int, error) { return 0, nil }
func (c *recordingConn) Close() error           { return nil }

func newTestDialogSession(t *testing.T, cli *Client, addr string) *DialogClientSession {
	t.Helper()

	host, port, err := sip.ParseAddr(addr)
	require.NoError(t, err)

	inviteReq := sip.NewRequest(sip.INVITE, sip.Uri{User: "bob", Host: host, Port: port})
	inviteReq.SetTransport("TCP")
	inviteReq.SetDestination(addr)
	inviteReq.SetBody(nil)

	dua := &DialogUA{
		Client: cli,
		ContactHDR: sip.ContactHeader{
			Address: sip.Uri{User: "alice", Host: "127.0.0.1", Port: 5060},
		},
	}

	dtx, err := dua.WriteInvite(context.Background(), inviteReq)
	require.NoError(t, err)

	// Fabricate the 2xx that establishes the dialog. ACK construction only
	// relies on the INVITE request plus this response. The response Contact is
	// dropped so the request keeps targeting the test peer instead of the
	// locally advertised contact.
	dtx.InviteResponse = sip.NewResponseFromRequest(inviteReq, 200, "OK", nil)
	dtx.InviteResponse.RemoveHeader("Contact")
	return dtx
}

// TestDialogSessionAckPropagatesContext is the regression for Ack(ctx) silently
// ignoring its context: a canceled context must not write anything.
func TestDialogSessionAckPropagatesContext(t *testing.T) {
	addr := drainingPeerListener(t)
	cli, _ := newWriteContextClient(t)

	dtx := newTestDialogSession(t, cli, addr)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := dtx.Ack(ctx)
	require.Error(t, err)
	require.ErrorIs(t, err, sip.ErrWriteCanceled)
}

// TestDialogSessionWriteRequestContextUnblocksStalledTCPPeer covers the
// in-dialog ctx write path (ACK/BYE style) against a stalled peer.
func TestDialogSessionWriteRequestContextUnblocksStalledTCPPeer(t *testing.T) {
	addr := stalledPeerListener(t)
	cli, _ := newWriteContextClient(t)

	dtx := newTestDialogSession(t, cli, addr)

	host, port, err := sip.ParseAddr(addr)
	require.NoError(t, err)

	ack := sip.NewRequest(sip.ACK, sip.Uri{User: "bob", Host: host, Port: port})
	ack.SetTransport("TCP")
	ack.SetBody(bytes.Repeat([]byte("a"), 4<<20))

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	start := time.Now()
	err = dtx.WriteRequestContext(ctx, ack)
	elapsed := time.Since(start)

	require.Error(t, err)
	require.ErrorIs(t, err, sip.ErrWriteStalled)
	require.Less(t, elapsed, 3*time.Second)
}

// TestServerWriteResponseContextUsesLegacyPathWithoutBudget keeps the stateless
// server entry point honest: without a configured protection it must not reject
// connections that cannot honor a context.
func TestServerWriteResponseContextUsesLegacyPathWithoutBudget(t *testing.T) {
	ua, err := NewUA()
	require.NoError(t, err)
	defer ua.Close()

	srv, err := NewServer(ua)
	require.NoError(t, err)

	// No connection in the pool for this address: the call must fail with the
	// usual lookup error, not with ErrWriteNotCancelable.
	res := sip.NewResponse(200, "OK")
	res.SetTransport("TCP")
	res.SetDestination("127.0.0.1:65000")

	err = srv.WriteResponseContext(context.Background(), res)
	require.Error(t, err)
	require.False(t, errors.Is(err, sip.ErrWriteNotCancelable))
}

// The submit budget covers establishing TLS, even before a writer exists.
func TestReliableSubmitBudgetBoundsTLSHandshake(t *testing.T) {
	for _, mode := range []string{"transaction", "client write", "transport write"} {
		t.Run(mode, func(t *testing.T) {
			for _, tc := range []struct {
				name           string
				budget, parent time.Duration
			}{
				{"UA deadline", 60 * time.Millisecond, 600 * time.Millisecond},
				{"earlier caller", 600 * time.Millisecond, 60 * time.Millisecond},
			} {
				t.Run(tc.name, func(t *testing.T) {
					ln, err := net.Listen("tcp", "127.0.0.1:0")
					require.NoError(t, err)
					defer ln.Close()
					peerDone := make(chan error, 1)
					go func() {
						c, e := ln.Accept()
						if e != nil {
							peerDone <- e
							return
						}
						defer c.Close()
						_, e = io.Copy(io.Discard, c)
						peerDone <- e
					}()
					ua, err := NewUA(WithUserAgentReliableWriteTimeout(tc.budget), WithUserAgenTLSConfig(&tls.Config{InsecureSkipVerify: true}))
					require.NoError(t, err)
					defer ua.Close()
					cli, err := NewClient(ua)
					require.NoError(t, err)
					req := buildTCPRequest(t, ln.Addr().String(), nil)
					req.SetTransport("TLS")
					ctx, cancel := context.WithTimeout(context.Background(), tc.parent)
					defer cancel()
					start := time.Now()
					switch mode {
					case "transaction":
						var tx sip.ClientTransaction
						tx, err = cli.TransactionRequest(ctx, req)
						if err == nil {
							tx.Terminate()
						}
					case "client write":
						err = cli.WriteRequestContext(ctx, req)
					case "transport write":
						require.NoError(t, clientRequestBuildReq(cli, req))
						err = ua.tp.WriteMsgContext(ctx, req)
					}
					require.ErrorIs(t, err, context.DeadlineExceeded)
					require.Less(t, time.Since(start), 300*time.Millisecond)
					select {
					case e := <-peerDone:
						require.NoError(t, e)
					case <-time.After(time.Second):
						t.Fatal("failed TLS handshake did not close/join the peer socket")
					}
				})
			}
		})
	}
}

// A submit gets one budget, not a fresh write budget after TLS establishment.
func TestReliableSubmitBudgetIsSharedByHandshakeAndWrite(t *testing.T) {
	for _, mode := range []string{"transaction", "legacy transport"} {
		t.Run(mode, func(t *testing.T) {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, err)
			defer ln.Close()
			serverConfig := testServerTlsConfig(t)
			stop := make(chan struct{})
			peerDone := make(chan struct{})
			defer func() { close(stop); <-peerDone }()
			go func() {
				defer close(peerDone)
				raw, e := ln.Accept()
				if e != nil {
					return
				}
				defer raw.Close()
				_ = raw.(*net.TCPConn).SetReadBuffer(4096)
				// This real establishment phase consumes most of the same submit budget.
				time.Sleep(250 * time.Millisecond)
				conn := tls.Server(raw, serverConfig)
				if e = conn.Handshake(); e != nil {
					return
				}
				<-stop
			}()
			ua, err := NewUA(WithUserAgentReliableWriteTimeout(350*time.Millisecond), WithUserAgenTLSConfig(&tls.Config{InsecureSkipVerify: true}))
			require.NoError(t, err)
			defer ua.Close()
			cli, err := NewClient(ua)
			require.NoError(t, err)
			req := buildTCPRequest(t, ln.Addr().String(), bytes.Repeat([]byte("x"), 4<<20))
			req.SetTransport("TLS")
			require.NoError(t, clientRequestBuildReq(cli, req))
			start := time.Now()
			if mode == "transaction" {
				var tx sip.ClientTransaction
				tx, err = cli.TransactionRequest(context.Background(), req)
				if err == nil {
					tx.Terminate()
				}
			} else {
				err = ua.tp.WriteMsg(req)
			}
			require.ErrorIs(t, err, sip.ErrWriteStalled)
			require.Less(t, time.Since(start), 500*time.Millisecond, "write replenished the establishment budget")
		})
	}
}
