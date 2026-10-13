package proxy

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sync"
	"time"
)

const (
	socksVersion5        = 0x05
	socksAuthNoAuth      = 0x00
	socksAuthNoAccept    = 0xff
	socksCmdConnect      = 0x01
	socksCmdUDPAssociate = 0x03
	socksAtypIPv4        = 0x01
	socksAtypDomain      = 0x03
	socksAtypIPv6        = 0x04

	socksReplySuccess   = 0x00
	socksReplyFailure   = 0x01
	socksReplyHostUnrch = 0x04
	socksReplyCmdUnsup  = 0x07
	socksReplyAtypUnsup = 0x08

	halfCloseDrainTimeout = 5 * time.Second
)

var (
	errUnsupportedAddrType = errors.New("unsupported SOCKS address type")
	copyBufferPool         = sync.Pool{
		New: func() any {
			return make([]byte, 128*1024)
		},
	}
)

type TunnelOpener interface {
	OpenTunnel(authority string) (net.Conn, error)
}

type dnsCacheEntry struct {
	response []byte
	expires  time.Time
}

type SocksServer struct {
	listener         net.Listener
	upstream         TunnelOpener
	handshakeTimeout time.Duration
	idleTimeout      time.Duration
	verbose          bool
	closeOnce        sync.Once
	closed           chan struct{}
	dnsCache         map[string]dnsCacheEntry
	dnsCacheMu       sync.RWMutex
}

func NewSocksServer(upstream TunnelOpener, handshakeTimeout, idleTimeout time.Duration, verbose bool) *SocksServer {
	if handshakeTimeout <= 0 {
		handshakeTimeout = 10 * time.Second
	}
	return &SocksServer{
		upstream:         upstream,
		handshakeTimeout: handshakeTimeout,
		idleTimeout:      idleTimeout,
		verbose:          verbose,
		closed:           make(chan struct{}),
		dnsCache:         make(map[string]dnsCacheEntry),
	}
}

func (s *SocksServer) ListenAndServe(addr string) error {
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	s.listener = l

	for {
		conn, err := l.Accept()
		if err != nil {
			select {
			case <-s.closed:
				return nil
			default:
				return err
			}
		}
		go s.handleConn(conn)
	}
}

func (s *SocksServer) Addr() net.Addr {
	if s.listener != nil {
		return s.listener.Addr()
	}
	return nil
}

func (s *SocksServer) Close() error {
	s.closeOnce.Do(func() {
		close(s.closed)
		if s.listener != nil {
			_ = s.listener.Close()
		}
	})
	return nil
}

func (s *SocksServer) handleConn(conn net.Conn) {
	defer conn.Close()
	clientAddr := conn.RemoteAddr().String()

	if s.handshakeTimeout > 0 {
		_ = conn.SetDeadline(time.Now().Add(s.handshakeTimeout))
	}

	cmd, target, replyCode, err := handshakeSOCKS5(conn)
	if err != nil {
		if replyCode != 0 {
			_ = writeSOCKSReply(conn, replyCode, nil)
		}
		return
	}

	if s.handshakeTimeout > 0 {
		_ = conn.SetDeadline(time.Time{})
	}

	if cmd == socksCmdUDPAssociate {
		s.handleUDPAssociate(conn)
		return
	}

	if s.verbose {
		log.Printf("[SOCKS5] CONNECT client=%s target=%s", clientAddr, target)
	}

	upstreamConn, err := s.upstream.OpenTunnel(target)
	if err != nil {
		_ = writeSOCKSReply(conn, socksReplyFailure, nil)
		if s.verbose {
			log.Printf("[SOCKS5] OpenTunnel error client=%s target=%s: %v", clientAddr, target, err)
		}
		return
	}
	defer upstreamConn.Close()

	if err := writeSOCKSReply(conn, socksReplySuccess, conn.LocalAddr()); err != nil {
		return
	}

	proxyBidirectional(conn, upstreamConn, s.idleTimeout)
}

func (s *SocksServer) handleUDPAssociate(tcpConn net.Conn) {
	clientAddr := tcpConn.RemoteAddr().String()
	if s.verbose {
		log.Printf("[SOCKS5] UDP ASSOCIATE requested by %s", clientAddr)
	}

	bindIP := net.ParseIP("127.0.0.1")
	if localTCP, ok := tcpConn.LocalAddr().(*net.TCPAddr); ok && localTCP.IP != nil {
		bindIP = localTCP.IP
	}

	udpConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: bindIP, Port: 0})
	if err != nil {
		_ = writeSOCKSReply(tcpConn, socksReplyFailure, nil)
		if s.verbose {
			log.Printf("[SOCKS5] UDP ASSOCIATE listen failed: %v", err)
		}
		return
	}
	defer udpConn.Close()

	boundAddr := udpConn.LocalAddr().(*net.UDPAddr)
	if s.verbose {
		log.Printf("[SOCKS5] UDP ASSOCIATE relay bound to %s", boundAddr.String())
	}

	if err := writeSOCKSReply(tcpConn, socksReplySuccess, boundAddr); err != nil {
		return
	}

	// RFC 1928: A UDP association terminates when the TCP connection terminates.
	closed := make(chan struct{})
	go func() {
		discard := make([]byte, 512)
		for {
			_, err := tcpConn.Read(discard)
			if err != nil {
				break
			}
		}
		close(closed)
		_ = udpConn.Close()
	}()

	buf := make([]byte, 65535)
	for {
		n, senderAddr, err := udpConn.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-closed:
				return
			case <-s.closed:
				return
			default:
				return
			}
		}

		if n < 4 {
			continue
		}

		// FRAG byte at index 2
		if buf[2] != 0 {
			continue // Drop fragmented SOCKS5 UDP packets (RFC 1928 allows ignoring fragments)
		}

		atyp := buf[3]
		var dstHost string
		var dataOffset int

		switch atyp {
		case socksAtypIPv4:
			if n < 10 {
				continue
			}
			dstHost = net.IP(buf[4:8]).String()
			dataOffset = 10
		case socksAtypDomain:
			if n < 5 {
				continue
			}
			dLen := int(buf[4])
			if n < 5+dLen+2 {
				continue
			}
			dstHost = string(buf[5 : 5+dLen])
			dataOffset = 5 + dLen + 2
		case socksAtypIPv6:
			if n < 22 {
				continue
			}
			dstHost = net.IP(buf[4:20]).String()
			dataOffset = 22
		default:
			continue
		}

		dstPort := binary.BigEndian.Uint16(buf[dataOffset-2 : dataOffset])
		payload := buf[dataOffset:n]
		rawTarget := net.JoinHostPort(dstHost, fmt.Sprintf("%d", dstPort))

		hdrCopy := append([]byte(nil), buf[:dataOffset]...)
		payloadCopy := append([]byte(nil), payload...)

		if dstPort == 53 {
			// DNS Query: resolve via encrypted VPN tunnel
			go func(cAddr *net.UDPAddr, tgt string, hdr, query []byte) {
				dnsResp, err := s.resolveDNS(tgt, query)
				if err != nil {
					if s.verbose {
						log.Printf("[SOCKS5] DNS resolution to %s failed: %v", tgt, err)
					}
					return
				}
				respPacket := make([]byte, len(hdr)+len(dnsResp))
				copy(respPacket, hdr)
				copy(respPacket[len(hdr):], dnsResp)
				_, _ = udpConn.WriteToUDP(respPacket, cAddr)
			}(senderAddr, rawTarget, hdrCopy, payloadCopy)
		} else {
			// Non-DNS UDP: best-effort direct relay
			go func(cAddr *net.UDPAddr, tgt string, hdr, data []byte) {
				rAddr, err := net.ResolveUDPAddr("udp", tgt)
				if err != nil {
					return
				}
				rConn, err := net.DialUDP("udp", nil, rAddr)
				if err != nil {
					return
				}
				defer rConn.Close()
				_ = rConn.SetDeadline(time.Now().Add(4 * time.Second))

				if _, err := rConn.Write(data); err != nil {
					return
				}

				respBuf := make([]byte, 65535)
				rn, err := rConn.Read(respBuf)
				if err != nil || rn <= 0 {
					return
				}

				respPacket := make([]byte, len(hdr)+rn)
				copy(respPacket, hdr)
				copy(respPacket[len(hdr):], respBuf[:rn])
				_, _ = udpConn.WriteToUDP(respPacket, cAddr)
			}(senderAddr, rawTarget, hdrCopy, payloadCopy)
		}
	}
}

func (s *SocksServer) resolveDNS(target string, query []byte) ([]byte, error) {
	if len(query) < 12 {
		return nil, errors.New("DNS query too short")
	}

	cacheKey := string(query[2:]) // strip 16-bit transaction ID
	if cached := s.dnsCacheGet(cacheKey, query[:2]); cached != nil {
		return cached, nil
	}

	// 1. Try standard DNS over TCP on port 53 via upstream tunnel
	resp, err := s.resolveDNSOverTCP(target, query, 4*time.Second)
	if err == nil {
		s.dnsCachePut(cacheKey, resp, 30*time.Second)
		return resp, nil
	}

	if s.verbose {
		log.Printf("[SOCKS5] DNS over TCP to %s failed (%v), falling back to DoH...", target, err)
	}

	// 2. Fallback to DNS over HTTPS (port 443) through the tunnel
	resp, err = s.resolveDNSOverHTTPS("1.1.1.1:443", query, 5*time.Second)
	if err == nil {
		s.dnsCachePut(cacheKey, resp, 30*time.Second)
		return resp, nil
	}

	return nil, fmt.Errorf("DNS resolution failed: %w", err)
}

func (s *SocksServer) resolveDNSOverTCP(target string, query []byte, timeout time.Duration) ([]byte, error) {
	conn, err := s.upstream.OpenTunnel(target)
	if err != nil {
		return nil, fmt.Errorf("open tunnel to %s: %w", target, err)
	}
	defer conn.Close()

	if timeout > 0 {
		_ = conn.SetDeadline(time.Now().Add(timeout))
	}

	queryLen := uint16(len(query))
	sendBuf := make([]byte, 2+len(query))
	binary.BigEndian.PutUint16(sendBuf[:2], queryLen)
	copy(sendBuf[2:], query)

	if _, err := conn.Write(sendBuf); err != nil {
		return nil, fmt.Errorf("write DNS TCP query: %w", err)
	}

	var respLenBuf [2]byte
	if _, err := io.ReadFull(conn, respLenBuf[:]); err != nil {
		return nil, fmt.Errorf("read DNS TCP response length: %w", err)
	}
	respLen := binary.BigEndian.Uint16(respLenBuf[:])
	if respLen == 0 || respLen > 4096 {
		return nil, fmt.Errorf("invalid DNS TCP response length %d", respLen)
	}

	respData := make([]byte, respLen)
	if _, err := io.ReadFull(conn, respData); err != nil {
		return nil, fmt.Errorf("read DNS TCP response data: %w", err)
	}

	return respData, nil
}

func (s *SocksServer) resolveDNSOverHTTPS(dohServer string, query []byte, timeout time.Duration) ([]byte, error) {
	rawConn, err := s.upstream.OpenTunnel(dohServer)
	if err != nil {
		return nil, fmt.Errorf("open tunnel to DoH server %s: %w", dohServer, err)
	}
	defer rawConn.Close()

	if timeout > 0 {
		_ = rawConn.SetDeadline(time.Now().Add(timeout))
	}

	tlsConn := tls.Client(rawConn, &tls.Config{
		ServerName: "cloudflare-dns.com",
		MinVersion: tls.VersionTLS12,
	})
	if err := tlsConn.Handshake(); err != nil {
		return nil, fmt.Errorf("DoH TLS handshake: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, "https://"+dohServer+"/dns-query", bytes.NewReader(query))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/dns-message")
	req.Header.Set("Accept", "application/dns-message")
	req.Header.Set("Host", "cloudflare-dns.com")

	if err := req.Write(tlsConn); err != nil {
		return nil, fmt.Errorf("writing DoH request: %w", err)
	}

	resp, err := http.ReadResponse(bufio.NewReader(tlsConn), req)
	if err != nil {
		return nil, fmt.Errorf("reading DoH response: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("DoH returned HTTP status %d", resp.StatusCode)
	}

	respData, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return nil, fmt.Errorf("reading DoH response body: %w", err)
	}

	if len(respData) < 12 {
		return nil, errors.New("DoH response too short")
	}

	respData[0] = query[0]
	respData[1] = query[1]
	return respData, nil
}

func (s *SocksServer) dnsCacheGet(key string, txID []byte) []byte {
	s.dnsCacheMu.RLock()
	entry, ok := s.dnsCache[key]
	s.dnsCacheMu.RUnlock()

	if !ok || time.Now().After(entry.expires) {
		return nil
	}

	resp := make([]byte, len(entry.response))
	copy(resp, entry.response)
	if len(resp) >= 2 && len(txID) >= 2 {
		resp[0] = txID[0]
		resp[1] = txID[1]
	}
	return resp
}

func (s *SocksServer) dnsCachePut(key string, response []byte, ttl time.Duration) {
	s.dnsCacheMu.Lock()
	defer s.dnsCacheMu.Unlock()

	if s.dnsCache == nil {
		s.dnsCache = make(map[string]dnsCacheEntry)
	}
	if len(s.dnsCache) > 1024 {
		now := time.Now()
		for k, v := range s.dnsCache {
			if now.After(v.expires) {
				delete(s.dnsCache, k)
			}
		}
		if len(s.dnsCache) > 1024 {
			s.dnsCache = make(map[string]dnsCacheEntry)
		}
	}
	s.dnsCache[key] = dnsCacheEntry{
		response: append([]byte(nil), response...),
		expires:  time.Now().Add(ttl),
	}
}

func handshakeSOCKS5(conn net.Conn) (byte, string, byte, error) {
	header := make([]byte, 2)
	if _, err := io.ReadFull(conn, header); err != nil {
		return 0, "", 0, err
	}
	if header[0] != socksVersion5 {
		return 0, "", 0, fmt.Errorf("unexpected SOCKS version %d", header[0])
	}

	methods := make([]byte, int(header[1]))
	if _, err := io.ReadFull(conn, methods); err != nil {
		return 0, "", 0, err
	}

	method := byte(socksAuthNoAccept)
	for _, candidate := range methods {
		if candidate == socksAuthNoAuth {
			method = socksAuthNoAuth
			break
		}
	}
	if _, err := conn.Write([]byte{socksVersion5, method}); err != nil {
		return 0, "", 0, err
	}
	if method == socksAuthNoAccept {
		return 0, "", 0, fmt.Errorf("no supported SOCKS auth methods")
	}

	reqHeader := make([]byte, 4)
	if _, err := io.ReadFull(conn, reqHeader); err != nil {
		return 0, "", 0, err
	}
	if reqHeader[0] != socksVersion5 {
		return 0, "", socksReplyFailure, fmt.Errorf("unexpected request version %d", reqHeader[0])
	}
	cmd := reqHeader[1]
	if cmd != socksCmdConnect && cmd != socksCmdUDPAssociate {
		return cmd, "", socksReplyCmdUnsup, fmt.Errorf("unsupported SOCKS command %d", cmd)
	}

	host, err := readSOCKSAddr(conn, reqHeader[3])
	if err != nil {
		if errors.Is(err, errUnsupportedAddrType) {
			return cmd, "", socksReplyAtypUnsup, err
		}
		return cmd, "", socksReplyFailure, err
	}

	portBytes := make([]byte, 2)
	if _, err := io.ReadFull(conn, portBytes); err != nil {
		return cmd, "", socksReplyFailure, err
	}
	port := binary.BigEndian.Uint16(portBytes)
	return cmd, net.JoinHostPort(host, fmt.Sprintf("%d", port)), socksReplySuccess, nil
}

func readSOCKSAddr(r io.Reader, atyp byte) (string, error) {
	switch atyp {
	case socksAtypIPv4:
		buf := make([]byte, net.IPv4len)
		if _, err := io.ReadFull(r, buf); err != nil {
			return "", err
		}
		return net.IP(buf).String(), nil
	case socksAtypIPv6:
		buf := make([]byte, net.IPv6len)
		if _, err := io.ReadFull(r, buf); err != nil {
			return "", err
		}
		return net.IP(buf).String(), nil
	case socksAtypDomain:
		var size [1]byte
		if _, err := io.ReadFull(r, size[:]); err != nil {
			return "", err
		}
		buf := make([]byte, int(size[0]))
		if _, err := io.ReadFull(r, buf); err != nil {
			return "", err
		}
		return string(buf), nil
	default:
		return "", errUnsupportedAddrType
	}
}

func writeSOCKSReply(w io.Writer, rep byte, addr net.Addr) error {
	host := "0.0.0.0"
	port := 0
	if addr != nil {
		switch a := addr.(type) {
		case *net.TCPAddr:
			host = a.IP.String()
			port = a.Port
		case *net.UDPAddr:
			host = a.IP.String()
			port = a.Port
		}
	}

	ip := net.ParseIP(host)
	atyp := byte(socksAtypIPv4)
	addrBytes := []byte{0, 0, 0, 0}
	if ip != nil && ip.To4() != nil {
		addrBytes = ip.To4()
	} else if ip != nil && ip.To16() != nil {
		atyp = socksAtypIPv6
		addrBytes = ip.To16()
	}

	reply := make([]byte, 0, 6+len(addrBytes))
	reply = append(reply, socksVersion5, rep, 0x00, atyp)
	reply = append(reply, addrBytes...)
	reply = append(reply, byte(port>>8), byte(port))
	_, err := w.Write(reply)
	return err
}

func proxyBidirectional(clientConn, upstreamConn net.Conn, idleTimeout time.Duration) {
	var activity func()
	done := make(chan struct{})

	if idleTimeout > 0 {
		activityCh := make(chan struct{}, 1)
		activity = func() {
			select {
			case activityCh <- struct{}{}:
			default:
			}
		}
		go closeIdleConns(clientConn, upstreamConn, idleTimeout, activityCh, done)
	} else {
		activity = func() {}
	}

	results := make(chan error, 2)
	go func() {
		err := copyConn(upstreamConn, clientConn, activity)
		if err == nil {
			closeWrite(upstreamConn)
		}
		results <- err
	}()

	go func() {
		err := copyConn(clientConn, upstreamConn, activity)
		if err == nil {
			closeWrite(clientConn)
		}
		results <- err
	}()

	firstErr := <-results
	if firstErr != nil {
		_ = clientConn.Close()
		_ = upstreamConn.Close()
	}
	var secondErr error
	if firstErr == nil {
		drainTimer := time.NewTimer(halfCloseDrainTimeout)
		select {
		case secondErr = <-results:
			if !drainTimer.Stop() {
				<-drainTimer.C
			}
		case <-drainTimer.C:
			_ = clientConn.Close()
			_ = upstreamConn.Close()
			secondErr = <-results
		}
	} else {
		secondErr = <-results
	}
	if secondErr != nil {
		_ = clientConn.Close()
		_ = upstreamConn.Close()
	}
	close(done)
}

func closeIdleConns(clientConn, upstreamConn net.Conn, idleTimeout time.Duration, activity <-chan struct{}, done <-chan struct{}) {
	timer := time.NewTimer(idleTimeout)
	defer timer.Stop()
	for {
		select {
		case <-activity:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(idleTimeout)
		case <-timer.C:
			_ = clientConn.Close()
			_ = upstreamConn.Close()
			return
		case <-done:
			return
		}
	}
}

func copyConn(dst, src net.Conn, activity func()) error {
	buf := copyBufferPool.Get().([]byte)
	defer copyBufferPool.Put(buf)

	for {
		nr, readErr := src.Read(buf)
		if nr > 0 {
			activity()
			nw, writeErr := dst.Write(buf[:nr])
			if nw > 0 {
				activity()
			}
			if writeErr != nil {
				return writeErr
			}
			if nw != nr {
				return io.ErrShortWrite
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return nil
			}
			return readErr
		}
	}
}

func closeWrite(conn net.Conn) {
	if closeWriter, ok := conn.(interface{ CloseWrite() error }); ok {
		_ = closeWriter.CloseWrite()
	}
}
