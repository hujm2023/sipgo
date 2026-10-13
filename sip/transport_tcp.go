package sip

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// TCP transport implementation
type TransportTCP struct {
	transport       string
	parser          *Parser
	log             *slog.Logger
	connectionReuse bool
	readFilter      TransportReadFilter

	pool *connectionPool

	DialerCreate func(laddr net.Addr) net.Dialer

	onConnClose func(conn Connection)

	// writeTimeout is the reliable write budget applied to every connection of
	// this transport, including inbound (accepted) ones. Zero keeps the legacy
	// unbounded writes.
	writeTimeout time.Duration
}

func (t *TransportTCP) init(par *Parser) {
	t.parser = par
	t.pool = newConnectionPool()
	t.transport = "TCP"
	if t.log == nil {
		t.log = DefaultLogger()
	}
	if t.DialerCreate == nil {
		t.DialerCreate = func(laddr net.Addr) net.Dialer {
			return net.Dialer{
				Timeout:   1 * time.Minute,
				LocalAddr: laddr,
			}
		}
	}
}

func (t *TransportTCP) String() string {
	return "Transport<TCP>"
}

func (t *TransportTCP) Network() string {
	// return "tcp"
	return t.transport
}

func (t *TransportTCP) Close() error {
	// return t.connections.Done()
	return t.pool.Clear()
}

// applyWriteTimeout sets the reliable write budget for this transport and, when
// the transport is already initialized, for every connection it owns. It is
// called by the transport layer owner (UserAgent option) so that both outbound
// and inbound connections share one connection-level policy.
func (t *TransportTCP) applyWriteTimeout(d time.Duration) {
	if d < 0 {
		d = 0
	}
	t.writeTimeout = d
	if t.pool == nil {
		return
	}
	for _, c := range t.pool.all() {
		if tc, ok := c.(*TCPConnection); ok {
			tc.setWriteBudget(d)
		}
	}
}

// newConnection builds a connection of this transport with the transport level
// write budget installed.
func (t *TransportTCP) newConnection(conn net.Conn, refcount int) *TCPConnection {
	c := &TCPConnection{
		Conn:     conn,
		refcount: refcount,
	}
	c.setWriteBudget(t.writeTimeout)
	return c
}

// Serve is direct way to provide conn on which this worker will listen
func (t *TransportTCP) Serve(l net.Listener, handler MessageHandler) error {
	t.log.Debug("begin listening on", "network", t.Network(), "laddr", l.Addr().String())
	for {
		conn, err := l.Accept()
		if err != nil {
			t.log.Debug("Fail to accept conenction", "error", err)
			return err
		}
		t.initConnection(conn, conn.RemoteAddr().String(), handler)
	}
}

func (t *TransportTCP) GetConnection(addr string) Connection {
	c := t.pool.Get(addr)
	return c
}

func (t *TransportTCP) CreateConnection(ctx context.Context, laddr Addr, raddr Addr, handler MessageHandler) (Connection, error) {
	// We are letting transport layer to resolve our address
	// raddr, err := net.ResolveTCPAddr("tcp", addr)
	// if err != nil {
	// 	return nil, err
	// }
	// We do singleflight if laddr is required or connection reuse
	conn, err := t.pool.addSingleflight(raddr, laddr, t.connectionReuse, func() (Connection, error) {
		var tladdr *net.TCPAddr = nil
		if laddr.IP != nil {
			tladdr = &net.TCPAddr{
				IP:   laddr.IP,
				Port: laddr.Port,
			}
		}

		traddr := &net.TCPAddr{
			IP:   raddr.IP,
			Port: raddr.Port,
		}

		addr := traddr.String()
		t.log.Debug("Dialing new connection", "raddr", addr)

		d := t.DialerCreate(tladdr)

		conn, err := d.DialContext(ctx, "tcp", addr)
		if err != nil {
			return nil, fmt.Errorf("%s dial err=%w", t, err)
		}

		// if err := conn.SetKeepAlive(true); err != nil {
		// 	return nil, fmt.Errorf("%s keepalive err=%w", t, err)
		// }

		// if err := conn.SetKeepAlivePeriod(30 * time.Second); err != nil {
		// 	return nil, fmt.Errorf("%s keepalive period err=%w", t, err)
		// }

		t.log.Debug("New connection", "raddr", raddr)
		c := t.newConnection(conn, 2+TransportIdleConnection) // 1 returning + 1 reading + Idle

		go t.readConnection(c, c.LocalAddr().String(), c.RemoteAddr().String(), handler)
		return c, nil
	})
	if err != nil {
		return nil, err
	}

	c := conn.(*TCPConnection)
	return c, nil
}

func (t *TransportTCP) initConnection(conn net.Conn, raddr string, handler MessageHandler) Connection {
	// // conn.SetKeepAlive(true)
	// conn.SetKeepAlivePeriod(3 * time.Second)
	laddr := conn.LocalAddr().String()
	t.log.Debug("New connection", "raddr", raddr)
	c := t.newConnection(conn, 1+TransportIdleConnection)
	t.pool.Add(laddr, c)
	t.pool.Add(raddr, c)
	go t.readConnection(c, laddr, raddr, handler)
	return c
}

// This should performe better to avoid any interface allocation
func (t *TransportTCP) readConnection(conn *TCPConnection, laddr string, raddr string, handler MessageHandler) {
	buf := make([]byte, TransportBufferReadSize)
	defer t.pool.Delete(laddr)
	defer func() {
		if err := t.pool.CloseAndDelete(conn, raddr); err != nil {
			t.log.Warn("connection pool not clean cleanup", "error", err)
		}
	}()
	defer func() {
		if t.onConnClose != nil {
			t.onConnClose(conn)
		}
	}()

	// Create stream parser context
	par := t.parser.NewSIPStream()

	for {
		num, err := conn.Read(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) || errors.Is(err, io.EOF) {
				t.log.Debug("connection was closed", "error", err)
				return
			}

			t.log.Error("Read error", "error", err)
			return
		}

		data := buf[:num]
		if len(bytes.Trim(data, "\x00")) == 0 {
			continue
		}

		if t.readFilter != nil {
			filtered, err := t.readFilter(TransportReadProps{
				Transport:  t.Network(),
				LocalAddr:  conn.LocalAddr(),
				RemoteAddr: conn.RemoteAddr(),
			}, data)
			if err != nil {
				t.log.Error("Read filter error", "laddr", laddr, "raddr", raddr, "error", err)
				return
			}
			if len(filtered) == 0 {
				continue
			}
			data = filtered
		}

		// Check is keep alive
		datalen := len(data)
		if datalen <= 4 {
			// One or 2 CRLF
			// https://datatracker.ietf.org/doc/html/rfc5626#section-3.5.1
			if len(bytes.Trim(data, "\r\n")) == 0 {
				t.log.Debug("Keep alive CRLF received")
				if datalen == 4 {
					// 2 CRLF is ping
					if _, err := conn.Write(data[:2]); err != nil {
						t.log.Error("Failed to pong keep alive", "error", err)
						return
					}
				}
				continue
			}
		}

		// TODO fallback to parseFull if message size limit is set

		// t.log.Debug().Str("raddr", raddr).Str("data", string(data)).Msg("new message")
		if err := t.parseStream(par, data, raddr, handler); errors.Is(err, ErrMessageTooLarge) {
			return
		}
	}
}

func (t *TransportTCP) parseStream(par *ParserStream, data []byte, src string, handler MessageHandler) error {
	err := par.ParseSIPStream(data, func(msg Message) {
		msg.SetTransport(t.Network())
		msg.SetSource(src)
		handler(msg)
	})

	if err != nil {
		if err == ErrParseSipPartial {
			return nil
		}
		t.log.Error("failed to parse", "error", err, "data", string(data))
		return err
	}
	return nil
}

type TCPConnection struct {
	net.Conn

	mu       sync.RWMutex
	refcount int

	// writeQueue serializes socket writes for this connection. A goroutine that
	// gives up while still queued never touches the socket, so canceling a
	// queued write can not close a healthy connection.
	writeQueue chan struct{}

	// writeBudget bounds one socket write when the caller did not provide an
	// earlier context deadline. Zero disables the protection (legacy writes).
	writeBudget atomic.Int64
}

func (c *TCPConnection) Ref(i int) int {
	c.mu.Lock()
	c.refcount += i
	ref := c.refcount
	c.mu.Unlock()
	DefaultLogger().Debug("TCP reference increment", "ip", c.LocalAddr().String(), "dst", c.RemoteAddr().String(), "ref", ref)
	return ref
}

// Close is a hard close: it drops the reference count so the socket is closed
// immediately. A concurrent blocked write is unblocked by the socket close.
func (c *TCPConnection) Close() error {
	c.mu.Lock()
	c.refcount = 0
	c.mu.Unlock()
	DefaultLogger().Debug("TCP doing hard close", "ip", c.LocalAddr().String(), "dst", c.RemoteAddr().String(), "ref", 0)
	return c.Conn.Close()
}

func (c *TCPConnection) TryClose() (int, error) {
	c.mu.Lock()
	c.refcount--
	ref := c.refcount
	c.mu.Unlock()
	DefaultLogger().Debug("TCP reference decrement", "ip", c.LocalAddr().String(), "dst", c.RemoteAddr().String(), "ref", ref)
	if ref > 0 {
		return ref, nil
	}

	if ref < 0 {
		DefaultLogger().Warn("TCP ref went negative", "ip", c.LocalAddr().String(), "dst", c.RemoteAddr().String(), "ref", ref)
		return 0, nil
	}

	DefaultLogger().Debug("TCP closing", "ip", c.LocalAddr().String(), "dst", c.RemoteAddr().String(), "ref", ref)
	return ref, c.Conn.Close()
}

// setWriteBudget installs the reliable write budget for this connection.
func (c *TCPConnection) setWriteBudget(d time.Duration) {
	if d < 0 {
		d = 0
	}
	c.writeBudget.Store(int64(d))
}

func (c *TCPConnection) getWriteBudget() time.Duration {
	return time.Duration(c.writeBudget.Load())
}

// writeBudgetContext returns ctx clamped to the connection write budget. The
// returned context has an earlier deadline (or the same cancellation) when a
// budget is configured; otherwise ctx itself is returned with a no-op cancel.
func (c *TCPConnection) writeBudgetContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if budget := c.getWriteBudget(); budget > 0 {
		return context.WithTimeout(ctx, budget)
	}
	return ctx, func() {}
}

// writeToken returns the (lazily created) write serialization token.
func (c *TCPConnection) writeToken() chan struct{} {
	c.mu.RLock()
	q := c.writeQueue
	c.mu.RUnlock()
	if q != nil {
		return q
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.writeQueue == nil {
		q = make(chan struct{}, 1)
		q <- struct{}{}
		c.writeQueue = q
	}
	return c.writeQueue
}

// rawWrite is the unbounded socket write used once the write token is held.
func (c *TCPConnection) rawWrite(b []byte) (n int, err error) {
	// Some debug hook. TODO move to proper way
	n, err = c.Conn.Write(b)
	if SIPDebug {
		logSIPWrite("TCP", c.Conn.LocalAddr().String(), c.Conn.RemoteAddr().String(), b[:n])
	}
	return n, err
}

// writeContext writes data honoring ctx:
//
//   - while queued behind another write, ctx expiry returns ErrWriteCanceled
//     and the connection is left untouched;
//   - once the write started, ctx expiry hard-closes this exact connection so
//     the blocked write returns, and the call only returns after that write
//     returned (the watcher goroutine is joined).
func (c *TCPConnection) writeContext(ctx context.Context, data []byte) (int, error) {
	token := c.writeToken()

	if ctx == nil || ctx.Done() == nil {
		<-token
		defer func() { token <- struct{}{} }()
		return c.rawWrite(data)
	}

	select {
	case <-token:
	case <-ctx.Done():
		// No write has started, so this must not affect the connection. A
		// later, healthy write can still use it.
		return 0, fmt.Errorf("%w: %w", ErrWriteCanceled, ctx.Err())
	}
	defer func() { token <- struct{}{} }()

	// Do not start a write that is already over budget.
	if err := ctx.Err(); err != nil {
		return 0, fmt.Errorf("%w: %w", ErrWriteCanceled, err)
	}

	writeDone := make(chan struct{})
	watchDone := make(chan struct{})
	var stalled atomic.Bool

	go func() {
		defer close(watchDone)

		select {
		case <-writeDone:
			return
		case <-ctx.Done():
		}

		// Prefer a write that already returned: ctx may fire in the same
		// instant the socket accepted the last byte.
		select {
		case <-writeDone:
			return
		default:
		}

		stalled.Store(true)
		// Hard-close the exact connection. This is the only portable way to
		// wake a blocked stream write, and an interrupted TLS write leaves a
		// connection that must not be reused.
		_ = c.Conn.Close()
	}()

	n, err := c.rawWrite(data)
	close(writeDone)
	<-watchDone

	if stalled.Load() {
		return n, fmt.Errorf("%w on %s: %w", ErrWriteStalled, c.connAddrString(), ctx.Err())
	}
	return n, err
}

func (c *TCPConnection) connAddrString() string {
	if c.Conn == nil {
		return "<nil>"
	}
	return c.Conn.RemoteAddr().String()
}

func (c *TCPConnection) Read(b []byte) (n int, err error) {
	// Some debug hook. TODO move to proper way
	n, err = c.Conn.Read(b)
	if SIPDebug {
		logSIPRead("TCP", c.Conn.LocalAddr().String(), c.Conn.RemoteAddr().String(), b[:n])
	}
	return n, err
}

// Write writes raw bytes, bounded by the connection reliable write budget when
// one is installed. Automatic ACK/retransmission writes from transaction FSMs
// use this path too, so they share the same protection.
func (c *TCPConnection) Write(b []byte) (n int, err error) {
	ctx, cancel := c.writeBudgetContext(context.Background())
	defer cancel()
	return c.writeContext(ctx, b)
}

func (c *TCPConnection) WriteMsg(msg Message) error {
	buf := bufPool.Get().(*bytes.Buffer)
	defer bufPool.Put(buf)
	buf.Reset()
	msg.StringWrite(buf)
	data := buf.Bytes()

	n, err := c.Write(data)
	if err != nil {
		return fmt.Errorf("conn %s write err=%w", c.RemoteAddr().String(), err)
	}

	if n == 0 {
		return fmt.Errorf("wrote 0 bytes")
	}

	if n != len(data) {
		return fmt.Errorf("fail to write full message")
	}
	return nil
}

// WriteMsgContext implements ContextWriter for TCP and TLS connections.
func (c *TCPConnection) WriteMsgContext(ctx context.Context, msg Message) error {
	if ctx == nil {
		ctx = context.Background()
	}

	wctx, cancel := c.writeBudgetContext(ctx)
	defer cancel()

	buf := bufPool.Get().(*bytes.Buffer)
	defer bufPool.Put(buf)
	buf.Reset()
	msg.StringWrite(buf)
	data := buf.Bytes()

	n, err := c.writeContext(wctx, data)
	if err != nil {
		return fmt.Errorf("conn %s write err=%w", c.RemoteAddr().String(), err)
	}

	if n == 0 {
		return fmt.Errorf("wrote 0 bytes")
	}

	if n != len(data) {
		return fmt.Errorf("fail to write full message")
	}
	return nil
}
