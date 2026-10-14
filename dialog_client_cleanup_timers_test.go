//go:build go1.25

package sipgo

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/hujm2023/sipgo/sip"
	"github.com/hujm2023/sipgo/siptest"
	"github.com/stretchr/testify/require"
)

type cleanupRequesterFunc func(context.Context, *sip.Request) (sip.ClientTransaction, error)

func (f cleanupRequesterFunc) Request(ctx context.Context, req *sip.Request) (sip.ClientTransaction, error) {
	return f(ctx, req)
}

// Both real FSM and UA owner switch once from 487's D to the first fork's M.
func TestLateAnswerCleanupFirstForkGetsIndependentTimerM(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ua, err := NewUA()
		require.NoError(t, err)
		defer ua.Close()
		cli, err := NewClient(ua)
		require.NoError(t, err)
		var acks, byes atomic.Int64
		delegate := &siptest.ClientTxRequester{OnRequest: func(req *sip.Request) *sip.Response {
			if req.Method == sip.BYE {
				byes.Add(1)
			}
			return sip.NewResponseFromRequest(req, 200, "OK", nil)
		}}
		cli.TxRequester = cleanupRequesterFunc(func(ctx context.Context, req *sip.Request) (sip.ClientTransaction, error) {
			if req.Method == sip.ACK {
				acks.Add(1)
				return nil, nil
			}
			return delegate.Request(ctx, req)
		})
		session := newCancelTestSession(t, cli, newFakeClientTx())
		key, err := sip.ClientTxKeyMake(session.InviteRequest)
		require.NoError(t, err)
		tx := sip.NewClientTx(key, session.InviteRequest, &recordingConn{}, slog.Default())
		require.NoError(t, tx.Init())
		session.inviteTx = tx
		session.InviteResponse = sip.NewResponseFromRequest(session.InviteRequest, 180, "Ringing", nil)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		done := make(chan error, 1)
		go func() {
			done <- session.WaitAnswerWithCancel(ctx, AnswerOptions{}, InviteCancelPolicy{CancelTimeout: 5 * time.Second, FinalResponseTimeout: sip.Timer_B, CleanupTimeout: 5 * time.Second})
		}()
		synctest.Wait()
		tx.Receive(sip.NewResponseFromRequest(session.InviteRequest, 487, "Terminated", nil))
		synctest.Wait()
		require.ErrorIs(t, <-done, context.Canceled)
		time.Sleep(30 * time.Second)
		late := newLate2xxResponse(session.InviteRequest, "first-fork", "5062")
		tx.Receive(late)
		synctest.Wait()
		require.Equal(t, int64(1), acks.Load())
		require.Equal(t, int64(1), byes.Load())
		time.Sleep(3 * time.Second)
		tx.Receive(late.Clone())
		synctest.Wait()
		select {
		case <-tx.Done():
			t.Error("original TimerD truncated first fork's TimerM")
		default:
		}
		require.Equal(t, int64(2), acks.Load(), "t33 duplicate still needs ACK")
		require.Equal(t, int64(1), byes.Load())
		time.Sleep(28 * time.Second)
		tx.Receive(late.Clone())
		synctest.Wait()
		require.Equal(t, int64(3), acks.Load(), "t61 remains in the first TimerM")
		tx.Receive(newLate2xxResponse(session.InviteRequest, "last-fork", "5063"))
		synctest.Wait()
		require.Equal(t, int64(4), acks.Load())
		require.Equal(t, int64(2), byes.Load())
		time.Sleep(time.Second)
		synctest.Wait()
		select {
		case <-tx.Done():
		default:
			t.Error("duplicate refreshed the original TimerM beyond t62")
		}
		ua.cleanupMu.Lock()
		owners := len(ua.cleanups)
		ua.cleanupMu.Unlock()
		require.Zero(t, owners, "UA protocol owner exited at t62")
	})
}

func TestLateAnswerCleanupAckFailureCannotConfirmBye(t *testing.T) {
	for _, tc := range []struct {
		name         string
		recoverAfter time.Duration
		wantBye      int64
	}{
		{"all failed", 0, 0}, {"recover within original budget", time.Second, 1}, {"recover after original budget", 6 * time.Second, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ua, err := NewUA()
				require.NoError(t, err)
				defer ua.Close()
				cli, err := NewClient(ua)
				require.NoError(t, err)
				ackError := errors.New("fixture ACK not submitted")
				var failACK atomic.Bool
				failACK.Store(true)
				var tries, byes atomic.Int64
				delegate := &siptest.ClientTxRequester{OnRequest: func(req *sip.Request) *sip.Response {
					if req.Method == sip.BYE {
						byes.Add(1)
					}
					return sip.NewResponseFromRequest(req, 200, "OK", nil)
				}}
				cli.TxRequester = cleanupRequesterFunc(func(ctx context.Context, req *sip.Request) (sip.ClientTransaction, error) {
					if req.Method == sip.ACK {
						tries.Add(1)
						if failACK.Load() {
							return nil, ackError
						}
						return nil, nil
					}
					return delegate.Request(ctx, req)
				})
				session := newCancelTestSession(t, cli, newFakeClientTx())
				key, err := sip.ClientTxKeyMake(session.InviteRequest)
				require.NoError(t, err)
				tx := sip.NewClientTx(key, session.InviteRequest, &recordingConn{}, slog.Default())
				require.NoError(t, tx.Init())
				session.inviteTx = tx
				session.InviteResponse = sip.NewResponseFromRequest(session.InviteRequest, 180, "Ringing", nil)
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				done := make(chan error, 1)
				start := time.Now()
				go func() {
					done <- session.WaitAnswerWithCancel(ctx, AnswerOptions{}, InviteCancelPolicy{CancelTimeout: 5 * time.Second, FinalResponseTimeout: sip.Timer_B, CleanupTimeout: 5 * time.Second})
				}()
				synctest.Wait()
				late := newLate2xxResponse(session.InviteRequest, "failed-ack", "5062")
				tx.Receive(late)
				synctest.Wait()
				waitErr := <-done
				require.ErrorIs(t, waitErr, ackError)
				require.ErrorIs(t, waitErr, context.Canceled)
				ua.cleanupMu.Lock()
				var cleanup *lateAnswerCleanup
				for owner := range ua.cleanups {
					cleanup = owner
				}
				ua.cleanupMu.Unlock()
				require.NotNil(t, cleanup, "failed product cleanup still has a protocol owner")
				require.Less(t, time.Since(start), time.Second, "failed ACK must finish initial cleanup without waiting for BYE")
				require.Equal(t, int64(2), tries.Load())
				require.Zero(t, byes.Load(), "two failed ACK writes cannot authorize BYE")
				views := cleanup.dialogViews()
				require.Len(t, views, 1)
				originalDeadline, _ := views[0].ctx.Deadline()
				if tc.recoverAfter > 0 {
					time.Sleep(tc.recoverAfter)
					failACK.Store(false)
					tx.Receive(late.Clone())
					synctest.Wait()
					tx.Receive(late.Clone())
					synctest.Wait()
					require.Equal(t, int64(4), tries.Load(), "protocol retransmits still get ACK after product deadline")
				}
				require.Equal(t, tc.wantBye, byes.Load())
				got, _ := views[0].ctx.Deadline()
				require.Equal(t, originalDeadline, got, "duplicate cannot grant new BYE budget")
				require.ErrorIs(t, cleanup.join(), ackError)
			})
		})
	}
}

// Ending the INVITE response window closes admission, while an already
// registered BYE retains the same five seconds it received at the fork.
func TestLateAnswerCleanupPendingByeOutlivesTimerM(t *testing.T) {
	for _, action := range []string{"final at t64", "deadline at t66", "UA close at t62"} {
		t.Run(action, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ua, err := NewUA()
				require.NoError(t, err)
				defer ua.Close()
				cli, err := NewClient(ua)
				require.NoError(t, err)
				type pendingBye struct {
					req      *sip.Request
					response *siptest.ClientTxResponder
				}
				pending := make(chan pendingBye, 1)
				var acks, byes atomic.Int64
				delegate := &siptest.ClientTxRequesterResponder{OnRequest: func(req *sip.Request, w *siptest.ClientTxResponder) {
					if req.Method == sip.BYE {
						byes.Add(1)
						tag, _ := req.To().Params.Get("tag")
						if tag == "last-fork" {
							pending <- pendingBye{req.Clone(), w}
							return
						}
					}
					w.Receive(sip.NewResponseFromRequest(req, 200, "OK", nil))
				}}
				cli.TxRequester = cleanupRequesterFunc(func(ctx context.Context, req *sip.Request) (sip.ClientTransaction, error) {
					if req.Method == sip.ACK {
						acks.Add(1)
						return nil, nil
					}
					return delegate.Request(ctx, req)
				})
				session := newCancelTestSession(t, cli, newFakeClientTx())
				key, err := sip.ClientTxKeyMake(session.InviteRequest)
				require.NoError(t, err)
				tx := sip.NewClientTx(key, session.InviteRequest, &recordingConn{}, slog.Default())
				require.NoError(t, tx.Init())
				session.inviteTx = tx
				session.InviteResponse = sip.NewResponseFromRequest(session.InviteRequest, 180, "Ringing", nil)
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				waitDone := make(chan error, 1)
				go func() {
					waitDone <- session.WaitAnswerWithCancel(ctx, AnswerOptions{}, InviteCancelPolicy{CancelTimeout: 5 * time.Second, FinalResponseTimeout: sip.Timer_B, CleanupTimeout: 5 * time.Second})
				}()
				synctest.Wait()
				tx.Receive(sip.NewResponseFromRequest(session.InviteRequest, 487, "Terminated", nil))
				synctest.Wait()
				require.ErrorIs(t, <-waitDone, context.Canceled)
				time.Sleep(30 * time.Second)
				tx.Receive(newLate2xxResponse(session.InviteRequest, "first-fork", "5062"))
				synctest.Wait()
				time.Sleep(31 * time.Second)
				last := newLate2xxResponse(session.InviteRequest, "last-fork", "5063")
				tx.Receive(last)
				synctest.Wait()
				held := <-pending
				ua.cleanupMu.Lock()
				var cleanup *lateAnswerCleanup
				for owner := range ua.cleanups {
					cleanup = owner
				}
				ua.cleanupMu.Unlock()
				require.NotNil(t, cleanup)
				view, ok := cleanup.viewOf(last)
				require.True(t, ok)
				deadline, _ := view.ctx.Deadline()
				require.Equal(t, time.Now().Add(5*time.Second), deadline)
				time.Sleep(time.Second)
				synctest.Wait()
				select {
				case <-tx.Done():
				default:
					t.Fatal("original INVITE did not end at t62")
				}
				select {
				case <-view.done:
					t.Errorf("TimerM canceled an independently owned pending BYE: %v", view.error())
				default:
				}
				require.NoError(t, view.ctx.Err(), "product budget remains active after response admission ends")
				beforeACK, beforeBye := acks.Load(), byes.Load()
				cleanup.notify(newLate2xxResponse(session.InviteRequest, "after-M", "5064"))
				synctest.Wait()
				require.Equal(t, beforeACK, acks.Load())
				require.Equal(t, beforeBye, byes.Load(), "closed admission cannot start new dialog tasks")
				switch action {
				case "final at t64":
					time.Sleep(2 * time.Second)
					held.response.Receive(sip.NewResponseFromRequest(held.req, 200, "OK", nil))
					synctest.Wait()
					require.NoError(t, view.error())
				case "deadline at t66":
					time.Sleep(4 * time.Second)
					synctest.Wait()
					require.ErrorIs(t, view.error(), context.DeadlineExceeded)
				case "UA close at t62":
					require.NoError(t, ua.Close())
					synctest.Wait()
					require.ErrorIs(t, view.error(), context.Canceled)
				}
				select {
				case <-view.done:
				default:
					t.Fatal("BYE consumer was not joined")
				}
				ua.cleanupMu.Lock()
				owners := len(ua.cleanups)
				ua.cleanupMu.Unlock()
				require.Zero(t, owners)
				got, _ := view.ctx.Deadline()
				require.Equal(t, deadline, got, "protocol phase end cannot refresh product budget")
			})
		})
	}
}
