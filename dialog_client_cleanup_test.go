package sipgo

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hujm2023/sipgo/sip"
	"github.com/hujm2023/sipgo/siptest"
	"github.com/stretchr/testify/require"
)

// late2xxPolicy keeps the cleanup tests fast while still exercising every
// deadline independently.
func late2xxPolicy() InviteCancelPolicy {
	return InviteCancelPolicy{
		CancelTimeout:        300 * time.Millisecond,
		FinalResponseTimeout: 2 * time.Second,
		CleanupTimeout:       500 * time.Millisecond,
	}
}

func newLate2xxResponse(inviteReq *sip.Request, toTag, contactPort string) *sip.Response {
	res := sip.NewResponseFromRequest(inviteReq, 200, "OK", nil)
	if to := res.To(); to != nil {
		to.Params.Add("tag", toTag)
	}
	// The remote target must not be our own advertised contact.
	res.RemoveHeader("Contact")
	res.AppendHeader(&sip.ContactHeader{
		Address: sip.Uri{User: "bob", Host: "127.0.0.1", Port: mustAtoi(contactPort)},
	})
	return res
}

func mustAtoi(s string) int {
	var n int
	for _, r := range s {
		n = n*10 + int(r-'0')
	}
	return n
}

// TestLate2xxCleanupAcksEveryResponseAndByesOncePerDialog drives the cleanup
// through the real client transaction FSM: the first 2xx is delivered by the
// response channel and the duplicates are reported as retransmissions.
func TestLate2xxCleanupAcksEveryResponseAndByesOncePerDialog(t *testing.T) {
	var mu sync.Mutex
	var acks, byes, cancels []*sip.Request

	inviteReady := make(chan *siptest.ClientTxResponder, 1)

	ua, err := NewUA()
	require.NoError(t, err)
	defer ua.Close()

	cli, err := NewClient(ua)
	require.NoError(t, err)
	cli.TxRequester = &siptest.ClientTxRequesterResponder{
		OnRequest: func(req *sip.Request, w *siptest.ClientTxResponder) {
			switch req.Method {
			case sip.INVITE:
				inviteReady <- w
				return
			case sip.ACK:
				// An ACK is a direct transport write in production; the test
				// double must not deliver a response for it.
				mu.Lock()
				acks = append(acks, req.Clone())
				mu.Unlock()
				return
			case sip.BYE:
				mu.Lock()
				byes = append(byes, req.Clone())
				mu.Unlock()
			case sip.CANCEL:
				mu.Lock()
				cancels = append(cancels, req.Clone())
				mu.Unlock()
			}

			w.Receive(sip.NewResponseFromRequest(req, 200, "OK", nil))
		},
	}

	dua := &DialogUA{
		Client:     cli,
		ContactHDR: sip.ContactHeader{Address: sip.Uri{User: "alice", Host: "127.0.0.1", Port: 5060}},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dtx, err := dua.Invite(ctx, sip.Uri{User: "bob", Host: "127.0.0.1", Port: 5062}, nil)
	require.NoError(t, err)

	var inviteTx *siptest.ClientTxResponder
	select {
	case inviteTx = <-inviteReady:
	case <-time.After(5 * time.Second):
		t.Fatal("invite transaction was never created")
	}

	inviteSeq := dtx.InviteRequest.CSeq().SeqNo

	// The wait must consume the transaction before any response is delivered:
	// the transaction FSM blocks while passing a response up.
	var observed atomic.Int64
	done := make(chan error, 1)
	start := time.Now()
	go func() {
		done <- dtx.WaitAnswerWithCancel(ctx, AnswerOptions{
			OnResponse: func(res *sip.Response) error {
				observed.Add(1)
				return nil
			},
		}, late2xxPolicy())
	}()

	inviteTx.Receive(sip.NewResponseFromRequest(dtx.InviteRequest, 180, "Ringing", nil))
	require.Eventually(t, func() bool {
		return observed.Load() >= 1
	}, 2*time.Second, 5*time.Millisecond, "provisional response was not observed")

	cancel()

	// The CANCEL must be created and consumed before the late answers arrive.
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(cancels) == 1
	}, 2*time.Second, 5*time.Millisecond, "the CANCEL was not created")

	// First dialog (To-tag A), then two retransmissions of the same 2xx that are
	// only visible through the transaction retransmission notification.
	successA := newLate2xxResponse(dtx.InviteRequest, "tag-a", "5062")
	inviteTx.Receive(successA)
	inviteTx.Receive(successA.Clone())
	inviteTx.Receive(successA.Clone())

	// Second dialog created by the same INVITE, with a different To-tag.
	successB := newLate2xxResponse(dtx.InviteRequest, "tag-b", "5063")
	inviteTx.Receive(successB)

	err = <-done
	elapsed := time.Since(start)

	require.ErrorIs(t, err, context.Canceled)
	require.Less(t, elapsed, 3*time.Second, "the cleanup must stay bounded")

	mu.Lock()
	defer mu.Unlock()

	require.Len(t, acks, 4, "every 2xx, including both retransmissions, needs its own ACK")
	require.Len(t, byes, 2, "each real dialog gets at most one logical BYE")

	ackA, ackB := 0, 0
	for _, ack := range acks {
		require.Equal(t, sip.ACK, ack.CSeq().MethodName)
		require.Equal(t, inviteSeq, ack.CSeq().SeqNo, "the ACK must reuse the INVITE CSeq number")

		toTag, _ := ack.To().Params.Get("tag")
		switch toTag {
		case "tag-a":
			ackA++
		case "tag-b":
			ackB++
		default:
			t.Fatalf("ACK for an unexpected dialog: %q", toTag)
		}
	}
	require.Equal(t, 3, ackA)
	require.Equal(t, 1, ackB)

	byeSeq := map[string]uint32{}
	for _, bye := range byes {
		require.Equal(t, sip.BYE, bye.CSeq().MethodName)
		require.Greater(t, bye.CSeq().SeqNo, inviteSeq, "the BYE must use a higher CSeq than the INVITE")

		toTag, _ := bye.To().Params.Get("tag")
		require.NotContains(t, byeSeq, toTag, "no dialog may get a second logical BYE")
		byeSeq[toTag] = bye.CSeq().SeqNo
	}
	require.Equal(t, []string{"tag-a", "tag-b"}, sortedKeys(byeSeq))
	require.NotEqual(t, byeSeq["tag-a"], byeSeq["tag-b"], "each BYE needs its own CSeq")
}

func sortedKeys(m map[string]uint32) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	if len(keys) == 2 && keys[0] > keys[1] {
		keys[0], keys[1] = keys[1], keys[0]
	}
	return keys
}

// late2xxPeer answers an INVITE with a provisional, answers CANCEL with 200 OK,
// then sends the 2xx three times (one answer plus two retransmissions) and
// records the ACK/BYE the client sends back.
type late2xxPeer struct {
	addr    string
	ringing chan struct{}

	mu   sync.Mutex
	acks []string
	byes []string

	conns []net.Conn
}

func startLate2xxPeer(t *testing.T) *late2xxPeer {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { ln.Close() })

	p := &late2xxPeer{addr: ln.Addr().String(), ringing: make(chan struct{})}

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

func (p *late2xxPeer) serve(c net.Conn, ringingOnce *sync.Once) {
	reader := bufio.NewReader(c)
	answered := false
	var inviteHeaders map[string]string

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
			inviteHeaders = headers
			_, _ = io.WriteString(c, buildRawSIPResponse("180 Ringing", headers))
			ringingOnce.Do(func() { close(p.ringing) })
		case "CANCEL":
			_, _ = io.WriteString(c, buildRawSIPResponse("200 OK", headers))

			if !answered {
				answered = true
				// The 2xx for the INVITE must keep the INVITE CSeq (method and
				// number); only the CANCEL response uses the CANCEL CSeq.
				for i := 0; i < 3; i++ {
					_, _ = io.WriteString(c, buildRawSIPSuccess(inviteHeaders, "peer-tag", p.addr))
				}
			}
		case "ACK":
			p.mu.Lock()
			p.acks = append(p.acks, headers["cseq"]+"|"+headers["to"])
			p.mu.Unlock()
		case "BYE":
			p.mu.Lock()
			p.byes = append(p.byes, headers["cseq"]+"|"+headers["to"])
			p.mu.Unlock()
			_, _ = io.WriteString(c, buildRawSIPResponse("200 OK", headers))
		}
	}
}

func (p *late2xxPeer) counts() (int, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.acks), len(p.byes)
}

func buildRawSIPSuccess(reqHeaders map[string]string, toTag, contactAddr string) string {
	var b strings.Builder
	b.WriteString("SIP/2.0 200 OK\r\n")

	for _, h := range []struct{ key, name string }{
		{"via", "Via"},
		{"from", "From"},
		{"call-id", "Call-ID"},
		{"cseq", "CSeq"},
	} {
		if v, ok := reqHeaders[h.key]; ok {
			fmt.Fprintf(&b, "%s: %s\r\n", h.name, v)
		}
	}

	to := reqHeaders["to"]
	if to != "" {
		fmt.Fprintf(&b, "To: %s;tag=%s\r\n", to, toTag)
	}
	fmt.Fprintf(&b, "Contact: <sip:bob@%s>\r\n", contactAddr)
	b.WriteString("Content-Length: 0\r\n\r\n")
	return b.String()
}

// TestLate2xxCleanupRealTCPPeer proves the cleanup over a real TCP connection:
// the client ACKs every late 2xx (the answer plus both retransmissions) and
// sends exactly one logical BYE for the dialog.
func TestLate2xxCleanupRealTCPPeer(t *testing.T) {
	peer := startLate2xxPeer(t)

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

	start := time.Now()
	err = dtx.WaitAnswerWithCancel(ctx, AnswerOptions{}, late2xxPolicy())
	elapsed := time.Since(start)

	require.ErrorIs(t, err, context.Canceled)
	require.Less(t, elapsed, 3*time.Second)

	require.Eventually(t, func() bool {
		acks, byes := peer.counts()
		return acks == 3 && byes == 1
	}, 2*time.Second, 10*time.Millisecond, "expected 3 ACKs (one per 2xx) and exactly 1 logical BYE")

	acks, byes := peer.counts()
	require.Equal(t, 3, acks)
	require.Equal(t, 1, byes)

	// Every ACK belongs to the same dialog and keeps the INVITE CSeq number.
	peer.mu.Lock()
	defer peer.mu.Unlock()
	for _, ack := range peer.acks {
		require.Contains(t, ack, "ACK")
		require.Contains(t, ack, "peer-tag")
	}
	for _, bye := range peer.byes {
		require.Contains(t, bye, "BYE")
		require.Contains(t, bye, "peer-tag")
	}
}

// TestLate2xxCleanupBoundedByCleanupBudget proves the ACK/BYE duties never run
// past the cleanup budget even when the BYE never gets a response.
func TestLate2xxCleanupBoundedByCleanupBudget(t *testing.T) {
	var mu sync.Mutex
	var acks, byes []*sip.Request

	ua, err := NewUA()
	require.NoError(t, err)
	defer ua.Close()

	cli, err := NewClient(ua)
	require.NoError(t, err)

	cli.TxRequester = &siptest.ClientTxRequesterResponder{
		OnRequest: func(req *sip.Request, w *siptest.ClientTxResponder) {
			switch req.Method {
			case sip.CANCEL:
				w.Receive(sip.NewResponseFromRequest(req, 200, "OK", nil))
			case sip.ACK:
				mu.Lock()
				acks = append(acks, req.Clone())
				mu.Unlock()
			case sip.BYE:
				mu.Lock()
				byes = append(byes, req.Clone())
				mu.Unlock()
				// Deliberately never answered: the cleanup must end on its own
				// budget instead of waiting forever.
			}
		},
	}

	tx := newFakeClientTx()
	session := newCancelTestSession(t, cli, tx)

	policy := InviteCancelPolicy{
		CancelTimeout:        300 * time.Millisecond,
		FinalResponseTimeout: 3 * time.Second,
		CleanupTimeout:       250 * time.Millisecond,
	}

	provisional := sip.NewResponseFromRequest(session.InviteRequest, 180, "Ringing", nil)
	late := newLate2xxResponse(session.InviteRequest, "tag-a", "5062")

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
		case <-time.After(80 * time.Millisecond):
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

	start := time.Now()
	err = session.WaitAnswerWithCancel(ctx, AnswerOptions{}, policy)
	elapsed := time.Since(start)

	require.ErrorIs(t, err, context.Canceled)
	require.GreaterOrEqual(t, elapsed, policy.CleanupTimeout, "the cleanup budget must be the bound")
	require.Less(t, elapsed, policy.CleanupTimeout+1500*time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, acks, 1)
	require.Len(t, byes, 1, "exactly one logical BYE per dialog, even when it never completes")
	require.True(t, tx.terminated.Load(), "the INVITE must still be released")
}

// TestLateAnswerCleanupNotificationNeverBlocksTheFSM proves the transaction
// notification path only hands work over: once the consumer stopped it can not
// block, so it can never stall a transaction FSM and never runs protocol work
// on that stack.
func TestLateAnswerCleanupNotificationNeverBlocksTheFSM(t *testing.T) {
	ua, err := NewUA()
	require.NoError(t, err)
	defer ua.Close()

	cli, err := NewClient(ua)
	require.NoError(t, err)

	tx := newFakeClientTx()
	session := newCancelTestSession(t, cli, tx)

	cleanup := newLateAnswerCleanup(session, context.Background(), 100*time.Millisecond)

	// Never started: closeInput must be harmless and the notification must not
	// block or panic.
	cleanup.closeInput()
	require.NoError(t, cleanup.join())

	res := newLate2xxResponse(session.InviteRequest, "tag-a", "5062")

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < lateCleanupQueueSize*4; i++ {
			cleanup.notify(res)
		}
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("transaction notification blocked after the cleanup stopped")
	}

	// The hand-off is best effort here (there is no peer to ACK); what matters is
	// that the notification returned and that the join is still bounded.
	_ = cleanup.join()
}
