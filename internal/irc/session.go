package irc

import (
	"bufio"
	"fmt"
	"log"
	"net"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	MaxMessages         = 200
	ReconnectMaxRetries = 10
	ReconnectBaseDelay  = 2 * time.Second
	ReconnectMaxDelay   = 2 * time.Minute
)

// ChatMessage represents a single chat message or event
type ChatMessage struct {
	Time   time.Time
	Prefix string // nickname or server
	Text   string
	Kind   string // "privmsg", "notice", "join", "part", "quit", "system", "action"
}

// ChannelState holds state for a single IRC channel
type ChannelState struct {
	mu            sync.RWMutex
	messages      []ChatMessage
	users         map[string]bool
	historyLoaded bool
}

// AddMessage adds a message to the channel's ring buffer
func (c *ChannelState) AddMessage(msg ChatMessage) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.messages = append(c.messages, msg)
	if len(c.messages) > MaxMessages {
		c.messages = c.messages[len(c.messages)-MaxMessages:]
	}
}

// GetMessages returns a copy of all messages
func (c *ChannelState) GetMessages() []ChatMessage {
	c.mu.RLock()
	defer c.mu.RUnlock()

	result := make([]ChatMessage, len(c.messages))
	copy(result, c.messages)
	return result
}

// AddUser adds a user to the channel
func (c *ChannelState) AddUser(nick string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.users == nil {
		c.users = make(map[string]bool)
	}
	c.users[nick] = true
}

// RemoveUser removes a user from the channel
func (c *ChannelState) RemoveUser(nick string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	delete(c.users, nick)
}

// GetUsers returns a sorted list of users
func (c *ChannelState) GetUsers() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()

	users := make([]string, 0, len(c.users))
	for nick := range c.users {
		users = append(users, nick)
	}
	return users
}

// RenameUser renames a user in the channel
func (c *ChannelState) RenameUser(oldNick, newNick string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.users[oldNick] {
		delete(c.users, oldNick)
		c.users[newNick] = true
	}
}

// IsHistoryLoaded returns whether history has been loaded for this channel
func (c *ChannelState) IsHistoryLoaded() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.historyLoaded
}

// SetHistoryLoaded marks history as loaded for this channel
func (c *ChannelState) SetHistoryLoaded() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.historyLoaded = true
}

// IRCSession represents a single user's IRC session
type IRCSession struct {
	ID       string
	dialer   IRCDialer
	conn     net.Conn
	nick     string
	username string
	realname string

	mu           sync.RWMutex
	channels     map[string]*ChannelState
	lastSeenTime map[string]time.Time // tracks when each channel was last viewed
	status       string               // "connected", "reconnecting", "failed", "disconnected"
	registered   bool                 // true when IRC registration complete (001 received)
	lastHTTP     time.Time
	currentChan  string

	outgoing chan string
	done     chan struct{}
}

// NewIRCSession creates a new IRC session
func NewIRCSession(id string, dialer IRCDialer, nick, username, realname string) *IRCSession {
	return &IRCSession{
		ID:           id,
		dialer:       dialer,
		nick:         nick,
		username:     username,
		realname:     realname,
		channels:     make(map[string]*ChannelState),
		lastSeenTime: make(map[string]time.Time),
		status:       "disconnected",
		lastHTTP:     time.Now(),
		outgoing:     make(chan string, 100),
		done:         make(chan struct{}),
	}
}

// Start initiates the IRC connection and starts read/write loops
func (s *IRCSession) Start() error {
	conn, err := s.dialer.Dial()
	if err != nil {
		s.setStatus("failed")
		return err
	}

	s.mu.Lock()
	s.conn = conn
	s.status = "connected"
	s.mu.Unlock()

	// Send initial IRC registration
	s.sendRaw(fmt.Sprintf("NICK %s", s.nick))
	s.sendRaw(fmt.Sprintf("USER %s 0 * :%s", s.username, s.realname))

	// Start goroutines
	go s.writeLoop()
	go s.readLoop()

	return nil
}

// GetOrCreateChannel gets or creates a channel state
func (s *IRCSession) GetOrCreateChannel(name string) *ChannelState {
	s.mu.Lock()
	defer s.mu.Unlock()

	name = strings.ToLower(name)
	if s.channels[name] == nil {
		s.channels[name] = &ChannelState{
			users: make(map[string]bool),
		}
	}
	return s.channels[name]
}

// GetChannel gets a channel state (read-only)
func (s *IRCSession) GetChannel(name string) *ChannelState {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.channels[strings.ToLower(name)]
}

// GetStatus returns the current connection status
func (s *IRCSession) GetStatus() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.status
}

func (s *IRCSession) setStatus(status string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status = status
}

// GetRegistered returns whether IRC registration is complete
func (s *IRCSession) GetRegistered() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.registered
}

func (s *IRCSession) setRegistered(registered bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.registered = registered
}

// GetNick returns the current nickname
func (s *IRCSession) GetNick() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.nick
}

// SetNick sets the nickname
func (s *IRCSession) SetNick(nick string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nick = nick
}

// UpdateLastHTTP updates the last HTTP activity timestamp
func (s *IRCSession) UpdateLastHTTP() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastHTTP = time.Now()
}

// GetLastHTTP returns the last HTTP activity timestamp
func (s *IRCSession) GetLastHTTP() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lastHTTP
}

// SetCurrentChannel sets the current channel
func (s *IRCSession) SetCurrentChannel(channel string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.currentChan = channel
}

// GetCurrentChannel gets the current channel
func (s *IRCSession) GetCurrentChannel() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.currentChan
}

// MarkChannelSeen marks a channel as seen at the current time
func (s *IRCSession) MarkChannelSeen(channel string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastSeenTime[strings.ToLower(channel)] = time.Now()
}

// GetUnreadCount returns the number of unread messages in a channel
func (s *IRCSession) GetUnreadCount(channel string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()

	ch := s.channels[strings.ToLower(channel)]
	if ch == nil {
		return 0
	}

	lastSeen, exists := s.lastSeenTime[strings.ToLower(channel)]
	if !exists {
		// Never seen this channel, all messages are unread
		return len(ch.GetMessages())
	}

	// Count messages after last seen time
	unread := 0
	for _, msg := range ch.GetMessages() {
		if msg.Time.After(lastSeen) {
			unread++
		}
	}
	return unread
}

// ChannelInfo holds information about a channel for display
type ChannelInfo struct {
	Name        string
	UnreadCount int
	IsDM        bool // true if this is a direct message, not a channel
}

// GetAllChannels returns all channels with metadata
func (s *IRCSession) GetAllChannels() []ChannelInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()

	channels := make([]ChannelInfo, 0, len(s.channels))
	for name := range s.channels {
		isDM := !strings.HasPrefix(name, "#")
		channels = append(channels, ChannelInfo{
			Name:        name,
			UnreadCount: s.GetUnreadCount(name),
			IsDM:        isDM,
		})
	}

	// Sort channels: regular channels first (alphabetically), then DMs (alphabetically)
	sort.Slice(channels, func(i, j int) bool {
		if channels[i].IsDM != channels[j].IsDM {
			return !channels[i].IsDM // channels before DMs
		}
		return channels[i].Name < channels[j].Name
	})

	return channels
}

// SendMessage queues a message to be sent to IRC
func (s *IRCSession) SendMessage(msg string) {
	select {
	case s.outgoing <- msg:
	case <-s.done:
	default:
		log.Printf("Session %s: outgoing queue full, dropping message", s.ID)
	}
}

// sendRaw sends a raw message immediately (used internally)
func (s *IRCSession) sendRaw(msg string) error {
	s.mu.RLock()
	conn := s.conn
	s.mu.RUnlock()

	if conn == nil {
		return fmt.Errorf("not connected")
	}

	_, err := fmt.Fprintf(conn, "%s\r\n", msg)
	return err
}

// writeLoop handles outgoing messages
func (s *IRCSession) writeLoop() {
	for {
		select {
		case msg := <-s.outgoing:
			if err := s.sendRaw(msg); err != nil {
				log.Printf("Session %s: write error: %v", s.ID, err)
				return
			}
		case <-s.done:
			return
		}
	}
}

// readLoop handles incoming messages and reconnection
func (s *IRCSession) readLoop() {
	scanner := bufio.NewScanner(s.conn)

	for {
		if !scanner.Scan() {
			// Connection lost
			if err := scanner.Err(); err != nil {
				log.Printf("Session %s: read error: %v", s.ID, err)
			} else {
				log.Printf("Session %s: connection closed", s.ID)
			}

			// Attempt reconnection
			s.reconnect()
			return
		}

		line := scanner.Text()
		s.handleIRCLine(line)
	}
}

// reconnect attempts to reconnect with exponential backoff
func (s *IRCSession) reconnect() {
	s.setStatus("reconnecting")
	s.setRegistered(false)

	// Close old connection
	s.mu.Lock()
	if s.conn != nil {
		s.conn.Close()
		s.conn = nil
	}
	s.mu.Unlock()

	delay := ReconnectBaseDelay

	for retry := 0; retry < ReconnectMaxRetries; retry++ {
		log.Printf("Session %s: reconnecting (attempt %d/%d)...", s.ID, retry+1, ReconnectMaxRetries)

		// Wait before retry
		time.Sleep(delay)

		// Try to dial
		conn, err := s.dialer.Dial()
		if err != nil {
			log.Printf("Session %s: reconnect failed: %v", s.ID, err)
			delay *= 2
			if delay > ReconnectMaxDelay {
				delay = ReconnectMaxDelay
			}
			continue
		}

		// Success!
		s.mu.Lock()
		s.conn = conn
		s.status = "connected"
		s.mu.Unlock()

		// Re-register
		s.sendRaw(fmt.Sprintf("NICK %s", s.nick))
		s.sendRaw(fmt.Sprintf("USER %s 0 * :%s", s.username, s.realname))

		// Rejoin all channels
		s.mu.RLock()
		channelNames := make([]string, 0, len(s.channels))
		for name := range s.channels {
			channelNames = append(channelNames, name)
		}
		s.mu.RUnlock()

		for _, name := range channelNames {
			s.sendRaw(fmt.Sprintf("JOIN %s", name))
			ch := s.GetOrCreateChannel(name)
			ch.AddMessage(ChatMessage{
				Time:   time.Now(),
				Prefix: "system",
				Text:   fmt.Sprintf("Reconnected at %s", time.Now().Format("15:04:05")),
				Kind:   "system",
			})
		}

		log.Printf("Session %s: reconnected successfully", s.ID)

		// Restart read loop
		go s.readLoop()
		return
	}

	// Failed to reconnect
	log.Printf("Session %s: failed to reconnect after %d attempts", s.ID, ReconnectMaxRetries)
	s.setStatus("failed")
}

// Close shuts down the session
func (s *IRCSession) Close() {
	close(s.done)

	s.mu.Lock()
	if s.conn != nil {
		s.sendRaw("QUIT :Leaving")
		s.conn.Close()
		s.conn = nil
	}
	s.mu.Unlock()

	if s.dialer != nil {
		s.dialer.Close()
	}
}

// SessionStore manages all active IRC sessions
type SessionStore struct {
	mu   sync.RWMutex
	data map[string]*IRCSession
}

// NewSessionStore creates a new session store
func NewSessionStore() *SessionStore {
	return &SessionStore{
		data: make(map[string]*IRCSession),
	}
}

// Get retrieves a session by ID
func (ss *SessionStore) Get(id string) *IRCSession {
	ss.mu.RLock()
	defer ss.mu.RUnlock()
	return ss.data[id]
}

// Set stores a session
func (ss *SessionStore) Set(id string, session *IRCSession) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	ss.data[id] = session
}

// Delete removes a session
func (ss *SessionStore) Delete(id string) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	delete(ss.data, id)
}

// GetAll returns all sessions (for cleanup)
func (ss *SessionStore) GetAll() []*IRCSession {
	ss.mu.RLock()
	defer ss.mu.RUnlock()

	sessions := make([]*IRCSession, 0, len(ss.data))
	for _, s := range ss.data {
		sessions = append(sessions, s)
	}
	return sessions
}

// Count returns the number of active sessions
func (ss *SessionStore) Count() int {
	ss.mu.RLock()
	defer ss.mu.RUnlock()
	return len(ss.data)
}
