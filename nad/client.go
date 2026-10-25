package nad

import (
	"bytes"
	"errors"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

func dial(host string, port int, timeout time.Duration) (net.Conn, error) {
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return nil, connErr("cannot connect to %s: %v", addr, err)
	}
	return conn, nil
}

// readFor reads whatever arrives within settle and appends it to buf. A read
// timeout ends the window normally; EOF ends it early.
func readFor(conn net.Conn, buf []byte, settle time.Duration) ([]byte, error) {
	if err := conn.SetReadDeadline(time.Now().Add(settle)); err != nil {
		return buf, connErr("recv failed: %v", err)
	}
	chunk := make([]byte, 4096)
	for {
		n, err := conn.Read(chunk)
		buf = append(buf, chunk[:n]...)
		if err == nil {
			continue
		}
		if errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, io.EOF) {
			return buf, nil
		}
		return buf, connErr("recv failed: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Text protocol client  (C338 / C368 / C388 / M32 / AVR)
// ---------------------------------------------------------------------------

// TextClient is a low-level client for the CR-terminated ASCII protocol (port 30001).
type TextClient struct {
	Host    string
	Port    int
	Timeout time.Duration

	conn net.Conn
	buf  []byte
	mu   sync.Mutex
}

func NewTextClient(host string, port int, timeout time.Duration) *TextClient {
	if port == 0 {
		port = TextPort
	}
	return &TextClient{Host: host, Port: port, Timeout: timeout}
}

func (c *TextClient) Connect() error {
	conn, err := dial(c.Host, c.Port, c.Timeout)
	if err != nil {
		return err
	}
	c.conn = conn
	return nil
}

func (c *TextClient) Close() error {
	if c.conn == nil {
		return nil
	}
	err := c.conn.Close()
	c.conn = nil
	return err
}

func (c *TextClient) send(line string) error {
	if c.conn == nil {
		return connErr("not connected")
	}
	if err := c.conn.SetWriteDeadline(time.Now().Add(c.Timeout)); err != nil {
		return connErr("send failed: %v", err)
	}
	if _, err := c.conn.Write([]byte(line + "\r")); err != nil {
		return connErr("send failed: %v", err)
	}
	return nil
}

// readLines reads whatever is available for settle and returns Key=Value pairs
// from every complete line.
func (c *TextClient) readLines(settle time.Duration, out map[string]string) error {
	if c.conn == nil {
		return connErr("not connected")
	}
	var err error
	if c.buf, err = readFor(c.conn, c.buf, settle); err != nil {
		return err
	}
	for {
		idx := bytes.IndexAny(c.buf, "\r\n")
		if idx < 0 {
			return nil
		}
		text := strings.TrimSpace(latin1(c.buf[:idx]))
		c.buf = c.buf[idx+1:]
		if k, v, ok := strings.Cut(text, "="); ok {
			out[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
}

// Query sends "<key>?" and collects the responses.
//
// Retries when the amp returns nothing — it briefly stops answering while its
// control interface reboots in the ~1-2s after a power-on command.
func (c *TextClient) Query(key string, settle time.Duration, retries int) (map[string]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[string]string{}
	for attempt := 0; attempt <= retries; attempt++ {
		if err := c.send(key + "?"); err != nil {
			return out, err
		}
		if err := c.readLines(settle, out); err != nil {
			return out, err
		}
		if len(out) > 0 {
			break
		}
		if attempt < retries {
			time.Sleep(600 * time.Millisecond)
		}
	}
	return out, nil
}

// Set sends "<key>=<value>" (or bare "<key>+" / "<key>-") and collects the echo.
func (c *TextClient) Set(key, value string, settle time.Duration) (map[string]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	line := key + "=" + value
	if value == "+" || value == "-" {
		line = key + value
	}
	if err := c.send(line); err != nil {
		return nil, err
	}
	out := map[string]string{}
	err := c.readLines(settle, out)
	return out, err
}

// ---------------------------------------------------------------------------
// Binary protocol client  (D 7050)
// ---------------------------------------------------------------------------

// Update is one pushed D7050 frame.
type Update struct {
	Msg   byte
	Value byte
}

// BinaryClient is a low-level client for the D7050 binary protocol (port 50001).
type BinaryClient struct {
	Host    string
	Port    int
	Timeout time.Duration

	conn net.Conn
	mu   sync.Mutex
}

func NewBinaryClient(host string, port int, timeout time.Duration) *BinaryClient {
	if port == 0 {
		port = BinaryPort
	}
	return &BinaryClient{Host: host, Port: port, Timeout: timeout}
}

func (c *BinaryClient) Connect() error {
	conn, err := dial(c.Host, c.Port, c.Timeout)
	if err != nil {
		return err
	}
	c.conn = conn
	return nil
}

func (c *BinaryClient) Close() error {
	if c.conn == nil {
		return nil
	}
	err := c.conn.Close()
	c.conn = nil
	return err
}

func (c *BinaryClient) write(msg, value byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return connErr("not connected")
	}
	if err := c.conn.SetWriteDeadline(time.Now().Add(c.Timeout)); err != nil {
		return connErr("send failed: %v", err)
	}
	if _, err := c.conn.Write([]byte{0x00, 0x01, 0x02, msg, value}); err != nil {
		return connErr("send failed: %v", err)
	}
	return nil
}

func (c *BinaryClient) Set(msg, value byte) error { return c.write(msg, value) }

// Query asks for one value: [00 01 02 02 <msg>].
func (c *BinaryClient) Query(msg byte) error { return c.write(msgQuery, msg) }

// ReadUpdates parses pushed [00 01 <n> <msg> <val>...] frames.
func (c *BinaryClient) ReadUpdates(settle time.Duration) ([]Update, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return nil, connErr("not connected")
	}
	data, err := readFor(c.conn, nil, settle)
	if err != nil {
		return nil, err
	}
	var out []Update
	for i := 0; i+5 <= len(data); {
		if data[i] == 0x00 && data[i+1] == 0x01 {
			n := int(data[i+2])
			out = append(out, Update{Msg: data[i+3], Value: data[i+4]})
			i += 3 + max(n, 2)
		} else {
			i++
		}
	}
	return out, nil
}

// latin1 decodes ISO-8859-1 bytes (every byte maps to the same code point).
func latin1(b []byte) string {
	r := make([]rune, len(b))
	for i, c := range b {
		r[i] = rune(c)
	}
	return string(r)
}
