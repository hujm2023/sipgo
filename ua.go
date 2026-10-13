package sipgo

import (
	"crypto/tls"
	"errors"
	"net"
	"time"

	"github.com/hujm2023/sipgo/sip"
)

// ErrUserAgentReliableWriteTimeout is returned by
// WithUserAgentReliableWriteTimeout for a non-positive timeout. There is no
// "0 means unlimited" contract: enabling the protection always requires a
// positive budget, and not passing the option leaves the legacy behavior.
var ErrUserAgentReliableWriteTimeout = errors.New("reliable write timeout must be positive")

type UserAgent struct {
	name        string
	hostname    string
	dnsResolver *net.Resolver
	tlsConfig   *tls.Config
	parser      *sip.Parser
	txOptions   []sip.TransactionLayerOption
	tpOptions   []sip.TransportLayerOption
	tp          *sip.TransportLayer
	tx          *sip.TransactionLayer

	// reliableWriteTimeout explicitly enables bounded TCP/TLS writes when > 0.
	reliableWriteTimeout time.Duration
}

type UserAgentOption func(s *UserAgent) error

// WithUserAgent changes user agent name
// Default: sipgo
func WithUserAgent(ua string) UserAgentOption {
	return func(s *UserAgent) error {
		s.name = ua
		return nil
	}
}

// WithUserAgentHostname represents FQDN of user that can be presented in From header
func WithUserAgentHostname(hostname string) UserAgentOption {
	return func(s *UserAgent) error {
		s.hostname = hostname
		return nil
	}
}

// WithUserAgentDNSResolver allows customizing default DNS resolver for transport layer
func WithUserAgentDNSResolver(r *net.Resolver) UserAgentOption {
	return func(s *UserAgent) error {
		s.dnsResolver = r
		return nil
	}
}

// WithUserAgenTLSConfig allows customizing default tls config.
func WithUserAgenTLSConfig(c *tls.Config) UserAgentOption {
	return func(s *UserAgent) error {
		s.tlsConfig = c
		return nil
	}
}

// WithUserAgentParser allows removing default behavior of parser
// You can define and remove default headers parser map and pass here.
// Only use if your benchmarks are better than default
func WithUserAgentParser(p *sip.Parser) UserAgentOption {
	return func(s *UserAgent) error {
		s.parser = p
		return nil
	}
}

// WithUserAgentTransactionLayerOptions allows setting options for the transaction layer
func WithUserAgentTransactionLayerOptions(o ...sip.TransactionLayerOption) UserAgentOption {
	return func(s *UserAgent) error {
		s.txOptions = o
		return nil
	}
}

// WithUserAgentTransportLayerOptions allows setting options for the transport layer
func WithUserAgentTransportLayerOptions(o ...sip.TransportLayerOption) UserAgentOption {
	return func(s *UserAgent) error {
		s.tpOptions = o
		return nil
	}
}

// WithUserAgentReliableWriteTimeout explicitly enables a write budget for every
// real TCP/TLS write of this UserAgent: outbound submits, inbound (accepted)
// responses and the automatic ACK/retransmission writes performed by the
// transaction FSMs. VoIPGo passes 2s.
//
// It is opt-in and off by default, so existing callers keep the legacy
// unbounded writes. Protocol timers (Timer A/B/D/H/J/M/L, retransmission
// windows, transaction identity) are untouched: this only bounds how long a
// single socket write may block. Because the protection is enforced by the
// connection owner, there is no per-call budget to reset and no per-transaction
// deadline to clear.
//
// A non-positive timeout is rejected instead of being silently interpreted as
// "unlimited".
func WithUserAgentReliableWriteTimeout(timeout time.Duration) UserAgentOption {
	return func(s *UserAgent) error {
		if timeout <= 0 {
			return ErrUserAgentReliableWriteTimeout
		}
		s.reliableWriteTimeout = timeout
		return nil
	}
}

// NewUA creates User Agent
// User Agent will create transport and transaction layer
// Check options for customizing user agent
func NewUA(options ...UserAgentOption) (*UserAgent, error) {
	ua := &UserAgent{
		name:        "sipgo",
		hostname:    "localhost",
		dnsResolver: net.DefaultResolver,
		parser:      sip.NewParser(),
	}

	for _, o := range options {
		if err := o(ua); err != nil {
			return nil, err
		}
	}

	// The reliable write protection is appended last so it survives any
	// WithUserAgentTransportLayerOptions replacement, without mutating the
	// caller's slice.
	tpOptions := ua.tpOptions
	if ua.reliableWriteTimeout > 0 {
		tpOptions = append(append([]sip.TransportLayerOption(nil), tpOptions...),
			sip.WithTransportLayerReliableWriteTimeout(ua.reliableWriteTimeout))
	}

	ua.tp = sip.NewTransportLayer(ua.dnsResolver, ua.parser, ua.tlsConfig, tpOptions...)
	ua.tx = sip.NewTransactionLayer(ua.tp, ua.txOptions...)
	return ua, nil
}

func (ua *UserAgent) Close() error {
	// stop transaction layer
	ua.tx.Close()

	// stop transport layer
	return ua.tp.Close()
}

func (ua *UserAgent) Name() string {
	return ua.name
}

func (ua *UserAgent) Hostname() string {
	return ua.hostname
}

func (ua *UserAgent) TransportLayer() *sip.TransportLayer {
	return ua.tp
}

func (ua *UserAgent) TransactionLayer() *sip.TransactionLayer {
	return ua.tx
}
