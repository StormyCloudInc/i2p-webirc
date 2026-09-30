package irc

import (
	"bufio"
	"fmt"
	sam3 "github.com/go-i2p/go-sam-go"
	"github.com/go-i2p/go-sam-go/common"
	"github.com/go-i2p/go-sam-go/stream"
	"net"
	"strconv"
	"strings"
	"time"
)

// NewBoundedSAM bounds the initial TCP dial and HELLO exchange as well as later
// control operations. The upstream constructor does not set either deadline.
func NewBoundedSAM(address string, timeout time.Duration) (*sam3.SAM, error) {
	conn, err := net.DialTimeout("tcp", address, timeout)
	if err != nil {
		return nil, err
	}
	conn.SetDeadline(time.Now().Add(timeout))
	base := &common.SAM{Conn: conn}
	base.SAMEmit.I2PConfig.SetSAMAddress(address)
	if _, err = conn.Write(base.HelloBytes()); err != nil {
		conn.Close()
		return nil, err
	}
	reply, err := samReply(bufio.NewReader(conn))
	if err != nil || !samResultOK(reply, "HELLO REPLY") {
		conn.Close()
		return nil, fmt.Errorf("SAM HELLO failed")
	}
	resolver, err := common.NewSAMResolver(base)
	if err != nil {
		conn.Close()
		return nil, err
	}
	base.SAMResolver = *resolver
	return &sam3.SAM{SAM: base}, nil
}

// DialSAMStream uses a bounded second SAM connection. Upstream DialContext's
// timeout starts after an unbounded HELLO and destination lookup. Keeping this
// small protocol boundary here prevents abandoned sessions holding sockets open.
func DialSAMStream(client *sam3.SAM, session *stream.StreamSession, destination string, timeout time.Duration) (net.Conn, error) {
	host := destination
	port := ""
	if strings.Contains(destination, ":") {
		var err error
		host, port, err = net.SplitHostPort(destination)
		if err != nil {
			return nil, fmt.Errorf("invalid IRC destination port")
		}
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return nil, fmt.Errorf("invalid IRC destination port")
		}
	}
	client.Conn.SetDeadline(time.Now().Add(timeout))
	addr, err := client.Lookup(host)
	if err != nil {
		return nil, fmt.Errorf("SAM destination lookup failed: %w", err)
	}
	conn, err := connectSAMStream(client.Sam(), session.ID(), addr.String(), port, timeout)
	// Session control socket stays open to maintain the I2P destination.
	client.Conn.SetDeadline(time.Time{})
	return conn, err
}
func connectSAMStream(address, id, destination, port string, timeout time.Duration) (net.Conn, error) {
	client, err := NewBoundedSAM(address, timeout)
	if err != nil {
		return nil, err
	}
	conn := client.Conn
	reader := bufio.NewReader(conn)
	command := fmt.Sprintf("STREAM CONNECT ID=%s DESTINATION=%s SILENT=false", id, destination)
	if port != "" {
		command += " TO_PORT=" + port
	}
	if _, err = fmt.Fprintf(conn, "%s\n", command); err != nil {
		conn.Close()
		return nil, err
	}
	reply, err := samReply(reader)
	if err != nil || !samResultOK(reply, "STREAM STATUS") {
		conn.Close()
		return nil, fmt.Errorf("SAM stream connection failed")
	}
	conn.SetDeadline(time.Time{})
	return &bufferedSAMConn{Conn: conn, reader: reader}, nil
}

type bufferedSAMConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedSAMConn) Read(p []byte) (int, error) { return c.reader.Read(p) }
func samReply(reader *bufio.Reader) (string, error) {
	var line []byte
	for len(line) < 4096 {
		b, err := reader.ReadByte()
		if err != nil {
			return "", err
		}
		if b == '\n' {
			return strings.TrimSpace(string(line)), nil
		}
		line = append(line, b)
	}
	return "", fmt.Errorf("SAM reply too long")
}
func samResultOK(reply, prefix string) bool {
	if !strings.HasPrefix(reply, prefix+" ") {
		return false
	}
	for _, field := range strings.Fields(reply) {
		if strings.HasPrefix(field, "RESULT=") {
			return field == "RESULT=OK"
		}
	}
	return false
}
