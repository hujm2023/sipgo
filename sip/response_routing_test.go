package sip

import (
	"context"
	"net"
	"testing"

	"github.com/hujm2023/sipgo/fakes"
	"github.com/stretchr/testify/require"
)

func TestResponseTargetFromVia(t *testing.T) {
	t.Run("maddr wins over received and rport", func(t *testing.T) {
		params := NewParams()
		params.Add("maddr", "198.51.100.20")
		params.Add("received", "203.0.113.10")
		params.Add("rport", "5088")

		host, port := responseTargetFromVia("UDP", &ViaHeader{
			Host:   "192.0.2.10",
			Port:   5070,
			Params: params,
		})

		require.Equal(t, "198.51.100.20", host)
		require.Equal(t, 5070, port)
	})

	t.Run("received and rport remain the fallback without maddr", func(t *testing.T) {
		params := NewParams()
		params.Add("received", "203.0.113.10")
		params.Add("rport", "5088")

		host, port := responseTargetFromVia("UDP", &ViaHeader{
			Host:   "192.0.2.10",
			Port:   5070,
			Params: params,
		})

		require.Equal(t, "203.0.113.10", host)
		require.Equal(t, 5088, port)
	})

	t.Run("falls back to sent-by when no override is present", func(t *testing.T) {
		host, port := responseTargetFromVia("UDP", &ViaHeader{
			Host: "192.0.2.10",
			Port: 5070,
		})

		require.Equal(t, "192.0.2.10", host)
		require.Equal(t, 5070, port)
	})
}

func TestResponseDestinationHonorsMaddr(t *testing.T) {
	params := NewParams()
	params.Add("maddr", "198.51.100.20")
	params.Add("received", "203.0.113.10")
	params.Add("rport", "5088")

	res := NewResponse(StatusOK, "OK")
	res.SetTransport("UDP")
	res.AppendHeader(&ViaHeader{
		Host:   "192.0.2.10",
		Port:   5070,
		Params: params,
	})

	require.Equal(t, "198.51.100.20:5070", res.Destination())
}

func TestServerRequestConnectionUsesMaddrForResolvedResponseAddr(t *testing.T) {
	tp := NewTransportLayer(net.DefaultResolver, NewParser(), nil)

	fakePacketConn := &fakes.UDPConn{
		LAddr: net.UDPAddr{
			IP:   net.ParseIP("127.0.0.1"),
			Port: 5060,
		},
		RAddr: net.UDPAddr{
			IP:   net.ParseIP("203.0.113.10"),
			Port: 5088,
		},
	}

	conn := &UDPConnection{
		PacketConn: fakePacketConn,
		PacketAddr: fakePacketConn.LocalAddr().String(),
		Listener:   true,
	}
	tp.udp.pool.Add("203.0.113.10:5088", conn)

	params := NewParams()
	params.Add("branch", GenerateBranch())
	params.Add("maddr", "198.51.100.20")

	req := NewRequest(OPTIONS, Uri{
		Scheme: "sip",
		Host:   "service.example.com",
		Port:   5060,
	})
	req.SetTransport("UDP")
	req.SetSource("203.0.113.10:5088")
	req.AppendHeader(&ViaHeader{
		Host:   "192.0.2.10",
		Port:   5070,
		Params: params,
	})

	gotConn, err := tp.serverRequestConnection(context.Background(), req)
	require.NoError(t, err)
	require.Same(t, conn, gotConn)
	require.Equal(t, "198.51.100.20:5070", req.raddr.String())
}
