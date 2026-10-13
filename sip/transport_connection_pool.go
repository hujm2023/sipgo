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
	m  map[string]Connection
	sf singleflight.Group
}

func newConnectionPool() *connectionPool {
	p := &connectionPool{}
	p.init()
	return p
}

func (p *connectionPool) init() {
	p.m = make(map[string]Connection)
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
	a := raddr.String()

	if laddr.Port > 0 || reuse {
		// TODO: implement singleflight without  type conversion
		laddrStr := laddr.String()
		// We create or return existing
		result := p.sf.DoChan(laddrStr+a, func() (any, error) {
			if laddr.Port > 0 {
				if c := p.getUnref(laddrStr); c != nil {
					return c, nil
				}
			} else {
				if c := p.getUnref(a); c != nil {
					return c, nil
				}
			}

			c, err := do()
			if err != nil {
				return nil, err
			}
			// Decrease reference as it will be increased after
			// Singleflight will return cached so we need todo this
			c.Ref(-1)

			p.Lock()
			defer p.Unlock()

			p.m[a] = c
			p.m[c.LocalAddr().String()] = c
			return c, nil
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
			// Stop waiting without touching the shared creation.
			return nil, fmt.Errorf("connection singleflight wait canceled: %w", ctx.Err())
		}
	}

	// There is nothing here to block
	c, err := do()
	if err != nil {
		return nil, err
	}

	if c.Ref(0) < 1 {
		c.Ref(1) // Make 1 reference count by default
	}
	p.m[a] = c
	p.m[c.LocalAddr().String()] = c
	return c, nil
}

func (p *connectionPool) Add(a string, c Connection) {
	// TODO how about multi connection support for same remote address
	// We can then check ref count

	if c.Ref(0) < 1 {
		c.Ref(1) // Make 1 reference count by default
	}
	p.Lock()
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
	defer p.Unlock()

	defer func() {
		// Remove all
		p.m = make(map[string]Connection)
	}()

	var werr error
	for _, c := range p.m {
		if c.Ref(0) <= 0 {
			continue
		}
		werr = errors.Join(werr, c.Close())
	}
	return werr
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
