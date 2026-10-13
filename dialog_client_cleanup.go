package sipgo

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hujm2023/sipgo/sip"
)

// lateCleanupQueueSize bounds how many final responses may wait for the cleanup
// worker. It is only a back-pressure buffer: every entry is handled by the
// single owned worker.
const lateCleanupQueueSize = 64

// lateAnswerCleanup is a UA-owned protocol controller. Its fixed protocol
// lifetime is separate from each dialog's product cleanup deadline. It freezes
// request/client identity and never accesses the caller's session after handoff.
type lateAnswerCleanup struct {
	client        *Client
	inviteRequest *sip.Request
	sequence      atomic.Uint32
	base          context.Context
	timeout       time.Duration
	invite        sip.ClientTransaction
	queue         chan *sip.Response
	ctx           context.Context
	stop          context.CancelFunc
	inputGone     chan struct{}
	initialDone   chan struct{}
	initialOnce   sync.Once
	workerWG      sync.WaitGroup
	tasksWG       sync.WaitGroup
	mu            sync.Mutex
	started       bool
	closed        bool
	views         map[string]*lateDialogView
	errs          []error
	initialID     string
}

type lateDialogView struct {
	id         string
	acks       atomic.Int64
	ctx        context.Context
	stop       context.CancelFunc
	mu         sync.Mutex
	byeStarted bool
	done       chan struct{}
	byeError   error
	initial    bool
}

func newLateAnswerCleanup(session *DialogClientSession, base context.Context, timeout time.Duration) *lateAnswerCleanup {
	c := &lateAnswerCleanup{
		client: session.UA.Client, inviteRequest: session.InviteRequest.Clone(),
		base: base, timeout: timeout, queue: make(chan *sip.Response, lateCleanupQueueSize),
		inputGone: make(chan struct{}), initialDone: make(chan struct{}), views: make(map[string]*lateDialogView),
	}
	c.sequence.Store(max(session.lastCSeqNo.Load(), c.inviteRequest.CSeq().SeqNo))
	return c
}

// notify only hands off protocol work, with the FSM lock released.
func (c *lateAnswerCleanup) notify(res *sip.Response) { c.enqueue(res) }

func (c *lateAnswerCleanup) enqueue(res *sip.Response) {
	if res == nil || res.IsProvisional() {
		return
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	if !c.started {
		c.ctx, c.stop = context.WithTimeout(c.base, max(sip.Timer_M, sip.Timer_D))
		if !c.client.UserAgent.trackCleanup(c) {
			c.stop()
			c.closed = true
			close(c.inputGone)
			c.mu.Unlock()
			return
		}
		c.started = true
		c.workerWG.Add(1)
		go c.worker()
	}
	c.mu.Unlock()
	if !res.IsSuccess() {
		c.initialOnce.Do(func() { close(c.initialDone) })
		return
	}
	// Clone on the notification stack, before another response changes FSM state.
	res = res.Clone()
	select {
	case c.queue <- res:
	case <-c.inputGone:
	case <-c.ctx.Done():
	}
}

// closeInput forbids all future admission, including lazy worker creation.
func (c *lateAnswerCleanup) closeInput() {
	c.mu.Lock()
	if !c.closed {
		c.closed = true
		close(c.inputGone)
	}
	c.mu.Unlock()
}

// join closes admission before waiting. The worker is the sole task creator,
// so tasksWG.Wait cannot race a later Add or a worker starting after this join.
func (c *lateAnswerCleanup) join() error {
	c.closeInput()
	c.workerWG.Wait()
	return c.cleanupError()
}

// completeInitial waits only for the first dialog's bounded ACK/BYE duties.
// The UA already owns the remaining protocol window, including new forks.
func (c *lateAnswerCleanup) completeInitial() error {
	c.mu.Lock()
	started := c.started
	c.mu.Unlock()
	if !started {
		return nil
	}
	select {
	case <-c.initialDone:
	case <-c.ctx.Done():
	}
	return c.cleanupError()
}

func (c *lateAnswerCleanup) worker() {
	defer c.workerWG.Done()
	defer c.client.UserAgent.untrackCleanup(c)
	defer func() {
		c.closeInput()
		c.tasksWG.Wait()
		c.stop()
		if c.invite != nil {
			c.invite.Terminate()
		}
		if err := c.cleanupError(); err != nil {
			c.client.log.Error("late answer protocol cleanup failed", "error", err)
		}
	}()
	for {
		select {
		case res := <-c.queue:
			c.handle(res)
		case <-c.ctx.Done():
			return
		case <-c.inputGone:
			c.drain()
			return
		}
	}
}

func (c *lateAnswerCleanup) drain() {
	for {
		select {
		case res := <-c.queue:
			c.handle(res)
		default:
			return
		}
	}
}

func (c *lateAnswerCleanup) handle(res *sip.Response) {
	view, created := c.dialogView(res)

	// Every 2xx, including every retransmission of it, carries its own ACK.
	// The first write failure gets exactly one extra attempt on the same dialog
	// identity, and both stay inside the cleanup budget.
	ackCtx := view.ctx
	var cancel context.CancelFunc
	if !created {
		ackCtx, cancel = context.WithTimeout(c.ctx, min(c.timeout, 2*time.Second))
		defer cancel()
	}
	if err := c.sendAck(ackCtx, res); err != nil {
		if retryErr := c.sendAck(ackCtx, res); retryErr != nil {
			c.recordError(fmt.Errorf("late 2xx ACK failed for dialog %q: %w", view.id, errors.Join(err, retryErr)))
		}
	}

	if created {
		c.startByeTask(view, res)
	}
}

func (c *lateAnswerCleanup) dialogView(res *sip.Response) (*lateDialogView, bool) {
	id, err := sip.DialogIDFromResponse(res)
	if err != nil || id == "" {
		// A response without a usable dialog identity still gets its ACK, but it
		// must not be merged with a real dialog's view.
		id = fmt.Sprintf("unidentified|%s", res.Short())
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if view, ok := c.views[id]; ok {
		return view, false
	}

	viewCtx, stop := context.WithTimeout(c.ctx, c.timeout)
	view := &lateDialogView{id: id, ctx: viewCtx, stop: stop, done: make(chan struct{})}
	if c.initialID == "" {
		c.initialID = id
		view.initial = true
	}
	c.views[id] = view
	return view, true
}

func (c *lateAnswerCleanup) sendAck(ctx context.Context, res *sip.Response) error {
	ack := newAckRequestUAC(c.inviteRequest, res, nil)
	applyDialogRouteSet(ack, res)

	if err := c.client.WriteRequestContext(ctx, ack); err != nil {
		return err
	}

	if view, ok := c.viewOf(res); ok {
		view.acks.Add(1)
	}
	return nil
}

func (c *lateAnswerCleanup) viewOf(res *sip.Response) (*lateDialogView, bool) {
	id, err := sip.DialogIDFromResponse(res)
	if err != nil || id == "" {
		id = fmt.Sprintf("unidentified|%s", res.Short())
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	view, ok := c.views[id]
	return view, ok
}

// startByeTask registers exactly one logical BYE per dialog. The task is owned
// by the controller (tracked in tasksWG), then joined by its UA at shutdown.
func (c *lateAnswerCleanup) startByeTask(view *lateDialogView, res *sip.Response) {
	view.mu.Lock()
	if view.byeStarted {
		view.mu.Unlock()
		return
	}
	view.byeStarted = true
	view.mu.Unlock()

	c.tasksWG.Add(1)
	go c.runBye(view, res)
}

func (c *lateAnswerCleanup) runBye(view *lateDialogView, res *sip.Response) {
	defer func() {
		view.stop()
		close(view.done)
		c.tasksWG.Done()
		if view.initial {
			c.initialOnce.Do(func() { close(c.initialDone) })
		}
	}()

	bye := newByeRequestUAC(c.inviteRequest, res, nil)
	bye.SetBody(nil)

	// A BYE inside the dialog needs a higher CSeq than the INVITE. The counter is
	// private to the protocol controller so it never mutates the released session.
	if cseq := bye.CSeq(); cseq != nil {
		cseq.SeqNo = c.sequence.Add(1)
		cseq.MethodName = sip.BYE
	}

	applyDialogRouteSet(bye, res)

	tx, err := c.client.TransactionRequest(view.ctx, bye, cleanupRequestValidate)
	if err != nil {
		err = fmt.Errorf("late 2xx BYE submit failed for dialog %q: %w", view.id, err)
		view.setByeError(err)
		c.recordError(err)
		return
	}
	defer tx.Terminate()

	// The BYE keeps its own bounded retransmission window: the transaction stays
	// alive until its final response or the cleanup budget expires.
	for {
		select {
		case response := <-tx.Responses():
			if response.IsProvisional() {
				continue
			}
			if !response.IsSuccess() {
				err := fmt.Errorf("late 2xx BYE failed for dialog %q: %s", view.id, response.Short())
				view.setByeError(err)
				c.recordError(err)
			}
			return
		case <-tx.Done():
			err := fmt.Errorf("late 2xx BYE not confirmed for dialog %q: %w", view.id, errors.Join(sip.ErrTransactionTerminated, tx.Err()))
			view.setByeError(err)
			c.recordError(err)
			return
		case <-view.ctx.Done():
			err := fmt.Errorf("late 2xx BYE did not complete for dialog %q: %w", view.id, view.ctx.Err())
			view.setByeError(err)
			c.recordError(err)
			return
		}
	}

}

func (v *lateDialogView) setByeError(err error) {
	v.mu.Lock()
	if v.byeError == nil {
		v.byeError = err
	}
	v.mu.Unlock()
}

func (v *lateDialogView) error() error {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.byeError
}

func (v *lateDialogView) byeSent() bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.byeStarted
}

func (c *lateAnswerCleanup) recordError(err error) {
	if err == nil {
		return
	}
	c.mu.Lock()
	c.errs = append(c.errs, err)
	c.mu.Unlock()
}

func (c *lateAnswerCleanup) cleanupError() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return errors.Join(c.errs...)
}

func (c *lateAnswerCleanup) dialogViews() []*lateDialogView {
	c.mu.Lock()
	defer c.mu.Unlock()

	views := make([]*lateDialogView, 0, len(c.views))
	for _, v := range c.views {
		views = append(views, v)
	}
	return views
}

// applyDialogRouteSet appends the dialog route set derived from the
// Record-Route of the response that created the dialog, mirroring what the
// in-dialog request builder does (RFC 3261 12.1.2).
func applyDialogRouteSet(req *sip.Request, res *sip.Response) {
	hdrs := res.GetHeaders("record-route")
	if len(hdrs) == 0 {
		return
	}

	for i := len(hdrs) - 1; i >= 0; i-- {
		req.AppendHeader(sip.NewHeader("Route", hdrs[i].Value()))
	}

	// Strict routing: the first Route becomes the request URI.
	if rh := req.Route(); rh != nil && !rh.Address.UriParams.Has("lr") {
		req.Recipient = rh.Address
	}
}

func cleanupRequestValidate(client *Client, req *sip.Request) error {
	if req.Via() == nil {
		ClientRequestAddVia(client, req)
	}
	if req.Body() == nil {
		req.SetBody(nil)
	}
	return nil
}
