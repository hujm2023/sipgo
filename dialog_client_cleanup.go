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

// lateCleanupSettle is how long the cleanup keeps accepting retransmissions
// after the response consumer stopped. Responses are matched by the transaction
// layer before they reach the FSM, so a few notifications can still arrive at
// that point; the settle window absorbs them so each of them still gets its ACK.
// The whole cleanup stays bounded by its own budget, this only shortens it.
const lateCleanupSettle = 50 * time.Millisecond

// lateAnswerCleanup owns the protocol duties for final responses that arrive
// after the caller explicitly stopped the initial INVITE:
//
//   - every late or duplicated 2xx gets its own ACK (a duplicated 2xx is only
//     reported through the transaction retransmission notification, so that
//     notification must be observed);
//   - every real dialog, including one with a different To-tag, gets at most one
//     logical BYE, with its own cleanup view;
//   - nothing runs on the transaction FSM notification stack: the notification
//     only hands the response to the single owned worker, and the per-dialog BYE
//     runs as a registered task. The whole cleanup is bounded by one budget and
//     joined before the caller is released.
type lateAnswerCleanup struct {
	session *DialogClientSession
	base    context.Context
	timeout time.Duration

	queue chan *sip.Response

	startOnce sync.Once
	started   atomic.Bool
	ctx       context.Context
	stop      context.CancelFunc

	inputOnce sync.Once
	inputGone chan struct{}

	workerWG sync.WaitGroup
	tasksWG  sync.WaitGroup

	mu    sync.Mutex
	views map[string]*lateDialogView
	errs  []error
}

// lateDialogView is the independent cleanup view of one real dialog. Views are
// keyed by the dialog identity of the actual response, so a response with a new
// To-tag can never reuse or end another dialog's cleanup.
type lateDialogView struct {
	id string

	acks atomic.Int64

	mu         sync.Mutex
	byeStarted bool

	// done is closed when this dialog's cleanup task finished.
	done     chan struct{}
	byeError error
}

func newLateAnswerCleanup(session *DialogClientSession, base context.Context, timeout time.Duration) *lateAnswerCleanup {
	return &lateAnswerCleanup{
		session:   session,
		base:      base,
		timeout:   timeout,
		queue:     make(chan *sip.Response, lateCleanupQueueSize),
		inputGone: make(chan struct{}),
		views:     make(map[string]*lateDialogView),
	}
}

// notify is the transaction retransmission notification. It is called from the
// FSM with the transaction lock released, and it must not perform protocol work
// on that stack: it only hands the response to the owned worker.
func (c *lateAnswerCleanup) notify(res *sip.Response) {
	c.enqueue(res)
}

// enqueue hands one final response to the cleanup worker. The worker and the
// cleanup budget are created on first use, so a call that never sees a final
// response pays nothing and has nothing to join.
func (c *lateAnswerCleanup) enqueue(res *sip.Response) {
	if res == nil || !res.IsSuccess() {
		// Non-2xx finals are acknowledged by the client transaction itself.
		return
	}

	c.startOnce.Do(func() {
		c.ctx, c.stop = context.WithTimeout(c.base, c.timeout)
		c.started.Store(true)
		c.workerWG.Add(1)
		go c.worker()
	})

	select {
	case <-c.inputGone:
		// The consumer stopped and the worker is draining its settle window. A
		// transaction FSM notification must never block on a stopped cleanup, so
		// only a non-blocking hand-off is attempted here.
		select {
		case c.queue <- res:
		default:
		}
	case c.queue <- res:
	case <-c.ctx.Done():
	}
}

// closeInput tells the cleanup that no further response can be delivered, so
// the worker drains what is queued and exits.
func (c *lateAnswerCleanup) closeInput() {
	c.inputOnce.Do(func() { close(c.inputGone) })
}

// join waits for the owner and its tasks to finish. It is bounded by the
// cleanup budget: the worker exits when the input is closed or the budget
// expires, and every BYE task observes the same budget.
func (c *lateAnswerCleanup) join() error {
	if !c.started.Load() {
		return nil
	}

	c.workerWG.Wait()
	c.tasksWG.Wait()
	c.stop()

	return c.cleanupError()
}

func (c *lateAnswerCleanup) worker() {
	defer c.workerWG.Done()

	for {
		select {
		case res := <-c.queue:
			c.handle(res)
			continue
		case <-c.ctx.Done():
			return
		case <-c.inputGone:
		}

		// The consumer stopped: drain what is already queued and then keep a
		// short settle window open for retransmissions that were matched before
		// the transaction was released. Everything stays inside the budget.
		c.drain()

		settle := time.NewTimer(lateCleanupSettle)
		select {
		case res := <-c.queue:
			settle.Stop()
			c.handle(res)
		case <-c.ctx.Done():
			settle.Stop()
			return
		case <-settle.C:
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
	if err := c.sendAck(res); err != nil {
		if retryErr := c.sendAck(res); retryErr != nil {
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

	view := &lateDialogView{id: id, done: make(chan struct{})}
	c.views[id] = view
	return view, true
}

func (c *lateAnswerCleanup) sendAck(res *sip.Response) error {
	ack := newAckRequestUAC(c.session.InviteRequest, res, nil)
	applyDialogRouteSet(ack, res)

	if err := c.session.UA.Client.WriteRequestContext(c.ctx, ack); err != nil {
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
// by the controller (tracked in tasksWG) and joined before the caller returns.
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
	defer c.tasksWG.Done()
	defer close(view.done)

	bye := newByeRequestUAC(c.session.InviteRequest, res, nil)
	bye.SetBody(nil)

	// A BYE inside the dialog needs a higher CSeq than the INVITE. The counter is
	// shared by the dialog session so concurrent dialog views never reuse it.
	if cseq := bye.CSeq(); cseq != nil {
		cseq.SeqNo = c.session.lastCSeqNo.Add(1)
		cseq.MethodName = sip.BYE
	}

	applyDialogRouteSet(bye, res)

	tx, err := c.session.UA.Client.TransactionRequest(c.ctx, bye, c.session.requestValidate)
	if err != nil {
		err = fmt.Errorf("late 2xx BYE submit failed for dialog %q: %w", view.id, err)
		view.setByeError(err)
		c.recordError(err)
		return
	}
	defer tx.Terminate()

	// The BYE keeps its own bounded retransmission window: the transaction stays
	// alive until its final response or the cleanup budget expires.
	select {
	case <-tx.Responses():
	case <-tx.Done():
	case <-c.ctx.Done():
		err := fmt.Errorf("late 2xx BYE did not complete for dialog %q: %w", view.id, c.ctx.Err())
		view.setByeError(err)
		c.recordError(err)
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
