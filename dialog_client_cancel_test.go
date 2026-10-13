package sipgo

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hujm2023/sipgo/sip"
	"github.com/stretchr/testify/require"
)

// fakeClientTx is an in-memory sip.ClientTransaction: it lets the cancel
// teardown be driven by a synthetic peer without any network I/O.
type fakeClientTx struct {
	responses chan *sip.Response
	done      chan struct{}

	closeOnce  sync.Once
	terminated atomic.Bool

	mu  sync.Mutex
	err error
}

func newFakeClientTx() *fakeClientTx {
	return &fakeClientTx{
		responses: make(chan *sip.Response),
		done:      make(chan struct{}),
	}
}

func (t *fakeClientTx) Responses() <-chan *sip.Response { return t.responses }
func (t *fakeClientTx) Done() <-chan struct{}           { return t.done }

func (t *fakeClientTx) Err() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.err
}

func (t *fakeClientTx) OnTerminate(f sip.FnTxTerminate) bool     { return false }
func (t *fakeClientTx) OnRetransmission(f sip.FnTxResponse) bool { return false }

func (t *fakeClientTx) Terminate() {
	t.closeOnce.Do(func() {
		t.mu.Lock()
		t.err = sip.ErrTransactionTerminated
		t.mu.Unlock()
		t.terminated.Store(true)
		close(t.done)
	})
}

func newCancelTestSession(t *testing.T, client *Client, tx sip.ClientTransaction) *DialogClientSession {
	t.Helper()

	req := sip.NewRequest(sip.INVITE, sip.Uri{User: "bob", Host: "127.0.0.1", Port: 5060})
	req.SetTransport("TCP")
	require.NoError(t, clientRequestBuildReq(client, req))

	session := &DialogClientSession{
		Dialog:   Dialog{InviteRequest: req},
		inviteTx: tx,
		UA: &DialogUA{
			Client:     client,
			ContactHDR: sip.ContactHeader{Address: sip.Uri{User: "alice", Host: "127.0.0.1", Port: 5060}},
		},
	}
	session.Dialog.Init()
	return session
}

func newSuccessResponse(req *sip.Request) *sip.Response {
	res := sip.NewResponseFromRequest(req, 200, "OK", nil)
	// A real 2xx carries the remote tag that identifies the dialog.
	if to := res.To(); to != nil {
		to.Params.Add("tag", "remote-tag")
	}
	return res
}

func newCancelCountingClient(t *testing.T, cancels *atomic.Int64) *Client {
	t.Helper()

	return testClient(t, func(req *sip.Request) *sip.Response {
		if req.Method == sip.CANCEL {
			cancels.Add(1)
		}
		return sip.NewResponseFromRequest(req, 200, "OK", nil)
	})
}

// sendProvisionalForever pushes provisional responses until stop is closed, so
// a test can prove they do not extend any deadline.
func sendProvisionalForever(tx *fakeClientTx, res *sip.Response, every time.Duration, delivered *atomic.Int64, stop <-chan struct{}) {
	go func() {
		ticker := time.NewTicker(every)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
			}

			select {
			case tx.responses <- res:
				delivered.Add(1)
			case <-stop:
				return
			}
		}
	}()
}

// TestWaitAnswerWithCancelFixedDeadlineWithRepeated1xx is the UAC-01 regression:
// a peer that keeps sending provisional responses must not be able to extend the
// finishing deadline.
func TestWaitAnswerWithCancelFixedDeadlineWithRepeated1xx(t *testing.T) {
	var cancels atomic.Int64
	client := newCancelCountingClient(t, &cancels)

	tx := newFakeClientTx()
	session := newCancelTestSession(t, client, tx)

	policy := InviteCancelPolicy{
		CancelTimeout:        300 * time.Millisecond,
		FinalResponseTimeout: 400 * time.Millisecond,
		CleanupTimeout:       200 * time.Millisecond,
	}

	provisional := sip.NewResponseFromRequest(session.InviteRequest, 180, "Ringing", nil)

	stop := make(chan struct{})
	defer close(stop)

	var delivered atomic.Int64
	sendProvisionalForever(tx, provisional, 20*time.Millisecond, &delivered, stop)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the caller already decided to stop waiting

	start := time.Now()
	err := session.WaitAnswerWithCancel(ctx, AnswerOptions{}, policy)
	elapsed := time.Since(start)

	require.ErrorIs(t, err, context.Canceled)
	require.GreaterOrEqual(t, elapsed, policy.FinalResponseTimeout,
		"the fixed final response deadline must be honored")
	require.Less(t, elapsed, 1500*time.Millisecond,
		"repeated 1xx must not extend the deadline")
	require.GreaterOrEqual(t, delivered.Load(), int64(5),
		"the peer really kept sending provisional responses")

	require.Equal(t, int64(1), cancels.Load(), "exactly one CANCEL may be created")
	require.True(t, tx.terminated.Load(), "the INVITE transaction must be released")

	select {
	case <-tx.Done():
	default:
		t.Fatal("INVITE transaction Done must be closed after the teardown")
	}
}

// TestWaitAnswerWithCancelWithoutProvisionalSendsNoCancel covers the RFC 3261
// 9.1 rule: without a provisional response no CANCEL is legal, so the cancel
// budget expires and the INVITE keeps its own deadline.
func TestWaitAnswerWithCancelWithoutProvisionalSendsNoCancel(t *testing.T) {
	var cancels atomic.Int64
	client := newCancelCountingClient(t, &cancels)

	tx := newFakeClientTx()
	session := newCancelTestSession(t, client, tx)

	policy := InviteCancelPolicy{
		CancelTimeout:        150 * time.Millisecond,
		FinalResponseTimeout: 350 * time.Millisecond,
		CleanupTimeout:       150 * time.Millisecond,
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	err := session.WaitAnswerWithCancel(ctx, AnswerOptions{}, policy)
	elapsed := time.Since(start)

	require.ErrorIs(t, err, context.Canceled)
	require.ErrorContains(t, err, "no provisional response before cancel deadline")
	require.Zero(t, cancels.Load(), "no CANCEL may be created without a provisional response")

	require.GreaterOrEqual(t, elapsed, policy.FinalResponseTimeout)
	require.Less(t, elapsed, 1200*time.Millisecond)
	require.True(t, tx.terminated.Load())
}

// TestWaitAnswerWithCancelLateSuccessEndsBeforeDeadline proves a late final
// response is still observed by the caller callback, ends the INVITE consumer
// early and is kept on the session for later cleanup.
func TestWaitAnswerWithCancelLateSuccessEndsBeforeDeadline(t *testing.T) {
	var cancels atomic.Int64
	client := newCancelCountingClient(t, &cancels)

	tx := newFakeClientTx()
	session := newCancelTestSession(t, client, tx)

	policy := InviteCancelPolicy{
		CancelTimeout:        300 * time.Millisecond,
		FinalResponseTimeout: 3 * time.Second,
		CleanupTimeout:       200 * time.Millisecond,
	}

	provisional := sip.NewResponseFromRequest(session.InviteRequest, 180, "Ringing", nil)
	late := newSuccessResponse(session.InviteRequest)

	stop := make(chan struct{})
	defer close(stop)

	go func() {
		select {
		case <-time.After(20 * time.Millisecond):
		case <-stop:
			return
		}
		select {
		case tx.responses <- provisional:
		case <-stop:
			return
		}

		select {
		case <-time.After(100 * time.Millisecond):
		case <-stop:
			return
		}
		select {
		case tx.responses <- late:
		case <-stop:
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var observed atomic.Int64
	start := time.Now()
	err := session.WaitAnswerWithCancel(ctx, AnswerOptions{
		OnResponse: func(res *sip.Response) error {
			observed.Add(1)
			return nil
		},
	}, policy)
	elapsed := time.Since(start)

	require.ErrorIs(t, err, context.Canceled)
	require.Less(t, elapsed, 1500*time.Millisecond, "a late final ends the wait before the fixed deadline")
	require.GreaterOrEqual(t, elapsed, 100*time.Millisecond)
	require.GreaterOrEqual(t, observed.Load(), int64(2), "the observer must see the provisional and the late 2xx")
	require.Equal(t, int64(1), cancels.Load())

	require.NotNil(t, session.InviteResponse)
	require.True(t, session.InviteResponse.IsSuccess())
	require.True(t, tx.terminated.Load())
}

// TestWaitAnswerWithCancelObservesCallbackErrorWithoutLosingCleanup proves a
// failing caller observer can not remove the cleanup responsibility.
func TestWaitAnswerWithCancelObservesCallbackErrorWithoutLosingCleanup(t *testing.T) {
	var cancels atomic.Int64
	client := newCancelCountingClient(t, &cancels)

	tx := newFakeClientTx()
	session := newCancelTestSession(t, client, tx)

	policy := InviteCancelPolicy{
		CancelTimeout:        300 * time.Millisecond,
		FinalResponseTimeout: 300 * time.Millisecond,
		CleanupTimeout:       150 * time.Millisecond,
	}

	provisional := sip.NewResponseFromRequest(session.InviteRequest, 180, "Ringing", nil)
	stop := make(chan struct{})
	defer close(stop)

	var delivered atomic.Int64
	sendProvisionalForever(tx, provisional, 20*time.Millisecond, &delivered, stop)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	observerErr := fmt.Errorf("observer failed")
	err := session.WaitAnswerWithCancel(ctx, AnswerOptions{
		OnResponse: func(res *sip.Response) error { return observerErr },
	}, policy)

	require.ErrorIs(t, err, context.Canceled)
	require.ErrorIs(t, err, observerErr)
	require.Equal(t, int64(1), cancels.Load(), "the CANCEL must still be created and consumed")
	require.True(t, tx.terminated.Load())
}

// TestWaitAnswerWithCancelSuccessPathUnchanged keeps the normal answer path
// identical to WaitAnswer: established dialog, no CANCEL, transaction still
// owned by the caller.
func TestWaitAnswerWithCancelSuccessPathUnchanged(t *testing.T) {
	var cancels atomic.Int64
	client := newCancelCountingClient(t, &cancels)

	tx := newFakeClientTx()
	session := newCancelTestSession(t, client, tx)

	success := newSuccessResponse(session.InviteRequest)

	go func() { tx.responses <- success }()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	policy := InviteCancelPolicy{
		CancelTimeout:        time.Second,
		FinalResponseTimeout: time.Second,
		CleanupTimeout:       time.Second,
	}

	require.NoError(t, session.WaitAnswerWithCancel(ctx, AnswerOptions{}, policy))
	require.Equal(t, sip.DialogStateEstablished, session.LoadState())
	require.NotEmpty(t, session.ID)
	require.Zero(t, cancels.Load())
	require.False(t, tx.terminated.Load(), "the caller still owns the INVITE transaction")
}

func TestWaitAnswerWithCancelRejectsInvalidPolicy(t *testing.T) {
	client := testClient(t, func(req *sip.Request) *sip.Response {
		return sip.NewResponseFromRequest(req, 200, "OK", nil)
	})
	tx := newFakeClientTx()
	session := newCancelTestSession(t, client, tx)

	bad := []InviteCancelPolicy{
		{CancelTimeout: 0, FinalResponseTimeout: time.Second, CleanupTimeout: time.Second},
		{CancelTimeout: time.Second, FinalResponseTimeout: 0, CleanupTimeout: time.Second},
		{CancelTimeout: time.Second, FinalResponseTimeout: time.Second, CleanupTimeout: -time.Second},
	}

	for _, policy := range bad {
		require.ErrorIs(t, session.WaitAnswerWithCancel(context.Background(), AnswerOptions{}, policy), ErrInviteCancelPolicy)
	}

	// A rejected policy must not have touched the transaction.
	require.False(t, tx.terminated.Load())
}

// cancelablePeer is a raw TCP SIP peer that answers an INVITE with a single
// provisional response, answers CANCEL with 200 OK, and never sends a final
// response for the INVITE.
type cancelablePeer struct {
	addr     string
	ringing  chan struct{}
	cancelCh chan struct{}

	mu    sync.Mutex
	conns []net.Conn
}

func startCancelablePeer(t *testing.T) *cancelablePeer {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { ln.Close() })

	p := &cancelablePeer{
		addr:     ln.Addr().String(),
		ringing:  make(chan struct{}),
		cancelCh: make(chan struct{}, 4),
	}

	var ringingOnce sync.Once

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			p.mu.Lock()
			p.conns = append(p.conns, c)
			p.mu.Unlock()
			go p.serve(c, &ringingOnce)
		}
	}()

	t.Cleanup(func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		for _, c := range p.conns {
			_ = c.Close()
		}
	})

	return p
}

func (p *cancelablePeer) serve(c net.Conn, ringingOnce *sync.Once) {
	reader := bufio.NewReader(c)
	for {
		startLine, headers, err := readRawSIPRequest(reader)
		if err != nil {
			return
		}

		fields := strings.Fields(startLine)
		method := ""
		if len(fields) > 0 {
			method = fields[0]
		}

		switch method {
		case "INVITE":
			_, _ = io.WriteString(c, buildRawSIPResponse("180 Ringing", headers))
			ringingOnce.Do(func() { close(p.ringing) })
		case "CANCEL":
			_, _ = io.WriteString(c, buildRawSIPResponse("200 OK", headers))
			select {
			case p.cancelCh <- struct{}{}:
			default:
			}
		}
	}
}

func readRawSIPRequest(r *bufio.Reader) (string, map[string]string, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return "", nil, err
	}

	startLine := strings.TrimRight(line, "\r\n")
	headers := make(map[string]string)
	contentLength := 0

	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return "", nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}

		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		value = strings.TrimSpace(value)
		headers[key] = value
		if key == "content-length" {
			contentLength, _ = strconv.Atoi(value)
		}
	}

	if contentLength > 0 {
		body := make([]byte, contentLength)
		if _, err := io.ReadFull(r, body); err != nil {
			return "", nil, err
		}
	}

	return startLine, headers, nil
}

func buildRawSIPResponse(statusLine string, reqHeaders map[string]string) string {
	var b strings.Builder
	b.WriteString("SIP/2.0 " + statusLine + "\r\n")

	for _, h := range []struct{ key, name string }{
		{"via", "Via"},
		{"from", "From"},
		{"to", "To"},
		{"call-id", "Call-ID"},
		{"cseq", "CSeq"},
	} {
		if v, ok := reqHeaders[h.key]; ok {
			fmt.Fprintf(&b, "%s: %s\r\n", h.name, v)
		}
	}

	b.WriteString("Content-Length: 0\r\n\r\n")
	return b.String()
}

// TestWaitAnswerWithCancelRealTCPPeer runs the teardown against a real TCP peer
// that never sends a final response for the INVITE.
func TestWaitAnswerWithCancelRealTCPPeer(t *testing.T) {
	peer := startCancelablePeer(t)

	ua, err := NewUA(WithUserAgentHostname("127.0.0.1"))
	require.NoError(t, err)
	defer ua.Close()

	cli, err := NewClient(ua)
	require.NoError(t, err)

	dua := &DialogUA{
		Client:     cli,
		ContactHDR: sip.ContactHeader{Address: sip.Uri{User: "alice", Host: "127.0.0.1", Port: 5060}},
	}

	host, port, err := sip.ParseAddr(peer.addr)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dtx, err := dua.Invite(ctx, sip.Uri{
		User:      "bob",
		Host:      host,
		Port:      port,
		UriParams: sip.HeaderParams{{K: "transport", V: "tcp"}},
	}, nil)
	require.NoError(t, err)

	go func() {
		select {
		case <-peer.ringing:
		case <-time.After(5 * time.Second):
		}
		cancel()
	}()

	policy := InviteCancelPolicy{
		CancelTimeout:        500 * time.Millisecond,
		FinalResponseTimeout: 900 * time.Millisecond,
		CleanupTimeout:       300 * time.Millisecond,
	}

	start := time.Now()
	err = dtx.WaitAnswerWithCancel(ctx, AnswerOptions{}, policy)
	elapsed := time.Since(start)

	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled)
	require.GreaterOrEqual(t, elapsed, policy.FinalResponseTimeout)
	require.Less(t, elapsed, 5*time.Second)

	select {
	case <-peer.cancelCh:
	case <-time.After(3 * time.Second):
		t.Fatal("the peer never received a CANCEL")
	}

	select {
	case <-dtx.inviteTx.Done():
	default:
		t.Fatal("the INVITE transaction must be released when the teardown returns")
	}
}
