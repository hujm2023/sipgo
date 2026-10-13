package sip

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
)

var ()

// TLS transport implementation
type TransportTLS struct {
	*TransportTCP

	// rootPool *x509.CertPool
	tlsClient func(conn net.Conn, hostname string) *tls.Conn
}

func (t *TransportTLS) init(par *Parser, dialTLSConf *tls.Config) {
	t.TransportTCP.init(par)
	t.transport = "TLS"
	// p.rootPool = roots
	t.tlsClient = func(conn net.Conn, hostname string) *tls.Conn {
		config := dialTLSConf

		if config.ServerName == "" {
			config = config.Clone()
			config.ServerName = hostname
		}
		return tls.Client(conn, config)
	}
}

func (t *TransportTLS) String() string {
	return "Transport<TLS>"
}

// CreateConnection creates TLS connection for TCP transport
func (t *TransportTLS) CreateConnection(ctx context.Context, laddr Addr, raddr Addr, handler MessageHandler) (Connection, error) {
	conn, err := t.pool.addSingleflightRead(ctx, raddr, laddr, t.connectionReuse, func(ctx context.Context) (Connection, error) {
		hostname := raddr.Hostname
		if hostname == "" {
			hostname = raddr.IP.String()
		}

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

		netDialer := t.DialerCreate(tladdr)

		addr := traddr.String()
		t.log.Debug("Dialing new connection", "raddr", addr)
		// No resolving should happen here
		conn, err := netDialer.DialContext(ctx, "tcp", addr)
		if err != nil {
			return nil, fmt.Errorf("dial TCP error: %w", err)
		}

		tlsConn := t.tlsClient(conn, hostname)

		if err := tlsConn.HandshakeContext(ctx); err != nil {
			_ = tlsConn.Close()
			return nil, fmt.Errorf("TLS handshake error: %w", err)
		}

		t.log.Debug("New connection", "raddr", raddr)
		c := t.newConnection(tlsConn, 2+TransportIdleConnection)
		return c, nil
	}, func(conn Connection) {
		c := conn.(*TCPConnection)
		t.readConnection(c, c.LocalAddr().String(), c.RemoteAddr().String(), handler)
	})
	if err != nil {
		return nil, err
	}
	c := conn.(*TCPConnection)
	return c, nil
}
