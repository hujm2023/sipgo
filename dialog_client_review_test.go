package sipgo

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hujm2023/sipgo/sip"
	"github.com/hujm2023/sipgo/siptest"
	"github.com/stretchr/testify/require"
)

// Hold CANCEL submission while the independent INVITE consumer receives its
// final response. A 481 is only explained by the race when that final exists.
func TestWaitAnswerWithCancelFinalResponseRace(t *testing.T) {
	for _, tc := range []struct {
		name        string
		inviteFinal int
		cancelFinal int
		cancelError bool
	}{
		{"success_then_481", 200, 481, false},
		{"487_then_481", 487, 481, false},
		{"481_without_final", 0, 481, true},
		{"success_then_500", 200, 500, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx := newFakeClientTx()
			var session *DialogClientSession
			var cancels, acks, byes atomic.Int64
			client := testClient(t, func(req *sip.Request) *sip.Response {
				switch req.Method {
				case sip.CANCEL:
					cancels.Add(1)
					if tc.inviteFinal != 0 {
						res := newLate2xxResponse(session.InviteRequest, "race", "5062")
						if tc.inviteFinal != 200 {
							res = sip.NewResponseFromRequest(session.InviteRequest, tc.inviteFinal, "Terminated", nil)
						}
						select {
						case tx.responses <- res:
						case <-time.After(time.Second):
							t.Error("INVITE consumer did not accept the racing final")
						}
					}
					return sip.NewResponseFromRequest(req, tc.cancelFinal, "Cancel result", nil)
				case sip.ACK:
					acks.Add(1)
				case sip.BYE:
					byes.Add(1)
				}
				return sip.NewResponseFromRequest(req, 200, "OK", nil)
			})
			t.Cleanup(func() { require.NoError(t, client.UserAgent.Close()) })
			session = newCancelTestSession(t, client, tx)
			session.InviteResponse = sip.NewResponseFromRequest(session.InviteRequest, 180, "Ringing", nil)
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			err := session.WaitAnswerWithCancel(ctx, AnswerOptions{}, InviteCancelPolicy{
				CancelTimeout: time.Second, FinalResponseTimeout: 100 * time.Millisecond, CleanupTimeout: time.Second,
			})
			require.ErrorIs(t, err, context.Canceled)
			require.Equal(t, tc.cancelError, strings.Contains(err.Error(), "cancel failed"), "CANCEL error attribution: %v", err)
			require.Equal(t, int64(1), cancels.Load())
			if tc.inviteFinal == 200 {
				require.Equal(t, int64(1), acks.Load())
				require.Equal(t, int64(1), byes.Load())
			}
		})
	}
}

// A fork may arrive before the CANCEL consumer has joined, even though 487
// already completed the original consumer. Its BYE is UA-owned and cannot
// retroactively extend the caller's wait.
func TestWaitAnswerWithCancel487BeforeForkedSuccess(t *testing.T) {
	inviteReady := make(chan *siptest.ClientTxResponder, 1)
	cancelReady := make(chan *siptest.ClientTxResponder, 1)
	byeReady := make(chan *siptest.ClientTxResponder, 1)
	ackReceived := make(chan struct{}, 4)
	finalObserved := make(chan struct{})
	var byes atomic.Int64
	client := testClientResponder(t, func(req *sip.Request, w *siptest.ClientTxResponder) {
		switch req.Method {
		case sip.INVITE:
			inviteReady <- w
		case sip.CANCEL:
			cancelReady <- w
		case sip.ACK:
			ackReceived <- struct{}{}
		case sip.BYE:
			byes.Add(1)
			byeReady <- w
		}
	})
	t.Cleanup(func() { require.NoError(t, client.UserAgent.Close()) })
	dua := &DialogUA{Client: client, ContactHDR: sip.ContactHeader{Address: sip.Uri{Host: "127.0.0.1", Port: 5060}}}
	session, err := dua.Invite(context.Background(), sip.Uri{Host: "127.0.0.1", Port: 5062}, nil)
	require.NoError(t, err)
	invite := <-inviteReady
	session.InviteResponse = sip.NewResponseFromRequest(session.InviteRequest, 180, "Ringing", nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan error, 1)
	go func() {
		done <- session.WaitAnswerWithCancel(ctx, AnswerOptions{OnResponse: func(res *sip.Response) error {
			if res.StatusCode == 487 {
				close(finalObserved)
			}
			return nil
		}}, InviteCancelPolicy{CancelTimeout: time.Second, FinalResponseTimeout: 2 * time.Second, CleanupTimeout: 5 * time.Second})
	}()
	var cancelResponse *siptest.ClientTxResponder
	select {
	case cancelResponse = <-cancelReady:
	case <-time.After(time.Second):
		t.Fatal("CANCEL did not start")
	}
	invite.Receive(sip.NewResponseFromRequest(session.InviteRequest, 487, "Terminated", nil))
	select {
	case <-finalObserved:
	case <-time.After(time.Second):
		t.Fatal("487 was not observed")
	}
	late := newLate2xxResponse(session.InviteRequest, "fork", "5062")
	invite.Receive(late)
	select {
	case <-byeReady:
	case <-time.After(time.Second):
		t.Fatal("forked 2xx was not compensated")
	}
	// The CANCEL response uses the original identity with method CANCEL.
	cancelReq := newCancelRequest(session.InviteRequest)
	cancelResponse.Receive(sip.NewResponseFromRequest(cancelReq, 200, "OK", nil))
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("a fork after 487 incorrectly extended the product wait")
	}
	invite.Receive(late.Clone())
	for i := 0; i < 2; i++ {
		select {
		case <-ackReceived:
		case <-time.After(time.Second):
			t.Fatal("missing original or duplicate ACK")
		}
	}
	require.NoError(t, client.UserAgent.Close())
	require.Equal(t, int64(1), byes.Load(), "one compensation BYE per dialog")
}

type cleanupReviewLogHandler struct {
	records chan slog.Record
}

func (h *cleanupReviewLogHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *cleanupReviewLogHandler) Handle(_ context.Context, r slog.Record) error {
	h.records <- r.Clone()
	return nil
}
func (h *cleanupReviewLogHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *cleanupReviewLogHandler) WithGroup(string) slog.Handler      { return h }

// Close is held in the join of an admitted BYE. A fresh late 2xx then arrives
// after shutdown fenced admission, but before Close has returned.
func TestLateAnswerCleanupCloseRaceLogsRejectedSuccess(t *testing.T) {
	byeStarted := make(chan struct{})
	releaseBye := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(releaseBye) }) })
	var acks, byes atomic.Int64
	client := testClient(t, func(req *sip.Request) *sip.Response {
		switch req.Method {
		case sip.ACK:
			acks.Add(1)
		case sip.BYE:
			byes.Add(1)
			close(byeStarted)
			<-releaseBye
		}
		return sip.NewResponseFromRequest(req, 200, "OK", nil)
	})
	handler := &cleanupReviewLogHandler{records: make(chan slog.Record, 16)}
	client.log = slog.New(handler)
	tx := newFakeClientTx()
	session := newCancelTestSession(t, client, tx)
	cleanup := newLateAnswerCleanup(session, context.Background(), time.Second)
	cleanup.invite = tx
	cleanup.enqueue(newLate2xxResponse(session.InviteRequest, "admitted", "5062"))
	select {
	case <-byeStarted:
	case <-time.After(time.Second):
		t.Fatal("initial BYE did not start")
	}
	closed := make(chan error, 1)
	go func() { closed <- client.UserAgent.Close() }()
	select {
	case <-cleanup.ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("Close did not cancel the admitted owner")
	}
	fresh := newLateAnswerCleanup(session, context.Background(), time.Second)
	fresh.notify(newLate2xxResponse(session.InviteRequest, "closing", "5063"))
	select {
	case r := <-handler.records:
		require.Equal(t, slog.LevelWarn, r.Level)
		require.Equal(t, "late answer cleanup admission stopped", r.Message)
	case <-time.After(time.Second):
		t.Fatal("late 2xx was silently discarded during Close")
	}
	select {
	case <-closed:
		t.Fatal("Close returned before the admitted BYE actually exited")
	default:
	}
	releaseOnce.Do(func() { close(releaseBye) })
	select {
	case err := <-closed:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("Close failed to join the admitted BYE")
	}
	require.NoError(t, fresh.join())
	require.True(t, tx.terminated.Load())
	require.Equal(t, int64(1), acks.Load())
	require.Equal(t, int64(1), byes.Load())
}
