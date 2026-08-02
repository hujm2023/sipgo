package sip

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/hujm2023/sipgo/fakes"
	"github.com/stretchr/testify/require"
)

func TestServerTransactionFSM(t *testing.T) {
	// SetTimers(1*time.Millisecond, 1*time.Millisecond, 1*time.Millisecond)
	req, _, _ := testCreateInvite(t, "sip:127.0.0.99:5060", "UDP", "127.0.0.2:5060")

	incoming := bytes.NewBuffer([]byte{})
	outgoing := bytes.NewBuffer([]byte{})

	t.Run("PassUpResponse", func(t *testing.T) {
		conn := &UDPConnection{
			PacketConn: &fakes.UDPConn{
				Reader:  incoming,
				Writers: map[string]io.Writer{"127.0.0.2:5060": outgoing},
			},
		}
		tx := NewServerTx("123", req, conn, slog.Default())
		err := tx.Init()
		require.NoError(t, err)

		err = tx.Receive(req)
		require.NoError(t, err)

		require.NoError(t, tx.Err())
		select {
		case <-tx.Done():
			t.Error("Transaction should not terminate")
		default:
		}
	})

	t.Run("OutOfOrderResponse", func(t *testing.T) {
		conn := &UDPConnection{
			PacketConn: &fakes.UDPConn{
				Reader:  incoming,
				Writers: map[string]io.Writer{"127.0.0.2:5060": outgoing},
			},
		}
		tx := NewServerTx("123", req, conn, slog.Default())
		err := tx.Init()
		require.NoError(t, err)

		// We received Cancel while dealing with resposn

		res100 := NewResponseFromRequest(req, StatusTrying, "Trying", nil)
		res200 := NewResponseFromRequest(req, StatusOK, "OK", nil)

		require.NoError(t, tx.Respond(res200))
		require.NoError(t, tx.Respond(res100))
		require.NoError(t, tx.Respond(res100))

		require.NoError(t, compareFunctions(tx.currentFsmState(), tx.inviteStateAccepted))
	})

}

func TestServerTransactionNonInviteFSM(t *testing.T) {
	// SetTimers(1*time.Millisecond, 1*time.Millisecond, 1*time.Millisecond)

	incoming := bytes.NewBuffer([]byte{})
	outgoing := bytes.NewBuffer([]byte{})

	conn := &UDPConnection{
		PacketConn: &fakes.UDPConn{
			Reader:  incoming,
			Writers: map[string]io.Writer{"127.0.0.1:5060": outgoing},
		},
	}

	t.Run("UDP", func(t *testing.T) {
		req := testCreateRequest(t, "OPTIONS", "sip:example.com", "UDP", "127.0.0.1:5060")
		tx := NewServerTx("123", req, conn, slog.Default())
		err := tx.Init()
		require.NoError(t, err)

		err = tx.Receive(req)
		require.NoError(t, err)
		require.NoError(t, compareFunctions(tx.currentFsmState(), tx.stateTrying))

		// passing 200 response
		err = tx.Respond(NewResponseFromRequest(req, 200, "OK", nil))
		require.NoError(t, err)
		require.NoError(t, compareFunctions(tx.currentFsmState(), tx.stateCompleted))

		// Timer j must be started
		require.NotNil(t, tx.timer_j)
	})

	t.Run("TCP", func(t *testing.T) {
		req := testCreateRequest(t, "OPTIONS", "sip:example.com", "TCP", "127.0.0.1:5060")
		tx := NewServerTx("123", req, conn, slog.Default())
		err := tx.Init()
		require.NoError(t, err)

		err = tx.Receive(req)
		require.NoError(t, err)
		require.NoError(t, compareFunctions(tx.currentFsmState(), tx.stateTrying))

		// passing 200 response
		err = tx.Respond(NewResponseFromRequest(req, 200, "OK", nil))
		require.NoError(t, err)
		require.NoError(t, compareFunctions(tx.currentFsmState(), tx.stateCompleted))

		// timer J should be zero
		require.Zero(t, tx.timer_j_time)
		require.Zero(t, <-tx.done)
	})
}

func TestServerTransactionRespondRejectsCRLF(t *testing.T) {
	req := testCreateRequest(t, "OPTIONS", "sip:example.com", "UDP", "127.0.0.1:5060")
	outgoing := bytes.NewBuffer(nil)
	conn := &UDPConnection{
		PacketConn: &fakes.UDPConn{
			Writers: map[string]io.Writer{"127.0.0.1:5060": outgoing},
		},
	}
	tx := NewServerTx("123", req, conn, slog.Default())

	res := NewResponseFromRequest(req, StatusOK, "OK\r\nInjected", nil)
	err := tx.Respond(res)
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid CRLF")
	require.Empty(t, outgoing.String())
}

func TestServerTransactionFSMInvite(t *testing.T) {
	req, _, _ := testCreateInvite(t, "sip:127.0.0.99:5060", "udp", "127.0.0.2:5060")

	incoming := bytes.NewBuffer([]byte{})
	outgoing := bytes.NewBuffer([]byte{})
	t.Run("InviteCancel", func(t *testing.T) {
		Timer_I = 10 * time.Millisecond
		conn := &UDPConnection{
			PacketConn: &fakes.UDPConn{
				Reader:  incoming,
				Writers: map[string]io.Writer{"127.0.0.2:5060": outgoing},
			},
		}
		tx := NewServerTx("123", req, conn, slog.Default())
		err := tx.Init()
		require.NoError(t, err)

		// We received Cancel while dealing with resposn
		res100 := NewResponseFromRequest(req, StatusTrying, "Trying", nil)
		require.NoError(t, tx.Respond(res100))

		// Cancel will play
		cancelReq := NewRequest(CANCEL, req.Recipient)
		cancelReq.AppendHeader(HeaderClone(req.Via())) // Cancel request must match invite TOP via and only have that Via
		cancelReq.AppendHeader(HeaderClone(req.From()))
		cancelReq.AppendHeader(HeaderClone(req.To()))
		cancelReq.AppendHeader(HeaderClone(req.CallID()))

		require.NoError(t, tx.Receive(cancelReq))
		require.NoError(t, compareFunctions(tx.currentFsmState(), tx.inviteStateCompleted))

		ack := NewRequest(ACK, req.Recipient)
		ack.AppendHeader(HeaderClone(req.Via())) // Cancel request must match invite TOP via and only have that Via
		ack.AppendHeader(HeaderClone(req.From()))
		ack.AppendHeader(HeaderClone(req.To()))
		ack.AppendHeader(HeaderClone(req.CallID()))
		require.NoError(t, tx.Receive(ack))

		require.Eventually(t, func() bool {
			return compareFunctions(tx.currentFsmState(), tx.inviteStateTerminated) == nil
		}, 10*Timer_I, Timer_I)
	})
}

func TestServerTransactionTerminateGracefullyReliableInvite(t *testing.T) {
	req, _, _ := testCreateInvite(t, "sip:127.0.0.99:5060", "TCP", "127.0.0.2:5060")
	newConn := func() *TCPConnection {
		conn := &TCPConnection{Conn: &fakes.TCPConn{
			LAddr:  net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 5060},
			RAddr:  net.TCPAddr{IP: net.ParseIP("127.0.0.2"), Port: 5060},
			Reader: bytes.NewBuffer(nil),
			Writer: bytes.NewBuffer(nil),
		}}
		conn.Ref(1)
		return conn
	}

	t.Run("Non2xxWaitsForACK", func(t *testing.T) {
		key, err := ServerTxKeyMake(req)
		require.NoError(t, err)

		handlerCalled := false
		txl := &TransactionLayer{
			clientTransactions: newTransactionStore[*ClientTx](),
			serverTransactions: newTransactionStore[*ServerTx](),
			reqHandler: func(_ *Request, _ *ServerTx) {
				handlerCalled = true
			},
			log: slog.Default(),
		}
		tx := NewServerTx(key, req, newConn(), slog.Default())
		require.True(t, tx.OnTerminate(txl.serverTxTerminate))
		txl.serverTransactions.items[key] = tx
		require.NoError(t, tx.Init())
		require.NoError(t, tx.Respond(NewResponseFromRequest(req, StatusTemporarilyUnavailable, "Temporarily Unavailable", nil)))

		gracefulDone := make(chan struct{})
		go func() {
			tx.TerminateGracefully()
			close(gracefulDone)
		}()

		select {
		case <-gracefulDone:
			t.Fatal("reliable INVITE server transaction terminated before receiving non-2xx ACK")
		case <-time.After(20 * time.Millisecond):
		}
		_, exists := txl.getServerTx(key)
		require.True(t, exists, "reliable INVITE transaction must remain registered until non-2xx ACK")

		ack := req.Clone()
		ack.Method = ACK
		ack.CSeq().MethodName = ACK
		require.NoError(t, txl.handleRequest(ack))
		require.False(t, handlerCalled, "matched non-2xx ACK must not reach the application request handler")

		require.Eventually(t, func() bool {
			select {
			case <-gracefulDone:
				return true
			default:
				return false
			}
		}, time.Second, time.Millisecond)
		require.NoError(t, compareFunctions(tx.currentFsmState(), tx.inviteStateTerminated))
		require.Eventually(t, func() bool {
			_, exists := txl.getServerTx(key)
			return !exists
		}, time.Second, time.Millisecond)
	})

	t.Run("2xxTerminatesImmediately", func(t *testing.T) {
		tx := NewServerTx("456", req, newConn(), slog.Default())
		require.NoError(t, tx.Init())
		require.NoError(t, tx.Respond(NewResponseFromRequest(req, StatusOK, "OK", nil)))

		tx.TerminateGracefully()

		select {
		case <-tx.Done():
		case <-time.After(time.Second):
			t.Fatal("reliable INVITE 2xx transaction did not terminate immediately")
		}
	})
}

func TestServerTransactionContext(t *testing.T) {
	req, _, _ := testCreateInvite(t, "sip:127.0.0.99:5060", "udp", "127.0.0.2:5060")
	tx := NewServerTx("123", req, nil, slog.Default())
	ctx := ServerTransactionContext(tx)
	tx.Terminate()
	require.Equal(t, context.Canceled, ctx.Err())
	require.Equal(t, ErrTransactionTerminated, tx.Err())
}

func TestServerTransactionReleasesConnRef(t *testing.T) {
	req := testCreateRequest(t, "OPTIONS", "sip:example.com", "UDP", "127.0.0.1:5060")
	conn := &UDPConnection{
		PacketConn: &fakes.UDPConn{
			Reader:  bytes.NewBuffer([]byte{}),
			Writers: map[string]io.Writer{},
		},
	}
	conn.Ref(2) // serverRequestConnection holds a reference before NewServerTx

	tx := NewServerTx("123", req, conn, slog.Default())
	require.NoError(t, tx.Init())

	tx.Terminate()
	<-tx.Done()

	require.Equal(t, 1, conn.Ref(0), "Terminate must release exactly one connection reference")
}
