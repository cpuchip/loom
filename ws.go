package loom

// A minimal RFC 6455 websocket, pure stdlib — no gorilla, no golang.org/x/net.
// loom's go.mod has no require block and that is a point of pride, so the transport
// is hand-rolled: net/http hijack for the server upgrade, crypto/sha1 + base64 for
// the accept key, crypto/rand for the client mask. Text frames only; one JSON
// message per frame (we reject fragmentation and binary rather than reassemble).

import (
	"bufio"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

// wsMagic is the RFC 6455 GUID appended to the client key before the accept hash.
const wsMagic = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// maxFrame caps a single frame's payload (both directions). A JSON control/turn
// message never approaches this; the cap is a defense against a peer (or a bug)
// asking us to allocate an unbounded buffer.
const maxFrame = 16 << 20 // 16 MB

// websocket opcodes (the low nibble of the first frame byte).
const (
	wsContinuation byte = 0x0
	wsTextFrame    byte = 0x1
	wsBinaryFrame  byte = 0x2
	wsCloseFrame   byte = 0x8
	wsPingFrame    byte = 0x9
	wsPongFrame    byte = 0xA
)

// wsConn is one framed websocket connection. It knows which side it is (isClient),
// because the client MUST mask its writes and the server MUST NOT. A read mutex and
// a write mutex let a turn's read loop run concurrently with an interrupt write
// (the same discipline claude.go uses for the live subprocess).
type wsConn struct {
	conn     net.Conn
	br       *bufio.Reader
	isClient bool
	readMu   sync.Mutex
	writeMu  sync.Mutex
}

// wsAcceptKey computes Sec-WebSocket-Accept = base64(sha1(key + magic)).
func wsAcceptKey(key string) string {
	h := sha1.Sum([]byte(key + wsMagic))
	return base64.StdEncoding.EncodeToString(h[:])
}

// connectionUpgrade reports whether the Connection header carries the "upgrade"
// token (it may be a comma list, e.g. "keep-alive, Upgrade").
func connectionUpgrade(h http.Header) bool {
	for _, v := range h.Values("Connection") {
		for _, tok := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(tok), "upgrade") {
				return true
			}
		}
	}
	return false
}

// wsUpgrade performs the server-side RFC 6455 handshake and hijacks the connection.
// The returned wsConn reads from the hijacked bufio.Reader (which may already hold
// bytes the client pipelined after the request) and writes raw frames to the conn.
func wsUpgrade(w http.ResponseWriter, r *http.Request) (*wsConn, error) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return nil, fmt.Errorf("ws: not a websocket upgrade request")
	}
	if !connectionUpgrade(r.Header) {
		return nil, fmt.Errorf("ws: missing Connection: Upgrade")
	}
	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" {
		return nil, fmt.Errorf("ws: missing Sec-WebSocket-Key")
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		return nil, fmt.Errorf("ws: response writer does not support hijack")
	}
	conn, brw, err := hj.Hijack()
	if err != nil {
		return nil, err
	}
	resp := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + wsAcceptKey(key) + "\r\n\r\n"
	if _, err := brw.WriteString(resp); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if err := brw.Flush(); err != nil {
		_ = conn.Close()
		return nil, err
	}
	// brw.Reader holds any bytes already read past the request line — reuse it, do
	// not wrap conn in a fresh reader (that would drop a pipelined first frame).
	return &wsConn{conn: conn, br: brw.Reader, isClient: false}, nil
}

// wsDial performs the client-side handshake: dial TCP (or TLS for wss://), send the
// upgrade request with a random Sec-WebSocket-Key, verify the server's accept. header
// carries any extra request headers (unused today; the auth token rides in the hello
// frame). tlsConfig is required for a wss:// URL and ignored for ws:// — it carries the
// pinned-mTLS identity + peer pin (see TLSClientConfig); the websocket then rides the
// encrypted conn transparently, since everything below reads from a net.Conn.
func wsDial(rawURL string, header http.Header, tlsConfig *tls.Config) (*wsConn, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	useTLS := false
	defaultPort := "80"
	switch u.Scheme {
	case "ws":
	case "wss":
		if tlsConfig == nil {
			return nil, fmt.Errorf("ws: wss:// requires pinned-mTLS — name the peer with --peer and pair first (loom pair)")
		}
		useTLS = true
		defaultPort = "443"
	default:
		return nil, fmt.Errorf("ws: unsupported scheme %q (want ws:// or wss://)", u.Scheme)
	}
	addr := u.Host
	if u.Port() == "" {
		addr = net.JoinHostPort(u.Hostname(), defaultPort)
	}
	var conn net.Conn
	if useTLS {
		conn, err = tls.Dial("tcp", addr, tlsConfig)
	} else {
		conn, err = net.Dial("tcp", addr)
	}
	if err != nil {
		return nil, err
	}
	var keyBytes [16]byte
	if _, err := rand.Read(keyBytes[:]); err != nil {
		_ = conn.Close()
		return nil, err
	}
	key := base64.StdEncoding.EncodeToString(keyBytes[:])
	path := u.RequestURI() // path + query, "/" when empty
	var req strings.Builder
	fmt.Fprintf(&req, "GET %s HTTP/1.1\r\n", path)
	fmt.Fprintf(&req, "Host: %s\r\n", u.Host)
	req.WriteString("Upgrade: websocket\r\n")
	req.WriteString("Connection: Upgrade\r\n")
	fmt.Fprintf(&req, "Sec-WebSocket-Key: %s\r\n", key)
	req.WriteString("Sec-WebSocket-Version: 13\r\n")
	for k, vs := range header {
		for _, v := range vs {
			fmt.Fprintf(&req, "%s: %s\r\n", k, v)
		}
	}
	req.WriteString("\r\n")
	if _, err := conn.Write([]byte(req.String())); err != nil {
		_ = conn.Close()
		return nil, err
	}
	// Reuse this reader for wsConn: ReadResponse leaves any post-101 frame bytes
	// buffered in it (a 101 has no body to consume).
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodGet})
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		_ = conn.Close()
		return nil, fmt.Errorf("ws: handshake failed: %s", resp.Status)
	}
	if got := resp.Header.Get("Sec-WebSocket-Accept"); got != wsAcceptKey(key) {
		_ = conn.Close()
		return nil, fmt.Errorf("ws: bad Sec-WebSocket-Accept (handshake not honored)")
	}
	return &wsConn{conn: conn, br: br, isClient: true}, nil
}

// ReadJSON reads one text frame and unmarshals it. It transparently answers pings
// and skips pongs. It returns io.EOF on a close frame or a closed socket.
func (c *wsConn) ReadJSON(v any) error {
	c.readMu.Lock()
	defer c.readMu.Unlock()
	payload, err := c.readFrame()
	if err != nil {
		return err
	}
	return json.Unmarshal(payload, v)
}

// readFrame returns the next TEXT frame's payload, looping past ping/pong control
// frames. Fragmentation and binary frames are rejected with a clear error.
func (c *wsConn) readFrame() ([]byte, error) {
	for {
		var h [2]byte
		if _, err := io.ReadFull(c.br, h[:]); err != nil {
			return nil, err
		}
		fin := h[0]&0x80 != 0
		opcode := h[0] & 0x0f
		masked := h[1]&0x80 != 0
		length := uint64(h[1] & 0x7f)
		switch length {
		case 126:
			var ext [2]byte
			if _, err := io.ReadFull(c.br, ext[:]); err != nil {
				return nil, err
			}
			length = uint64(binary.BigEndian.Uint16(ext[:]))
		case 127:
			var ext [8]byte
			if _, err := io.ReadFull(c.br, ext[:]); err != nil {
				return nil, err
			}
			length = binary.BigEndian.Uint64(ext[:])
		}
		if length > maxFrame {
			return nil, fmt.Errorf("ws: frame too large (%d bytes > %d cap)", length, maxFrame)
		}
		var mask [4]byte
		if masked {
			if _, err := io.ReadFull(c.br, mask[:]); err != nil {
				return nil, err
			}
		}
		payload := make([]byte, length)
		if _, err := io.ReadFull(c.br, payload); err != nil {
			return nil, err
		}
		if masked {
			for i := range payload {
				payload[i] ^= mask[i%4]
			}
		}
		switch opcode {
		case wsTextFrame:
			if !fin {
				return nil, fmt.Errorf("ws: fragmented frames not supported (send one JSON message per frame)")
			}
			return payload, nil
		case wsCloseFrame:
			return nil, io.EOF
		case wsPingFrame:
			_ = c.writeFrame(wsPongFrame, payload)
			continue
		case wsPongFrame:
			continue
		case wsContinuation:
			return nil, fmt.Errorf("ws: continuation frames not supported")
		case wsBinaryFrame:
			return nil, fmt.Errorf("ws: binary frames not supported (text/JSON only)")
		default:
			return nil, fmt.Errorf("ws: unknown opcode %#x", opcode)
		}
	}
}

// WriteJSON marshals v and writes it as one text frame.
func (c *wsConn) WriteJSON(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return c.writeFrame(wsTextFrame, b)
}

// writeFrame writes one frame (FIN set). A client masks the payload with a fresh
// random key; a server writes it plain. The whole frame goes out in a single
// Write under writeMu so concurrent writers (a turn's reply vs. an interrupt) never
// interleave bytes.
func (c *wsConn) writeFrame(opcode byte, payload []byte) error {
	n := len(payload)
	if n > maxFrame {
		return fmt.Errorf("ws: outgoing frame too large (%d bytes > %d cap)", n, maxFrame)
	}
	b0 := 0x80 | opcode // FIN + opcode

	var hdr []byte
	switch {
	case n < 126:
		hdr = []byte{b0, byte(n)}
	case n < 1<<16:
		hdr = []byte{b0, 126, byte(n >> 8), byte(n)}
	default:
		hdr = make([]byte, 10)
		hdr[0] = b0
		hdr[1] = 127
		binary.BigEndian.PutUint64(hdr[2:], uint64(n))
	}

	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	if c.isClient {
		hdr[1] |= 0x80 // MASK bit
		var key [4]byte
		if _, err := rand.Read(key[:]); err != nil {
			return err
		}
		buf := make([]byte, 0, len(hdr)+4+n)
		buf = append(buf, hdr...)
		buf = append(buf, key[:]...)
		for i := 0; i < n; i++ {
			buf = append(buf, payload[i]^key[i%4])
		}
		_, err := c.conn.Write(buf)
		return err
	}
	buf := make([]byte, 0, len(hdr)+n)
	buf = append(buf, hdr...)
	buf = append(buf, payload...)
	_, err := c.conn.Write(buf)
	return err
}

// Close sends a best-effort close frame and shuts the socket.
func (c *wsConn) Close() error {
	_ = c.writeFrame(wsCloseFrame, nil)
	return c.conn.Close()
}
