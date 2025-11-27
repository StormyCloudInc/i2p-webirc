package irc

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"
)

// MockIRCServer simulates an IRC server for testing
type MockIRCServer struct {
	mu          sync.RWMutex
	connections map[string]*MockIRCConn
	channels    map[string]map[string]bool // channel -> set of nicks
	msgDelay    time.Duration              // simulate network latency
	serverName  string
}

// NewMockIRCServer creates a new mock IRC server
func NewMockIRCServer(msgDelay time.Duration) *MockIRCServer {
	return &MockIRCServer{
		connections: make(map[string]*MockIRCConn),
		channels:    make(map[string]map[string]bool),
		msgDelay:    msgDelay,
		serverName:  "mock.irc.local",
	}
}

// NewConnection creates a new mock connection to this server
func (s *MockIRCServer) NewConnection(sessionID string) (*MockIRCConn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	conn := &MockIRCConn{
		server:    s,
		sessionID: sessionID,
		readBuf:   new(bytes.Buffer),
		writeBuf:  new(bytes.Buffer),
		readCond:  sync.NewCond(&sync.Mutex{}),
	}
	s.connections[sessionID] = conn
	return conn, nil
}

// RemoveConnection removes a connection from the server
func (s *MockIRCServer) RemoveConnection(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.connections, sessionID)
}

// GetChannelUsers returns users in a channel
func (s *MockIRCServer) GetChannelUsers(channel string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	channel = strings.ToLower(channel)
	users := s.channels[channel]
	if users == nil {
		return nil
	}

	result := make([]string, 0, len(users))
	for nick := range users {
		result = append(result, nick)
	}
	return result
}

// JoinChannel adds a user to a channel
func (s *MockIRCServer) JoinChannel(channel, nick string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	channel = strings.ToLower(channel)
	if s.channels[channel] == nil {
		s.channels[channel] = make(map[string]bool)
	}
	s.channels[channel][nick] = true
}

// PartChannel removes a user from a channel
func (s *MockIRCServer) PartChannel(channel, nick string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	channel = strings.ToLower(channel)
	if s.channels[channel] != nil {
		delete(s.channels[channel], nick)
	}
}

// BroadcastToChannel sends a message to all users in a channel
func (s *MockIRCServer) BroadcastToChannel(channel, from, msgType, text string) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	channel = strings.ToLower(channel)
	users := s.channels[channel]
	if users == nil {
		return
	}

	for nick := range users {
		for sessionID, conn := range s.connections {
			if conn.nick == nick && nick != from {
				msg := fmt.Sprintf(":%s!user@mock.i2p %s %s :%s\r\n", from, msgType, channel, text)
				conn.queueResponse(msg)
				_ = sessionID // silence unused warning
			}
		}
	}
}

// Stats returns server statistics
func (s *MockIRCServer) Stats() (connections, channels int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.connections), len(s.channels)
}

// MockIRCConn simulates an IRC connection
type MockIRCConn struct {
	server    *MockIRCServer
	sessionID string
	nick      string

	mu       sync.Mutex
	readBuf  *bytes.Buffer
	writeBuf *bytes.Buffer
	readCond *sync.Cond
	closed   bool

	registered bool
}

// queueResponse adds a response to the read buffer
func (c *MockIRCConn) queueResponse(msg string) {
	c.readCond.L.Lock()
	defer c.readCond.L.Unlock()

	if c.server.msgDelay > 0 {
		time.Sleep(c.server.msgDelay)
	}

	c.readBuf.WriteString(msg)
	c.readCond.Signal()
}

// Read implements net.Conn
func (c *MockIRCConn) Read(b []byte) (int, error) {
	c.readCond.L.Lock()
	defer c.readCond.L.Unlock()

	// Wait for data or closed
	for c.readBuf.Len() == 0 && !c.closed {
		c.readCond.Wait()
	}

	if c.closed && c.readBuf.Len() == 0 {
		return 0, io.EOF
	}

	return c.readBuf.Read(b)
}

// Write implements net.Conn - parses IRC commands and generates responses
func (c *MockIRCConn) Write(b []byte) (int, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return 0, io.ErrClosedPipe
	}
	c.mu.Unlock()

	lines := strings.Split(string(b), "\r\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		c.processCommand(line)
	}
	return len(b), nil
}

// processCommand handles an IRC command and generates appropriate responses
func (c *MockIRCConn) processCommand(line string) {
	parts := strings.SplitN(line, " ", 2)
	if len(parts) == 0 {
		return
	}

	cmd := strings.ToUpper(parts[0])
	var params string
	if len(parts) > 1 {
		params = parts[1]
	}

	switch cmd {
	case "NICK":
		c.nick = strings.TrimSpace(params)

	case "USER":
		// Registration complete after NICK and USER
		if c.nick != "" && !c.registered {
			c.registered = true
			// Send welcome messages
			c.queueResponse(fmt.Sprintf(":%s 001 %s :Welcome to the Mock IRC Network %s\r\n",
				c.server.serverName, c.nick, c.nick))
			c.queueResponse(fmt.Sprintf(":%s 002 %s :Your host is %s\r\n",
				c.server.serverName, c.nick, c.server.serverName))
			c.queueResponse(fmt.Sprintf(":%s 003 %s :This server was created for testing\r\n",
				c.server.serverName, c.nick))
			c.queueResponse(fmt.Sprintf(":%s 004 %s %s mock-1.0 o o\r\n",
				c.server.serverName, c.nick, c.server.serverName))
		}

	case "JOIN":
		channel := strings.TrimSpace(params)
		if channel == "" {
			return
		}
		// Handle multiple channels
		for _, ch := range strings.Split(channel, ",") {
			ch = strings.TrimSpace(ch)
			if ch == "" {
				continue
			}
			c.server.JoinChannel(ch, c.nick)

			// Send join confirmation
			c.queueResponse(fmt.Sprintf(":%s!user@mock.i2p JOIN %s\r\n", c.nick, ch))

			// Send topic (none set)
			c.queueResponse(fmt.Sprintf(":%s 331 %s %s :No topic is set\r\n",
				c.server.serverName, c.nick, ch))

			// Send names list
			users := c.server.GetChannelUsers(ch)
			names := strings.Join(users, " ")
			c.queueResponse(fmt.Sprintf(":%s 353 %s = %s :%s\r\n",
				c.server.serverName, c.nick, ch, names))
			c.queueResponse(fmt.Sprintf(":%s 366 %s %s :End of /NAMES list\r\n",
				c.server.serverName, c.nick, ch))
		}

	case "PART":
		paramParts := strings.SplitN(params, " ", 2)
		channel := strings.TrimSpace(paramParts[0])
		c.server.PartChannel(channel, c.nick)
		c.queueResponse(fmt.Sprintf(":%s!user@mock.i2p PART %s\r\n", c.nick, channel))

	case "PRIVMSG":
		paramParts := strings.SplitN(params, " :", 2)
		if len(paramParts) != 2 {
			return
		}
		target := strings.TrimSpace(paramParts[0])
		text := paramParts[1]

		// If target is a channel, broadcast to other users
		if strings.HasPrefix(target, "#") {
			c.server.BroadcastToChannel(target, c.nick, "PRIVMSG", text)
		}

	case "PING":
		c.queueResponse(fmt.Sprintf("PONG %s\r\n", params))

	case "QUIT":
		c.Close()

	case "MODE":
		// Ignore mode requests for now

	case "WHO":
		// Send empty WHO response
		channel := strings.TrimSpace(params)
		c.queueResponse(fmt.Sprintf(":%s 315 %s %s :End of /WHO list\r\n",
			c.server.serverName, c.nick, channel))

	case "LIST":
		// List channels
		c.server.mu.RLock()
		for ch, users := range c.server.channels {
			c.queueResponse(fmt.Sprintf(":%s 322 %s %s %d :\r\n",
				c.server.serverName, c.nick, ch, len(users)))
		}
		c.server.mu.RUnlock()
		c.queueResponse(fmt.Sprintf(":%s 323 %s :End of /LIST\r\n",
			c.server.serverName, c.nick))
	}
}

// Close implements net.Conn
func (c *MockIRCConn) Close() error {
	c.readCond.L.Lock()
	c.closed = true
	c.readCond.Signal()
	c.readCond.L.Unlock()

	c.server.RemoveConnection(c.sessionID)
	return nil
}

// LocalAddr implements net.Conn
func (c *MockIRCConn) LocalAddr() net.Addr {
	return &mockAddr{addr: "local:" + c.sessionID}
}

// RemoteAddr implements net.Conn
func (c *MockIRCConn) RemoteAddr() net.Addr {
	return &mockAddr{addr: "mock.irc.local:6667"}
}

// SetDeadline implements net.Conn
func (c *MockIRCConn) SetDeadline(t time.Time) error {
	return nil
}

// SetReadDeadline implements net.Conn
func (c *MockIRCConn) SetReadDeadline(t time.Time) error {
	return nil
}

// SetWriteDeadline implements net.Conn
func (c *MockIRCConn) SetWriteDeadline(t time.Time) error {
	return nil
}

// mockAddr implements net.Addr
type mockAddr struct {
	addr string
}

func (a *mockAddr) Network() string { return "mock" }
func (a *mockAddr) String() string  { return a.addr }

// MockIRCDialer implements IRCDialer using a mock server
type MockIRCDialer struct {
	server    *MockIRCServer
	sessionID string
	conn      *MockIRCConn
}

// NewMockIRCDialer creates a new mock IRC dialer
func NewMockIRCDialer(server *MockIRCServer, sessionID string) *MockIRCDialer {
	return &MockIRCDialer{
		server:    server,
		sessionID: sessionID,
	}
}

// Dial implements IRCDialer
func (d *MockIRCDialer) Dial() (net.Conn, error) {
	conn, err := d.server.NewConnection(d.sessionID)
	if err != nil {
		return nil, err
	}
	d.conn = conn
	return conn, nil
}

// Close implements IRCDialer
func (d *MockIRCDialer) Close() error {
	if d.conn != nil {
		return d.conn.Close()
	}
	return nil
}
