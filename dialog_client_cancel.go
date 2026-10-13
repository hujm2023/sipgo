package sipgo

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/hujm2023/sipgo/sip"
)

// InviteCancelPolicy bounds the explicit cancellation of an initial INVITE
// started with WaitAnswerWithCancel.
//
// The three timeouts are independent on purpose: the CANCEL request has its own
// transaction and consumer, the original INVITE keeps its own fixed
// final-response deadline, and the protocol cleanup triggered by a late answer
// has its own budget. None of them may extend another.
//
// Every field must be positive. WaitAnswerWithCancel rejects an invalid policy
// instead of authorizing an unowned exit of an existing transaction; there is
// no zero-as-unlimited contract.
type InviteCancelPolicy struct {
	// CancelTimeout bounds the CANCEL half as a whole: waiting for a provisional
	// response when none was received yet, submitting the CANCEL request and
	// consuming the CANCEL final response.
	// VoIPGo passes 5s.
	CancelTimeout time.Duration

	// FinalResponseTimeout is measured once, from the moment the cancellation
	// starts, and bounds how long the original INVITE is still consumed
	// afterwards. Repeated 1xx responses never extend it.
	// VoIPGo passes sip.Timer_B.
	FinalResponseTimeout time.Duration

	// CleanupTimeout is shared by the protocol duties the cancel path owns once
	// a final response arrives (a late/duplicate 2xx still needs its ACK and, at
	// most, one logical BYE per real dialog).
	// VoIPGo passes 5s.
	CleanupTimeout time.Duration
}

// ErrInviteCancelPolicy is returned by WaitAnswerWithCancel for a policy with a
// non-positive timeout.
var ErrInviteCancelPolicy = errors.New("invite cancel policy timeouts must be positive")

func (p InviteCancelPolicy) validate() error {
	if p.CancelTimeout <= 0 || p.FinalResponseTimeout <= 0 || p.CleanupTimeout <= 0 {
		return fmt.Errorf("%w: cancel=%s final=%s cleanup=%s",
			ErrInviteCancelPolicy, p.CancelTimeout, p.FinalResponseTimeout, p.CleanupTimeout)
	}
	return nil
}

// WaitAnswerWithCancel waits for the answer like WaitAnswer, but when ctx ends
// while the INVITE is still pending it runs the caller-requested protocol
// teardown under an explicit InviteCancelPolicy instead of the legacy unbounded
// wait.
//
// The caller states "do not continue this call". The library then:
//   - keeps consuming the original INVITE under a fixed final-response deadline
//     that repeated 1xx responses can not extend;
//   - creates at most one CANCEL, only after a provisional and before any final
//     response, with its own transaction, consumer and deadline;
//   - joins both consumers and releases the INVITE transaction before returning.
//
// Normal answers are unchanged: the same states, `ErrDialogResponse` for a
// non-2xx and the same observer callback. On cancellation the returned error
// still matches ctx (`context.Canceled`/`context.DeadlineExceeded`) and is
// joined with any CANCEL/cleanup error. `Close` keeps meaning local cleanup
// only and never sends BYE or CANCEL.
func (s *DialogClientSession) WaitAnswerWithCancel(ctx context.Context, answer AnswerOptions, policy InviteCancelPolicy) error {
	if err := policy.validate(); err != nil {
		return err
	}

	return s.waitAnswer(ctx, answer, func(cancelCtx context.Context, tx sip.ClientTransaction) error {
		return newInviteTeardown(s, tx, answer, policy).run(cancelCtx)
	})
}

// inviteTeardown owns the bounded teardown of one initial INVITE after the
// caller explicitly stopped waiting for an answer.
//
// It runs exactly two consumers - one for the CANCEL it creates and one for the
// original INVITE - under independent deadlines, joins both and only then
// returns, so the caller never releases its fence while a signaling task is
// still running without an owner.
type inviteTeardown struct {
	session *DialogClientSession
	policy  InviteCancelPolicy
	answers AnswerOptions

	invite sip.ClientTransaction

	finalCtx    context.Context
	finalCancel context.CancelFunc
	cancelCtx   context.Context
	cancelStop  context.CancelFunc

	// cleanup owns every protocol duty triggered by a late/duplicate 2xx. It
	// starts lazily and is joined before the teardown returns.
	cleanup *lateAnswerCleanup

	wg sync.WaitGroup

	firstProvisional chan struct{}
	provisionalOnce  sync.Once
	firstFinal       chan struct{}
	finalOnce        sync.Once

	mu          sync.Mutex
	sawFinal    bool
	lastRes     *sip.Response
	finalRes    *sip.Response
	inviteErr   error
	observerErr error
}

func newInviteTeardown(s *DialogClientSession, invite sip.ClientTransaction, answers AnswerOptions, policy InviteCancelPolicy) *inviteTeardown {
	return &inviteTeardown{
		session:          s,
		policy:           policy,
		answers:          answers,
		invite:           invite,
		firstProvisional: make(chan struct{}),
		firstFinal:       make(chan struct{}),
	}
}

func (t *inviteTeardown) run(ctx context.Context) error {
	// The business context is already done. Keep its values, but rebuild
	// explicit and independent deadlines for every signaling responsibility
	// instead of inheriting a canceled context.
	base := context.WithoutCancel(ctx)

	t.finalCtx, t.finalCancel = context.WithTimeout(base, t.policy.FinalResponseTimeout)
	defer t.finalCancel()

	t.cancelCtx, t.cancelStop = context.WithTimeout(base, t.policy.CancelTimeout)
	defer t.cancelStop()

	t.cleanup = newLateAnswerCleanup(t.session, base, t.policy.CleanupTimeout)

	// Duplicated 2xx responses are only reported through the transaction
	// retransmission notification, and each of them still needs its ACK. The
	// notification never runs protocol work itself.
	t.invite.OnRetransmission(t.cleanup.notify)

	t.log().Debug("invite teardown started",
		"cancel_timeout", t.policy.CancelTimeout,
		"final_timeout", t.policy.FinalResponseTimeout,
		"cleanup_timeout", t.policy.CleanupTimeout,
	)

	// The original INVITE consumer starts first so a late final response is
	// always delivered instead of blocking the transaction FSM while the CANCEL
	// is submitted.
	t.wg.Add(1)
	go t.consumeInvite()

	cancelErr := t.cancelInvite()

	// Independent consumers: the INVITE keeps being consumed until its own
	// final response or the deadline fixed when the teardown started.
	t.wg.Wait()
	t.cleanup.closeInput()

	if res := t.lastFinalResponse(); res != nil {
		t.session.InviteResponse = res
	}

	// The protocol cleanup owns its own budget and is joined here, so the caller
	// never releases its fence while an ACK/BYE task is still running. The INVITE
	// transaction stays alive until then: responses already matched by the
	// transaction layer must still be able to reach the cleanup and get their
	// ACK instead of being dropped while the cleanup drains.
	cleanupErr := t.cleanup.join()
	t.invite.Terminate()

	return errors.Join(ctx.Err(), cancelErr, t.inviteError(), t.observerError(), cleanupErr)
}

func (t *inviteTeardown) log() *slog.Logger {
	if t.session != nil && t.session.UA != nil && t.session.UA.Client != nil && t.session.UA.Client.log != nil {
		return t.session.UA.Client.log
	}
	return sip.DefaultLogger().With("caller", "DialogClientSession")
}

// cancelInvite performs the CANCEL half of the teardown. It never waits for the
// original INVITE final response, and a failure here can not drop that
// responsibility.
func (t *inviteTeardown) cancelInvite() error {
	if err := t.awaitProvisional(); err != nil {
		return err
	}

	if !t.cancelStillValid() {
		// RFC 3261 9.1: a CANCEL is only valid before a final response.
		return nil
	}

	// The in-dialog request builder reads the last INVITE response (for the To
	// tag and the route set). The consumer records into its own field to stay
	// race free, so publish it here, from the goroutine that drives the CANCEL.
	if res := t.lastResponse(); res != nil {
		t.session.InviteResponse = res
	}

	cancelReq := newCancelRequest(t.session.InviteRequest)

	// The CANCEL gets its own transaction and its own consumer; its 200 only
	// confirms that the request was processed and is never proof that the
	// original INVITE finished.
	res, err := t.session.Do(t.cancelCtx, cancelReq)
	if err != nil {
		return fmt.Errorf("cancel request failed: %w", err)
	}
	if res.StatusCode != sip.StatusOK {
		return fmt.Errorf("cancel failed with non 200. code=%d", res.StatusCode)
	}
	return nil
}

// awaitProvisional waits, bounded by the CANCEL budget, until the original
// INVITE produced a provisional or a final response. Without a provisional no
// CANCEL may be sent, so the budget simply expires and the INVITE consumer keeps
// its own deadline.
func (t *inviteTeardown) awaitProvisional() error {
	// A provisional may already have been consumed by the answer wait before
	// the teardown started.
	if res := t.session.InviteResponse; res != nil && res.IsProvisional() {
		return nil
	}

	select {
	case <-t.firstProvisional:
		return nil
	case <-t.firstFinal:
		return nil
	case <-t.cancelCtx.Done():
		return fmt.Errorf("no provisional response before cancel deadline: %w", t.cancelCtx.Err())
	}
}

func (t *inviteTeardown) cancelStillValid() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return !t.sawFinal
}

// consumeInvite is the second, independent consumer: it keeps the original
// INVITE transaction drained until a final response arrives, the transaction
// terminates by itself, or the fixed deadline expires.
func (t *inviteTeardown) consumeInvite() {
	defer t.wg.Done()

	for {
		select {
		case res := <-t.invite.Responses():
			t.observe(res)
			if !res.IsProvisional() {
				// A final response ends the consumption; the remaining protocol
				// duties run under the cleanup budget before this teardown
				// returns.
				return
			}
		case <-t.invite.Done():
			t.mu.Lock()
			t.inviteErr = t.invite.Err()
			t.mu.Unlock()
			return
		case <-t.finalCtx.Done():
			// Deadline fixed when the teardown started: repeated 1xx responses
			// can not extend it.
			return
		}
	}
}

func (t *inviteTeardown) observe(res *sip.Response) {
	t.mu.Lock()
	t.lastRes = res
	if !res.IsProvisional() {
		t.sawFinal = true
		if t.finalRes == nil {
			t.finalRes = res
		}
	}
	t.mu.Unlock()

	if res.IsProvisional() {
		t.provisionalOnce.Do(func() { close(t.firstProvisional) })
	} else {
		t.finalOnce.Do(func() { close(t.firstFinal) })
	}

	if !res.IsProvisional() {
		// A late/duplicate 2xx is cleaned up by the owned worker, never on this
		// stack and never with an unowned goroutine per response.
		t.cleanup.enqueue(res)
	}

	// The caller observer keeps its own evidence of what really arrived. Its
	// error is recorded, but it can not remove a cleanup responsibility.
	if t.answers.OnResponse != nil {
		if err := t.answers.OnResponse(res); err != nil {
			t.mu.Lock()
			if t.observerErr == nil {
				t.observerErr = err
			}
			t.mu.Unlock()
		}
	}
}

func (t *inviteTeardown) lastResponse() *sip.Response {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.lastRes
}

func (t *inviteTeardown) lastFinalResponse() *sip.Response {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.finalRes
}

func (t *inviteTeardown) inviteError() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.inviteErr
}

func (t *inviteTeardown) observerError() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.observerErr
}
