package proxy

import (
	"encoding/binary"
	"io"
	"net"
	"testing"
)

type mockTunnelOpener struct {
	openFunc func(authority string) (net.Conn, error)
}

func (m *mockTunnelOpener) OpenTunnel(authority string) (net.Conn, error) {
	if m.openFunc != nil {
		return m.openFunc(authority)
	}
	return nil, net.ErrClosed
}

func TestHandshakeSOCKS5(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	// Client goroutine simulating SOCKS5 client
	go func() {
		// 1. Send auth method request
		if _, err := client.Write([]byte{socksVersion5, 0x01, socksAuthNoAuth}); err != nil {
			return
		}

		// 2. Read server auth choice
		authChoice := make([]byte, 2)
		if _, err := io.ReadFull(client, authChoice); err != nil {
			return
		}

		// 3. Send UDP ASSOCIATE request
		req := []byte{socksVersion5, socksCmdUDPAssociate, 0x00, socksAtypIPv4, 0, 0, 0, 0, 0, 0}
		_, _ = client.Write(req)
	}()

	cmd, target, replyCode, err := handshakeSOCKS5(server)
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}
	if replyCode != socksReplySuccess {
		t.Fatalf("expected success reply code, got %d", replyCode)
	}
	if cmd != socksCmdUDPAssociate {
		t.Fatalf("expected CMD UDP ASSOCIATE (3), got %d", cmd)
	}
	_ = target
}

func TestWriteSOCKSReply(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	go func() {
		_ = writeSOCKSReply(server, socksReplySuccess, &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 12345})
	}()

	resp := make([]byte, 10)
	if _, err := io.ReadFull(client, resp); err != nil {
		t.Fatalf("read reply: %v", err)
	}
	if resp[0] != socksVersion5 || resp[1] != socksReplySuccess {
		t.Fatalf("unexpected header: %v", resp[:2])
	}
	port := binary.BigEndian.Uint16(resp[8:10])
	if port != 12345 {
		t.Fatalf("expected port 12345, got %d", port)
	}
}
