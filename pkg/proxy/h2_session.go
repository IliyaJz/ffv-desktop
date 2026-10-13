package proxy

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/http2"
)

const (
	defaultTunnelWriteBufSize    = 512 * 1024
	upstreamKeepAliveInterval    = 30 * time.Second
	upstreamKeepAlivePingTimeout = 10 * time.Second
)

var (
	errTunnelDeadline         = errors.New("deadlines are not supported for HTTP CONNECT tunnel streams")
	errProxyHTTP2Unavailable  = errors.New("upstream server did not negotiate HTTP/2")
)

type ProxySession interface {
	OpenTunnel(authority string) (net.Conn, error)
	IsAlive() bool
	UpdateToken(token string) error
	Close() error
}

type H2ProxySession struct {
	raw           net.Conn
	cc            *http2.ClientConn
	proxyHost     string
	token         string
	tokenMu       sync.RWMutex
	timeout       time.Duration
	alive         atomic.Bool
	stopKeepAlive context.CancelFunc
	closeOnce     sync.Once
	closeErr      error
}

func NewH2ProxySession(proxyURL *url.URL, token string, timeout time.Duration, dialer *UpstreamDialer) (*H2ProxySession, error) {
	proxyHost := proxyURL.Host
	if proxyURL.Port() == "" {
		proxyHost = net.JoinHostPort(proxyURL.Hostname(), "443")
	}

	dialTimeout := timeout
	if dialTimeout <= 0 {
		dialTimeout = 30 * time.Second
	}

	ctx, cancel := context.WithTimeout(context.Background(), dialTimeout)
	defer cancel()

	if dialer == nil {
		dialer = &UpstreamDialer{Timeout: dialTimeout}
	}

	rawConn, err := dialer.DialContext(ctx, "tcp", proxyHost)
	if err != nil {
		return nil, fmt.Errorf("dialing upstream %s: %w", proxyHost, err)
	}

	if tcpConn, ok := rawConn.(*net.TCPConn); ok {
		_ = tcpConn.SetReadBuffer(4 * 1024 * 1024)
		_ = tcpConn.SetWriteBuffer(4 * 1024 * 1024)
		_ = tcpConn.SetNoDelay(true)
		_ = tcpConn.SetKeepAlive(true)
		_ = tcpConn.SetKeepAlivePeriod(30 * time.Second)
	}

	tlsConfig := &tls.Config{
		ServerName: proxyURL.Hostname(),
		MinVersion: tls.VersionTLS12,
		NextProtos: []string{"h2"},
	}

	tlsConn := tls.Client(rawConn, tlsConfig)
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		_ = rawConn.Close()
		return nil, fmt.Errorf("tls handshake with %s: %w", proxyHost, err)
	}

	if tlsConn.ConnectionState().NegotiatedProtocol != "h2" {
		_ = tlsConn.Close()
		return nil, errProxyHTTP2Unavailable
	}

	transport := &http2.Transport{
		ReadIdleTimeout:  30 * time.Second,
		PingTimeout:      15 * time.Second,
		WriteByteTimeout: 30 * time.Second,
	}

	cc, err := transport.NewClientConn(tlsConn)
	if err != nil {
		_ = tlsConn.Close()
		return nil, fmt.Errorf("initiating http2 client conn: %w", err)
	}

	keepAliveCtx, cancelKeepAlive := context.WithCancel(context.Background())
	session := &H2ProxySession{
		raw:           tlsConn,
		cc:            cc,
		proxyHost:     proxyHost,
		token:         token,
		timeout:       timeout,
		stopKeepAlive: cancelKeepAlive,
	}
	session.alive.Store(true)
	go session.runKeepAlive(keepAliveCtx)

	return session, nil
}

func (s *H2ProxySession) runKeepAlive(ctx context.Context) {
	ticker := time.NewTicker(upstreamKeepAliveInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pingCtx, cancel := context.WithTimeout(ctx, upstreamKeepAlivePingTimeout)
			err := s.cc.Ping(pingCtx)
			cancel()
			if err == nil {
				continue
			}
			if ctx.Err() != nil {
				return
			}
			s.alive.Store(false)
			return
		}
	}
}

func (s *H2ProxySession) IsAlive() bool {
	if !s.alive.Load() {
		return false
	}
	return s.cc.CanTakeNewRequest()
}

func (s *H2ProxySession) bearerToken() string {
	s.tokenMu.RLock()
	defer s.tokenMu.RUnlock()
	return s.token
}

func (s *H2ProxySession) UpdateToken(token string) error {
	trimmed := strings.TrimSpace(token)
	if trimmed == "" {
		return errors.New("empty proxy session token")
	}
	s.tokenMu.Lock()
	s.token = trimmed
	s.tokenMu.Unlock()
	return nil
}

func (s *H2ProxySession) OpenTunnel(authority string) (net.Conn, error) {
	reqBody := newTunnelWriteBuffer(defaultTunnelWriteBufSize)
	resp, cancel, err := roundTripWithOpenTimeout(s.timeout, func(ctx context.Context) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodConnect, "http://"+authority, reqBody)
		if err != nil {
			return nil, err
		}
		req.Host = authority
		req.URL.Host = s.proxyHost
		req.Header.Set("Proxy-Authorization", "Bearer "+s.bearerToken())
		return s.cc.RoundTrip(req)
	})
	if err != nil {
		reqBody.failRead(io.ErrClosedPipe)
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		cancel()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		_ = resp.Body.Close()
		reqBody.failRead(io.ErrClosedPipe)
		return nil, fmt.Errorf("upstream CONNECT %s returned HTTP %d: %s", authority, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	return &tunnelConn{
		reader:  resp.Body,
		writer:  reqBody,
		reqBody: reqBody,
		name:    tunnelAddr(authority),
		cancel:  cancel,
	}, nil
}

func (s *H2ProxySession) Close() error {
	s.closeOnce.Do(func() {
		s.alive.Store(false)
		if s.stopKeepAlive != nil {
			s.stopKeepAlive()
		}
		if s.cc != nil {
			_ = s.cc.Close()
		}
		if s.raw != nil {
			s.closeErr = s.raw.Close()
		}
	})
	return s.closeErr
}

type roundTripResult struct {
	resp *http.Response
	err  error
}

func roundTripWithOpenTimeout(timeout time.Duration, do func(context.Context) (*http.Response, error)) (*http.Response, context.CancelFunc, error) {
	ctx, cancel := context.WithCancel(context.Background())
	if timeout <= 0 {
		resp, err := do(ctx)
		if err != nil {
			cancel()
			return nil, nil, err
		}
		return resp, cancel, nil
	}

	resultCh := make(chan roundTripResult, 1)
	go func() {
		resp, err := do(ctx)
		resultCh <- roundTripResult{resp: resp, err: err}
	}()

	timer := time.NewTimer(timeout)
	select {
	case result := <-resultCh:
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		if result.err != nil {
			cancel()
			return nil, nil, result.err
		}
		return result.resp, cancel, nil
	case <-timer.C:
		cancel()
		return nil, nil, fmt.Errorf("connect timeout after %s", timeout)
	}
}

// tunnelWriteBuffer is a bounded, concurrency-safe ring buffer used as the
// request body of a CONNECT tunnel.
type tunnelWriteBuffer struct {
	mu       sync.Mutex
	notEmpty *sync.Cond
	notFull  *sync.Cond
	buf      []byte
	n        int
	off      int
	closed   bool
	readErr  error
}

func newTunnelWriteBuffer(size int) *tunnelWriteBuffer {
	if size < 1 {
		size = defaultTunnelWriteBufSize
	}
	b := &tunnelWriteBuffer{buf: make([]byte, size)}
	b.notEmpty = sync.NewCond(&b.mu)
	b.notFull = sync.NewCond(&b.mu)
	return b
}

func (b *tunnelWriteBuffer) Read(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	for b.n == 0 && b.readErr == nil && !b.closed {
		b.notEmpty.Wait()
	}
	if b.n == 0 {
		if b.readErr != nil {
			return 0, b.readErr
		}
		return 0, io.EOF
	}

	end := b.off + b.n
	if end > len(b.buf) {
		end = len(b.buf)
	}
	n := copy(p, b.buf[b.off:end])
	b.off = (b.off + n) % len(b.buf)
	b.n -= n
	b.notFull.Broadcast()
	return n, nil
}

func (b *tunnelWriteBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	total := 0
	for len(p) > 0 {
		for b.n == len(b.buf) && b.readErr == nil && !b.closed {
			b.notFull.Wait()
		}
		if b.readErr != nil {
			return total, b.readErr
		}
		if b.closed {
			return total, io.ErrClosedPipe
		}

		space := len(b.buf) - b.n
		head := (b.off + b.n) % len(b.buf)
		writable := len(b.buf) - head
		if space < writable {
			writable = space
		}
		if writable > len(p) {
			writable = len(p)
		}
		copy(b.buf[head:], p[:writable])
		b.n += writable
		p = p[writable:]
		total += writable
		b.notEmpty.Broadcast()
	}
	return total, nil
}

func (b *tunnelWriteBuffer) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil
	}
	b.closed = true
	b.notEmpty.Broadcast()
	b.notFull.Broadcast()
	return nil
}

func (b *tunnelWriteBuffer) failRead(err error) {
	b.mu.Lock()
	if b.readErr == nil {
		b.readErr = err
	}
	b.closed = true
	b.notEmpty.Broadcast()
	b.notFull.Broadcast()
	b.mu.Unlock()
}

type tunnelConn struct {
	reader    io.ReadCloser
	writer    *tunnelWriteBuffer
	reqBody   *tunnelWriteBuffer
	cancel    context.CancelFunc
	name      tunnelAddr
	closeOnce sync.Once
	readOnce  sync.Once
	writeOnce sync.Once
	closeErr  error
	readErr   error
	writeErr  error
}

func (c *tunnelConn) Read(p []byte) (int, error)  { return c.reader.Read(p) }
func (c *tunnelConn) Write(p []byte) (int, error) { return c.writer.Write(p) }

func (c *tunnelConn) Close() error {
	c.closeOnce.Do(func() {
		writeErr := c.CloseWrite()
		readErr := c.CloseRead()
		if c.reqBody != nil {
			c.reqBody.failRead(io.ErrClosedPipe)
		}
		if c.cancel != nil {
			c.cancel()
		}
		if writeErr != nil {
			c.closeErr = writeErr
		} else {
			c.closeErr = readErr
		}
	})
	return c.closeErr
}

func (c *tunnelConn) CloseRead() error {
	c.readOnce.Do(func() {
		if c.reader != nil {
			c.readErr = c.reader.Close()
		}
	})
	return c.readErr
}

func (c *tunnelConn) CloseWrite() error {
	c.writeOnce.Do(func() {
		if c.writer != nil {
			c.writeErr = c.writer.Close()
		}
	})
	return c.writeErr
}

func (c *tunnelConn) LocalAddr() net.Addr              { return c.name }
func (c *tunnelConn) RemoteAddr() net.Addr             { return c.name }
func (c *tunnelConn) SetDeadline(time.Time) error      { return errTunnelDeadline }
func (c *tunnelConn) SetReadDeadline(time.Time) error  { return errTunnelDeadline }
func (c *tunnelConn) SetWriteDeadline(time.Time) error { return errTunnelDeadline }

type tunnelAddr string

func (a tunnelAddr) Network() string { return "tcp" }
func (a tunnelAddr) String() string  { return string(a) }
