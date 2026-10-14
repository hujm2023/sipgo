package sip

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gobwas/ws"
	"github.com/stretchr/testify/require"
)

// The public CreateConnection API takes local then remote, whereas its pool
// takes remote then local. Exercise creation and reuse with distinct addresses
// so swapping the pool arguments cannot silently cache the local address.
func TestWSSConnectionReusesRemoteAlias(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	var upgrades atomic.Int64
	peersDone := make(chan struct{}, 2)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _, _, err := ws.UpgradeHTTP(r, w)
		if err != nil {
			t.Errorf("WSS upgrade failed: %v", err)
			return
		}
		defer func() { _ = conn.Close(); peersDone <- struct{}{} }()
		upgrades.Add(1)
		<-release
	}))
	tr := &TransportWSS{TransportWS: &TransportWS{connectionReuse: true}}
	tr.init(NewParser(), &tls.Config{InsecureSkipVerify: true})
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		_ = tr.Close()
		server.Close()
		for i := int64(0); i < upgrades.Load(); i++ {
			select {
			case <-peersDone:
			case <-time.After(time.Second):
				t.Error("WSS peer did not exit")
			}
		}
	})
	remote := server.Listener.Addr().(*net.TCPAddr)
	raddr := Addr{IP: remote.IP, Port: remote.Port}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	first, err := tr.CreateConnection(ctx, Addr{}, raddr, func(Message) {})
	require.NoError(t, err)
	defer first.TryClose()
	cached := tr.GetConnection(raddr.String())
	require.NotNil(t, cached, "created WSS connection must be indexed by remote address")
	defer cached.TryClose()
	require.Same(t, first, cached)
	second, err := tr.CreateConnection(ctx, Addr{}, raddr, func(Message) {})
	require.NoError(t, err)
	defer second.TryClose()
	require.Same(t, first, second, "same remote must reuse the established WSS connection")
	require.Equal(t, int64(1), upgrades.Load())
}
