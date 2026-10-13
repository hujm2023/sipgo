package sip

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"sync"

	"golang.org/x/sync/singleflight"
)

type Connection interface {
	// LocalAddr used for connection
	LocalAddr() net.Addr
	// WriteMsg marshals message and sends to socket
	WriteMsg(msg Message) error
	// Reference of connection can be increased/decreased to prevent closing to earlyss
	Ref(i int) int
	// Close decreases reference and if ref = 0 closes connection. Returns last ref. If 0 then it is closed
	TryClose() (int, error)

	Close() error
}

// ContextWriter is an optional capability implemented by connections that can
// bound a single message write with a caller context.
//
// The required Connection method set is intentionally left untouched so custom
// connection implementations keep compiling; callers must type-assert to
// ContextWriter before relying on a bounded write.
//
// Implementations must honor this contract:
//   - giving up while the write is still queued must not touch the socket;
//   - once the write has actually started, an expired context must really
//     unblock the underlying write (hard-closing that exact connection is
//     allowed) and the call must only return after that write returned.
type ContextWriter interface {
	WriteMsgContext(ctx context.Context, msg Message) error
}

var (
	// ErrWriteCanceled is returned when a write was abandoned before it
	// started, either while waiting for the connection write queue or because
	// the context was already done. The connection is left untouched.
	ErrWriteCanceled = errors.New("connection write canceled before start")

	// ErrWriteStalled is returned when a write had already started and did not
	// return before the budget/context expired. The exact connection was
	// hard-closed to unblock it, so the message may be partially sent and the
	// connection must not be reused.
	ErrWriteStalled = errors.New("connection write stalled")

	// ErrWriteNotCancelable is returned by context-aware transports when a
	// connection that cannot honor a context is asked for a bounded write on a
	// protected reliable transport (TCP/TLS). It prevents silently degrading a
	// requested budget into an unbounded write.
	ErrWriteNotCancelable = errors.New("connection does not support context writes")
)

var bufPool = sync.Pool{
	New: func() interface{} {
		// The Pool's New function should generally only return pointer
		// types, since a pointer can be put into the return interface
		// value without an allocation:
		b := new(bytes.Buffer)
		// b.Grow(2048)
		return b
	},
}

type connectionPool struct {
	// TODO consider sync.Map way with atomic checks to reduce mutex contention
	sync.RWMutex
	m               map[string]Connection
	sf              singleflight.Group
	closed          bool
	creating        sync.WaitGroup
	reading         sync.WaitGroup
	nextCreation    uint64
	cancelCreations map[uint64]context.CancelFunc
	nextReader      uint64
	readers         map[uint64]Connection
}

func newConnectionPool() *connectionPool {
	p := &connectionPool{}
	p.init()
	return p
}

func (p *connectionPool) init() {
	p.m = make(map[string]Connection)
	p.cancelCreations = make(map[uint64]context.CancelFunc)
	p.readers = make(map[uint64]Connection)
}

// addSingleflight returns an existing connection or shares one in-flight
// creation with other callers of the same key.
//
// The waiter honors ctx: giving up while the shared call is still running
// returns immediately instead of blocking behind another caller's dial. The
// in-flight creation stays owned by the singleflight itself; its result is
// cached in the pool (or dropped on error) even when nobody waits for it
// anymore, so an abandoned wait never leaves an unowned dial or a leaked
// connection.
func (p *connectionPool) addSingleflight(ctx context.Context, raddr Addr, laddr Addr, reuse bool, do func() (Connection, error)) (Connection, error) {
	return p.addSingleflightRead(ctx, raddr, laddr, reuse, func(context.Context) (Connection, error) { return do() }, nil)
}

// addSingleflightRead makes TCP/TLS establishment and the successful reader the
// pool's responsibility. A waiter may leave without abandoning either owner.
// Publication and reader registration are atomic with Clear closing admission.
func (p *connectionPool) addSingleflightRead(ctx context.Context, raddr Addr, laddr Addr, reuse bool, do func(context.Context) (Connection, error), read func(Connection)) (Connection, error) {
	a := raddr.String()
	shared := laddr.Port > 0 || reuse
	create := func() (Connection, error) {
		p.Lock()
		if p.closed {
			p.Unlock()
			return nil, net.ErrClosed
		}
		var creationID uint64
		createCtx := ctx
		if read != nil {
			var cancel context.CancelFunc
			createCtx, cancel = context.WithCancel(ctx)
			p.nextCreation++
			creationID = p.nextCreation
			p.cancelCreations[creationID] = cancel
			p.creating.Add(1)
		}
		p.Unlock()
		if read != nil {
			defer func() {
				p.Lock()
				cancel := p.cancelCreations[creationID]
				delete(p.cancelCreations, creationID)
				p.Unlock()
				cancel()
				p.creating.Done()
			}()
		}
		c, err := do(createCtx)
		if err != nil {
			return nil, err
		}
		if shared {
			c.Ref(-1)
		} else if c.Ref(0) < 1 {
			c.Ref(1)
		}
		if err := p.publishRead(c, a, read); err != nil {
			return nil, err
		}
		return c, nil
	}
	if !shared {
		return create()
	}
	key := laddr.String() + a
	result := p.sf.DoChan(key, func() (any, error) {
		var cached Connection
		if laddr.Port > 0 {
			cached = p.getUnref(laddr.String())
		} else {
			cached = p.getUnref(a)
		}
		if cached != nil {
			return cached, nil
		}
		return create()
	})
	select {
	case res := <-result:
		if res.Err != nil {
			return nil, res.Err
		}
		c := res.Val.(Connection)
		c.Ref(1)
		return c, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("connection singleflight wait canceled: %w", ctx.Err())
	}
}

func (p *connectionPool) publishRead(c Connection, addr string, read func(Connection)) error {
	p.Lock()
	if p.closed {
		p.Unlock()
		_ = c.Close()
		return net.ErrClosed
	}
	p.m[addr] = c
	p.m[c.LocalAddr().String()] = c
	if read != nil {
		p.nextReader++
		readerID := p.nextReader
		p.readers[readerID] = c
		p.reading.Add(1)
		go func() {
			defer p.reading.Done()
			defer func() { p.Lock(); delete(p.readers, readerID); p.Unlock() }()
			read(c)
		}()
	}
	p.Unlock()
	return nil
}

func (p *connectionPool) Add(a string, c Connection) {
	// TODO how about multi connection support for same remote address
	// We can then check ref count

	if c.Ref(0) < 1 {
		c.Ref(1) // Make 1 reference count by default
	}
	p.Lock()
	if p.closed {
		p.Unlock()
		_ = c.Close()
		return
	}
	p.m[a] = c
	p.Unlock()
}

// Getting connection pool increases reference
// Make sure you TryClose after finish
func (p *connectionPool) Get(a string) (c Connection) {
	// p.RLock()
	// c, exists := p.m[a]
	// p.RUnlock()
	// if !exists {
	// 	return nil
	// }
	c = p.getUnref(a)
	if c == nil {
		return nil
	}
	c.Ref(1)
	return c
}

func (p *connectionPool) getUnref(a string) (c Connection) {
	p.RLock()
	c, exists := p.m[a]
	if p.closed {
		exists = false
	}
	p.RUnlock()
	if !exists {
		return nil
	}
	return c
}

// CloseAndDelete closes connection and deletes from pool
func (p *connectionPool) CloseAndDelete(c Connection, addr string) error {
	p.Lock()
	defer p.Unlock()
	delete(p.m, addr)
	ref, _ := c.TryClose() // Be nice. Saves from double closing
	if ref > 0 {
		return c.Close()
	}
	return nil
}

func (p *connectionPool) Delete(addr string) {
	p.Lock()
	defer p.Unlock()
	delete(p.m, addr)
}

func (p *connectionPool) DeleteMultiple(addrs []string) {
	p.Lock()
	defer p.Unlock()
	for _, a := range addrs {
		delete(p.m, a)
	}
}

// Clear will clear all connection from pool and close them
func (p *connectionPool) Clear() error {
	p.Lock()
	p.closed = true
	cancels := make([]context.CancelFunc, 0, len(p.cancelCreations))
	for _, cancel := range p.cancelCreations {
		cancels = append(cancels, cancel)
	}
	owned := make([]Connection, 0, len(p.readers))
	for _, c := range p.readers {
		owned = append(owned, c)
	}
	conns := make([]Connection, 0, len(p.m))
	for _, c := range p.m {
		conns = append(conns, c)
	}
	p.m = make(map[string]Connection)
	p.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	var err error
	// A reader owns its socket even if every address alias was replaced/deleted.
	// Close these sockets first; cached aliases then observe the dropped refcount.
	for _, c := range owned {
		if e := c.Close(); e != nil && !errors.Is(e, net.ErrClosed) {
			err = errors.Join(err, e)
		}
	}
	for _, c := range conns {
		if c.Ref(0) > 0 {
			err = errors.Join(err, c.Close())
		}
	}
	p.creating.Wait()
	p.reading.Wait()
	return err
}

func (p *connectionPool) Size() int {
	p.RLock()
	l := len(p.m)
	p.RUnlock()
	return l
}

// all returns a snapshot of pooled connections. Used by transport owners that
// need to apply a connection-level setting to already established connections.
func (p *connectionPool) all() []Connection {
	p.RLock()
	defer p.RUnlock()
	conns := make([]Connection, 0, len(p.m))
	for _, c := range p.m {
		conns = append(conns, c)
	}
	return conns
}
