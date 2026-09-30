package bot

import (
	"bufio"
	"container/ring"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"github.com/dustinfields/i2p-irc/internal/irc"
	"log"
	"net"
	"strings"
	"sync"
	"time"

	sam3 "github.com/go-i2p/go-sam-go"
	"github.com/go-i2p/go-sam-go/stream"
)

// BotConfig holds configuration for a history bot
type BotConfig struct {
	Nick         string   // Bot nickname (e.g., "StormyBot")
	LocalAddr    string   // Local TCP address (e.g., "127.0.0.1:6668")
	Channels     []string // Channels to join
	HistorySize  int      // Number of messages to keep per channel
	NickServPass string   // NickServ password (optional, for registered nicks)
	UseInvisible bool     // Set +i mode (invisible)
	UseBot       bool     // Set +b mode (bot)
	ServerName   string   // Server name for logging (e.g., "postman", "simp")
}

// Message represents a single IRC message with metadata
type Message struct {
	Timestamp time.Time
	Nick      string
	Content   string
	Type      string // "msg", "join", "part", "action"
}

// ChannelHistory stores recent messages for a channel
type ChannelHistory struct {
	mu       sync.RWMutex
	messages *ring.Ring
	size     int
}

// NewChannelHistory creates a new channel history buffer
func NewChannelHistory(size int) *ChannelHistory {
	return &ChannelHistory{
		messages: ring.New(max(size, 1)),
		size:     max(size, 1),
	}
}

// Add adds a message to the history
func (ch *ChannelHistory) Add(msg Message) {
	ch.mu.Lock()
	defer ch.mu.Unlock()
	ch.messages.Value = msg
	ch.messages = ch.messages.Next()
}

// GetRecent returns the last n messages (or all if fewer than n)
func (ch *ChannelHistory) GetRecent(n int) []Message {
	ch.mu.RLock()
	defer ch.mu.RUnlock()

	if n <= 0 {
		return nil
	}
	if n > ch.size {
		n = ch.size
	}

	messages := make([]Message, 0, ch.size)

	// Walk backwards from current position
	ch.messages.Do(func(v interface{}) {
		if v != nil {
			messages = append(messages, v.(Message))
		}
	})

	// The ring cursor points at the oldest slot. Retain the newest n.
	if len(messages) > n {
		messages = messages[len(messages)-n:]
	}

	return messages
}

// HistoryBot maintains IRC channel history
type HistoryBot struct {
	connMu       sync.RWMutex
	writeMu      sync.Mutex
	stopOnce     sync.Once
	nick         string
	baseNick     string // original nick before collision handling
	nickSuffix   int    // suffix counter for nick collision handling
	sessionID    string // unique SAM session ID
	samAddr      string
	ircDest      string
	localAddr    string // local TCP address (e.g., "127.0.0.1:6668") - if set, uses TCP instead of SAM
	channels     []string
	historySize  int
	nickServPass string // NickServ password for identification
	useInvisible bool   // Set +i mode
	useBot       bool   // Set +b mode (marks as bot)
	serverName   string // Server name for logging

	histories   map[string]*ChannelHistory
	historiesMu sync.RWMutex

	conn         net.Conn
	sam          *sam3.SAM
	stream       *stream.StreamSession
	stopCh       chan struct{}
	stoppedCh    chan struct{}
	registered   bool
	registeredMu sync.Mutex
	identified   bool // Whether we've identified with NickServ
	identifiedMu sync.Mutex
}

// NewHistoryBot creates a new history bot using SAM for I2P connections
func NewHistoryBot(sessionID, nick, samAddr, ircDest string, channels []string, historySize int) *HistoryBot {
	if historySize == 0 {
		historySize = 50 // default to storing last 50 messages
	}

	return &HistoryBot{
		sessionID:   sessionID,
		nick:        nick,
		baseNick:    nick,
		nickSuffix:  0,
		samAddr:     samAddr,
		ircDest:     ircDest,
		channels:    channels,
		historySize: historySize,
		histories:   make(map[string]*ChannelHistory),
		stopCh:      make(chan struct{}),
		stoppedCh:   make(chan struct{}),
	}
}

// NewHistoryBotLocal creates a new history bot using local TCP (for I2P tunnels)
func NewHistoryBotLocal(nick, localAddr string, channels []string, historySize int) *HistoryBot {
	if historySize == 0 {
		historySize = 50 // default to storing last 50 messages
	}

	return &HistoryBot{
		nick:        nick,
		baseNick:    nick,
		nickSuffix:  0,
		localAddr:   localAddr,
		channels:    channels,
		historySize: historySize,
		histories:   make(map[string]*ChannelHistory),
		stopCh:      make(chan struct{}),
		stoppedCh:   make(chan struct{}),
	}
}

// NewHistoryBotWithConfig creates a new history bot with full configuration
func NewHistoryBotWithConfig(config BotConfig) *HistoryBot {
	if config.HistorySize == 0 {
		config.HistorySize = 50
	}
	if config.ServerName == "" {
		config.ServerName = "unknown"
	}

	return &HistoryBot{
		nick:         config.Nick,
		baseNick:     config.Nick,
		nickSuffix:   0,
		localAddr:    config.LocalAddr,
		channels:     config.Channels,
		historySize:  config.HistorySize,
		nickServPass: config.NickServPass,
		useInvisible: config.UseInvisible,
		useBot:       config.UseBot,
		serverName:   config.ServerName,
		histories:    make(map[string]*ChannelHistory),
		stopCh:       make(chan struct{}),
		stoppedCh:    make(chan struct{}),
	}
}

// randomSuffix generates a short random suffix for session names
func randomSuffix() string {
	b := make([]byte, 4)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// Start starts the history bot
func (hb *HistoryBot) Start() error {
	// Initialize channel histories
	hb.historiesMu.Lock()
	for _, channel := range hb.channels {
		if hb.histories[strings.ToLower(channel)] == nil {
			hb.histories[strings.ToLower(channel)] = NewChannelHistory(hb.historySize)
		}
	}
	hb.historiesMu.Unlock()

	var conn net.Conn
	var err error

	// Use local TCP if localAddr is set, otherwise use SAM
	if hb.localAddr != "" {
		// Local TCP mode - connect directly to local I2P tunnel
		log.Printf("%s Connecting via local tunnel: %s", hb.logPrefix(), hb.localAddr)
		conn, err = net.DialTimeout("tcp", hb.localAddr, 30*time.Second)
		if err != nil {
			return fmt.Errorf("failed to connect to local IRC tunnel %s: %w", hb.localAddr, err)
		}
	} else {
		// SAM mode - connect via I2P SAM bridge
		samClient, err := irc.NewBoundedSAM(hb.samAddr, 90*time.Second)
		if err != nil {
			return fmt.Errorf("failed to connect to SAM: %w", err)
		}
		hb.sam = samClient

		// Create streaming session with random suffix to avoid conflicts
		sessionName := hb.sessionID + "-" + randomSuffix()
		keys, err := samClient.NewKeys()
		if err != nil {
			samClient.Close()
			return fmt.Errorf("failed to generate keys: %w", err)
		}

		// Use nil for default options in go-sam-go
		streamSession, err := samClient.NewStreamSession(sessionName, keys, nil)
		if err != nil {
			samClient.Close()
			return fmt.Errorf("failed to create stream session: %w", err)
		}
		hb.stream = streamSession

		// Parse destination and port (format: "host" or "host:port")
		dest := hb.ircDest
		port := ""
		if idx := strings.LastIndex(dest, ":"); idx != -1 {
			// Check if this looks like a port (digits after colon)
			possiblePort := dest[idx+1:]
			if _, err := fmt.Sscanf(possiblePort, "%d", new(int)); err == nil {
				port = possiblePort
				dest = dest[:idx]
			}
		}

		// Dial the destination - go-sam-go's Dial handles lookup internally
		if port != "" {
			conn, err = irc.DialSAMStream(samClient, streamSession, dest+":"+port, 90*time.Second)
		} else {
			conn, err = irc.DialSAMStream(samClient, streamSession, dest, 90*time.Second)
		}
		if err != nil {
			samClient.Close()
			return fmt.Errorf("failed to connect to IRC: %w", err)
		}
	}

	hb.setConn(conn)
	log.Printf("%s CONNECTED to IRC server as %s", hb.logPrefix(), hb.nick)

	// Send initial IRC commands
	hb.send("NICK %s\r\n", hb.nick)
	hb.send("USER webirc 0 * :%s\r\n", hb.nick)
	log.Printf("%s Sent NICK and USER, waiting for registration...", hb.logPrefix())

	// Start message reader (will join channels after receiving 001)
	go hb.readMessages()

	return nil
}

// Stop stops the history bot
func (hb *HistoryBot) Stop() {
	hb.stopOnce.Do(func() {
		hb.registeredMu.Lock()
		hb.registered = false
		hb.registeredMu.Unlock()
		close(hb.stopCh)
		if conn := hb.getConn(); conn != nil {
			conn.Close()
		}
	})
}

// readMessages reads and processes IRC messages
func (hb *HistoryBot) readMessages() {
	defer close(hb.stoppedCh)
	defer func() {
		if hb.stream != nil {
			hb.stream.Close()
		}
		if hb.sam != nil {
			hb.sam.Close()
		}
	}()
	defer func() { hb.registeredMu.Lock(); hb.registered = false; hb.registeredMu.Unlock() }()

	for {
		scanner := bufio.NewScanner(hb.getConn())
		for scanner.Scan() {
			select {
			case <-hb.stopCh:
				return
			default:
			}

			line := scanner.Text()
			hb.processMessage(line)
		}

		// Connection lost
		if err := scanner.Err(); err != nil {
			log.Printf("%s DISCONNECTED - Error reading: %v", hb.logPrefix(), err)
		} else {
			log.Printf("%s DISCONNECTED - Connection closed", hb.logPrefix())
		}

		// Check if we're stopping
		select {
		case <-hb.stopCh:
			return
		default:
		}

		// Attempt reconnection
		log.Printf("%s RECONNECTING...", hb.logPrefix())
		if !hb.reconnect() {
			log.Printf("%s FAILED - Could not reconnect, stopping bot", hb.logPrefix())
			return
		}
	}
}

// reconnect attempts to reconnect to the IRC server with exponential backoff
func (hb *HistoryBot) reconnect() bool {
	const maxRetries = 10
	const baseDelay = 5 * time.Second
	const maxDelay = 2 * time.Minute

	// Close old connections completely
	if hb.stream != nil {
		hb.stream.Close()
		hb.stream = nil
	}
	if conn := hb.getConn(); conn != nil {
		conn.Close()
		hb.setConn(nil)
	}
	if hb.sam != nil {
		hb.sam.Close()
		hb.sam = nil
	}

	hb.registeredMu.Lock()
	hb.registered = false
	hb.registeredMu.Unlock()

	// Reset nick to base nick for fresh attempt
	hb.nick = hb.baseNick
	hb.nickSuffix = 0

	delay := baseDelay

	for retry := 0; retry < maxRetries; retry++ {
		log.Printf("%s RECONNECT attempt %d/%d (delay: %v)...", hb.logPrefix(), retry+1, maxRetries, delay)

		// Wait before retry
		select {
		case <-time.After(delay):
		case <-hb.stopCh:
			return false
		}

		var conn net.Conn
		var err error

		// Use local TCP if localAddr is set, otherwise use SAM
		if hb.localAddr != "" {
			// Local TCP mode - simple reconnect
			conn, err = net.DialTimeout("tcp", hb.localAddr, 30*time.Second)
			if err != nil {
				log.Printf("%s Failed to connect to local tunnel: %v", hb.logPrefix(), err)
				delay *= 2
				if delay > maxDelay {
					delay = maxDelay
				}
				continue
			}
		} else {
			// SAM mode - create fresh SAM connection
			// Parse destination and port
			dest := hb.ircDest
			port := ""
			if idx := strings.LastIndex(dest, ":"); idx != -1 {
				possiblePort := dest[idx+1:]
				if _, err := fmt.Sscanf(possiblePort, "%d", new(int)); err == nil {
					port = possiblePort
					dest = dest[:idx]
				}
			}

			samClient, err := irc.NewBoundedSAM(hb.samAddr, 90*time.Second)
			if err != nil {
				log.Printf("[HistoryBot] Failed to connect to SAM: %v", err)
				delay *= 2
				if delay > maxDelay {
					delay = maxDelay
				}
				continue
			}

			// Create a new SAM stream session with new keys
			keys, err := samClient.NewKeys()
			if err != nil {
				log.Printf("[HistoryBot] Failed to generate keys: %v", err)
				samClient.Close()
				delay *= 2
				if delay > maxDelay {
					delay = maxDelay
				}
				continue
			}

			sessionName := hb.sessionID + "-reconnect-" + randomSuffix()
			// Use nil for default options in go-sam-go
			streamSession, err := samClient.NewStreamSession(sessionName, keys, nil)
			if err != nil {
				log.Printf("[HistoryBot] Failed to create stream session: %v", err)
				samClient.Close()
				delay *= 2
				if delay > maxDelay {
					delay = maxDelay
				}
				continue
			}

			// Dial the destination - go-sam-go's Dial handles lookup internally
			if port != "" {
				conn, err = irc.DialSAMStream(samClient, streamSession, dest+":"+port, 90*time.Second)
			} else {
				conn, err = irc.DialSAMStream(samClient, streamSession, dest, 90*time.Second)
			}
			if err != nil {
				log.Printf("[HistoryBot] Dial failed: %v", err)
				samClient.Close()
				delay *= 2
				if delay > maxDelay {
					delay = maxDelay
				}
				continue
			}

			hb.sam = samClient
			hb.stream = streamSession
		}

		// Success! Update connection state
		hb.setConn(conn)
		log.Printf("%s RECONNECTED successfully", hb.logPrefix())

		// Re-register with IRC
		hb.send("NICK %s\r\n", hb.nick)
		hb.send("USER %s 0 * :%s\r\n", hb.nick, hb.nick)

		// Note: channels will be rejoined when we receive 001 in processMessage
		return true
	}

	log.Printf("%s FAILED - Could not reconnect after %d attempts", hb.logPrefix(), maxRetries)
	return false
}

// logPrefix returns the logging prefix for this bot
func (hb *HistoryBot) logPrefix() string {
	if hb.serverName != "" {
		return fmt.Sprintf("[%s]", hb.serverName)
	}
	return "[HistoryBot]"
}

// processMessage processes a single IRC message
func (hb *HistoryBot) processMessage(line string) {
	// Handle PING
	if strings.HasPrefix(line, "PING ") {
		pong := strings.Replace(line, "PING", "PONG", 1)
		hb.send("%s\r\n", pong)
		return
	}

	// Check for ban notice (comes before registration)
	if strings.Contains(line, "You are banned") {
		log.Printf("%s WARNING: Bot is banned from server! Message: %s", hb.logPrefix(), line)
		return
	}

	// Parse IRC message
	if !strings.HasPrefix(line, ":") {
		return
	}

	parts := strings.SplitN(line[1:], " ", 3)
	if len(parts) < 3 {
		return
	}

	prefix := parts[0]
	command := parts[1]
	params := parts[2]

	// Extract nick from prefix
	nick := prefix
	if idx := strings.Index(prefix, "!"); idx != -1 {
		nick = prefix[:idx]
	}

	// Handle numeric responses
	switch command {
	case "001":
		// Registration successful - now set modes, identify, and join channels
		hb.registeredMu.Lock()
		if !hb.registered {
			hb.registered = true
			hb.registeredMu.Unlock()
			log.Printf("%s Registration complete as %s", hb.logPrefix(), hb.nick)

			// Set user modes (+B for bot - capital B on most IRC networks)
			if hb.useBot {
				hb.send("MODE %s +B\r\n", hb.nick)
				log.Printf("%s Setting mode +B", hb.logPrefix())
			}
			if hb.useInvisible {
				hb.send("MODE %s +i\r\n", hb.nick)
				log.Printf("%s Setting mode +i", hb.logPrefix())
			}

			// Identify with NickServ if password is set
			if hb.nickServPass != "" {
				hb.send("PRIVMSG NickServ :identify %s\r\n", hb.nickServPass)
				log.Printf("%s Identifying with NickServ...", hb.logPrefix())
			}

			for _, channel := range hb.channels {
				hb.send("JOIN %s\r\n", channel)
			}
		} else {
			hb.registeredMu.Unlock()
		}
		return

	case "433":
		// Nick is already in use - try alternate nick
		hb.nickSuffix++
		if hb.nickSuffix == 1 {
			// First collision: try _ALT suffix
			hb.nick = hb.baseNick + "_ALT"
		} else if hb.nickSuffix > 3 {
			// Give up after a few attempts
			log.Printf("%s Failed to find available nick after %d attempts", hb.logPrefix(), hb.nickSuffix)
			return
		} else {
			// Subsequent collisions: add number to _ALT
			hb.nick = fmt.Sprintf("%s_ALT%d", hb.baseNick, hb.nickSuffix)
		}
		log.Printf("%s Nick in use, trying alternate: %s", hb.logPrefix(), hb.nick)
		hb.send("NICK %s\r\n", hb.nick)
		return

	case "NOTICE":
		// Check for NickServ responses
		if strings.EqualFold(nick, "NickServ") {
			if strings.Contains(params, "You are now identified") || strings.Contains(params, "Password accepted") {
				hb.identifiedMu.Lock()
				hb.identified = true
				hb.identifiedMu.Unlock()
				log.Printf("%s Successfully identified with NickServ", hb.logPrefix())
			} else if strings.Contains(params, "Invalid password") || strings.Contains(params, "Access denied") {
				log.Printf("%s NickServ identification failed: %s", hb.logPrefix(), params)
			}
		}
		return
	}

	switch command {
	case "PRIVMSG":
		// Format: :nick!user@host PRIVMSG #channel :message
		paramParts := strings.SplitN(params, " :", 2)
		if len(paramParts) != 2 {
			return
		}
		channel := strings.TrimSpace(paramParts[0])
		content := paramParts[1]

		// Skip private messages (non-channel messages)
		if !strings.HasPrefix(channel, "#") {
			return
		}

		msgType := "msg"
		if strings.HasPrefix(content, "\x01ACTION ") && strings.HasSuffix(content, "\x01") {
			msgType = "action"
			content = strings.TrimSuffix(strings.TrimPrefix(content, "\x01ACTION "), "\x01")
		}

		hb.addMessage(channel, Message{
			Timestamp: time.Now(),
			Nick:      nick,
			Content:   content,
			Type:      msgType,
		})

	// JOIN and PART events - log our own joins for monitoring but don't store in history
	case "JOIN":
		// Log when we successfully join a channel
		if nick == hb.nick {
			channel := strings.TrimPrefix(params, ":")
			log.Printf("%s JOINED channel %s", hb.logPrefix(), channel)
		}
		return

	case "PART":
		// Log when we leave a channel
		if nick == hb.nick {
			log.Printf("%s PARTED channel %s", hb.logPrefix(), params)
		}
		return
	}
}

// addMessage adds a message to channel history
func (hb *HistoryBot) addMessage(channel string, msg Message) {
	channel = strings.ToLower(channel)

	hb.historiesMu.RLock()
	history, exists := hb.histories[channel]
	hb.historiesMu.RUnlock()

	if exists {
		history.Add(msg)
	}
}

// IsHealthy returns true if the bot is connected and registered with IRC
func (hb *HistoryBot) IsHealthy() bool {
	hb.registeredMu.Lock()
	registered := hb.registered
	hb.registeredMu.Unlock()

	return registered
}

// GetHistory returns recent messages for a channel
func (hb *HistoryBot) GetHistory(channel string, limit int) []Message {
	channel = strings.ToLower(channel)

	hb.historiesMu.RLock()
	history, exists := hb.histories[channel]
	hb.historiesMu.RUnlock()

	if !exists {
		return []Message{}
	}

	return history.GetRecent(limit)
}

func (hb *HistoryBot) getConn() net.Conn {
	hb.connMu.RLock()
	defer hb.connMu.RUnlock()
	return hb.conn
}
func (hb *HistoryBot) setConn(conn net.Conn) {
	hb.connMu.Lock()
	defer hb.connMu.Unlock()
	hb.conn = conn
	select {
	case <-hb.stopCh:
		if conn != nil {
			conn.Close()
		}
	default:
	}
}
func (hb *HistoryBot) send(format string, args ...interface{}) {
	hb.writeMu.Lock()
	defer hb.writeMu.Unlock()
	if conn := hb.getConn(); conn != nil {
		conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
		fmt.Fprintf(conn, format, args...)
		conn.SetWriteDeadline(time.Time{})
	}
}

// Run retries initial tunnel failures as well as subsequent disconnections.
func (hb *HistoryBot) Run() {
	delay := 5 * time.Second
	for {
		select {
		case <-hb.stopCh:
			return
		default:
		}
		if err := hb.Start(); err == nil {
			<-hb.stoppedCh
			return
		}
		log.Printf("%s History tunnel unavailable; retrying", hb.logPrefix())
		select {
		case <-hb.stopCh:
			return
		case <-time.After(delay):
		}
		delay = min(delay*2, 2*time.Minute)
	}
}
