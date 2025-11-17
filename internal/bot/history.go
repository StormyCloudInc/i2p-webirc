package bot

import (
	"bufio"
	"container/ring"
	"fmt"
	"log"
	"net"
	"strings"
	"sync"
	"time"

	sam3 "github.com/eyedeekay/sam3"
)

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
		messages: ring.New(size),
		size:     size,
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

	if n > ch.size {
		n = ch.size
	}

	messages := make([]Message, 0, n)
	count := 0

	// Walk backwards from current position
	ch.messages.Do(func(v interface{}) {
		if v != nil && count < n {
			messages = append(messages, v.(Message))
			count++
		}
	})

	// Reverse to get chronological order
	for i := 0; i < len(messages)/2; i++ {
		j := len(messages) - 1 - i
		messages[i], messages[j] = messages[j], messages[i]
	}

	return messages
}

// HistoryBot maintains IRC channel history
type HistoryBot struct {
	nick         string
	samAddr      string
	ircDest      string
	channels     []string
	historySize  int

	histories    map[string]*ChannelHistory
	historiesMu  sync.RWMutex

	conn         net.Conn
	sam          *sam3.SAM
	stopCh       chan struct{}
	stoppedCh    chan struct{}
	registered   bool
	registeredMu sync.Mutex
}

// NewHistoryBot creates a new history bot
func NewHistoryBot(nick, samAddr, ircDest string, channels []string, historySize int) *HistoryBot {
	if historySize == 0 {
		historySize = 50 // default to storing last 50 messages
	}

	return &HistoryBot{
		nick:        nick,
		samAddr:     samAddr,
		ircDest:     ircDest,
		channels:    channels,
		historySize: historySize,
		histories:   make(map[string]*ChannelHistory),
		stopCh:      make(chan struct{}),
		stoppedCh:   make(chan struct{}),
	}
}

// Start starts the history bot
func (hb *HistoryBot) Start() error {
	// Initialize channel histories
	for _, channel := range hb.channels {
		hb.histories[strings.ToLower(channel)] = NewChannelHistory(hb.historySize)
	}

	// Connect to I2P SAM
	sam, err := sam3.NewSAM(hb.samAddr)
	if err != nil {
		return fmt.Errorf("failed to connect to SAM: %w", err)
	}
	hb.sam = sam

	// Create streaming session
	keys, err := sam.NewKeys()
	if err != nil {
		sam.Close()
		return fmt.Errorf("failed to generate keys: %w", err)
	}

	stream, err := sam.NewStreamSession("historybot", keys, sam3.Options_Small)
	if err != nil {
		sam.Close()
		return fmt.Errorf("failed to create stream session: %w", err)
	}

	// Lookup and connect to IRC server
	addr, err := stream.Lookup(hb.ircDest)
	if err != nil {
		sam.Close()
		return fmt.Errorf("failed to lookup IRC destination %s: %w", hb.ircDest, err)
	}

	conn, err := stream.DialI2P(addr)
	if err != nil {
		sam.Close()
		return fmt.Errorf("failed to connect to IRC: %w", err)
	}
	hb.conn = conn

	log.Printf("[HistoryBot] Connected to IRC server as %s", hb.nick)

	// Send initial IRC commands
	fmt.Fprintf(conn, "NICK %s\r\n", hb.nick)
	fmt.Fprintf(conn, "USER %s 0 * :%s\r\n", hb.nick, hb.nick)
	log.Printf("[HistoryBot] Sent NICK and USER, waiting for registration...")

	// Start message reader (will join channels after receiving 001)
	go hb.readMessages()

	return nil
}

// Stop stops the history bot
func (hb *HistoryBot) Stop() {
	close(hb.stopCh)
	if hb.conn != nil {
		hb.conn.Close()
	}
	if hb.sam != nil {
		hb.sam.Close()
	}
	<-hb.stoppedCh
}

// readMessages reads and processes IRC messages
func (hb *HistoryBot) readMessages() {
	defer close(hb.stoppedCh)

	for {
		scanner := bufio.NewScanner(hb.conn)
		for scanner.Scan() {
			select {
			case <-hb.stopCh:
				return
			default:
			}

			line := scanner.Text()
			log.Printf("[HistoryBot] Received: %s", line)
			hb.processMessage(line)
		}

		// Connection lost
		if err := scanner.Err(); err != nil {
			log.Printf("[HistoryBot] Error reading: %v", err)
		} else {
			log.Printf("[HistoryBot] Connection closed")
		}

		// Check if we're stopping
		select {
		case <-hb.stopCh:
			return
		default:
		}

		// Attempt reconnection
		if !hb.reconnect() {
			log.Printf("[HistoryBot] Failed to reconnect, stopping bot")
			return
		}
	}
}

// reconnect attempts to reconnect to the IRC server with exponential backoff
func (hb *HistoryBot) reconnect() bool {
	const maxRetries = 10
	const baseDelay = 5 * time.Second
	const maxDelay = 2 * time.Minute

	// Close old connection
	if hb.conn != nil {
		hb.conn.Close()
		hb.conn = nil
	}

	hb.registeredMu.Lock()
	hb.registered = false
	hb.registeredMu.Unlock()

	delay := baseDelay

	for retry := 0; retry < maxRetries; retry++ {
		log.Printf("[HistoryBot] Reconnecting (attempt %d/%d)...", retry+1, maxRetries)

		// Wait before retry
		select {
		case <-time.After(delay):
		case <-hb.stopCh:
			return false
		}

		// Try to connect
		addr, err := hb.sam.Lookup(hb.ircDest)
		if err != nil {
			log.Printf("[HistoryBot] Lookup failed: %v", err)
			delay *= 2
			if delay > maxDelay {
				delay = maxDelay
			}
			continue
		}

		// Create a new SAM stream session with new keys
		keys, err := hb.sam.NewKeys()
		if err != nil {
			log.Printf("[HistoryBot] Failed to generate keys: %v", err)
			delay *= 2
			if delay > maxDelay {
				delay = maxDelay
			}
			continue
		}

		stream, err := hb.sam.NewStreamSession("historybot-reconnect", keys, sam3.Options_Small)
		if err != nil {
			log.Printf("[HistoryBot] Failed to create stream session: %v", err)
			delay *= 2
			if delay > maxDelay {
				delay = maxDelay
			}
			continue
		}

		conn, err := stream.DialI2P(addr)
		if err != nil {
			log.Printf("[HistoryBot] Dial failed: %v", err)
			delay *= 2
			if delay > maxDelay {
				delay = maxDelay
			}
			continue
		}

		// Success!
		hb.conn = conn
		log.Printf("[HistoryBot] Reconnected successfully")

		// Re-register with IRC
		fmt.Fprintf(hb.conn, "NICK %s\r\n", hb.nick)
		fmt.Fprintf(hb.conn, "USER %s 0 * :%s\r\n", hb.nick, hb.nick)

		// Note: channels will be rejoined when we receive 001 in processMessage
		return true
	}

	log.Printf("[HistoryBot] Failed to reconnect after %d attempts", maxRetries)
	return false
}

// processMessage processes a single IRC message
func (hb *HistoryBot) processMessage(line string) {
	// Handle PING
	if strings.HasPrefix(line, "PING ") {
		pong := strings.Replace(line, "PING", "PONG", 1)
		fmt.Fprintf(hb.conn, "%s\r\n", pong)
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
		// Registration successful - now we can join channels
		hb.registeredMu.Lock()
		if !hb.registered {
			hb.registered = true
			hb.registeredMu.Unlock()
			log.Printf("[HistoryBot] Registration complete, joining channels...")
			for _, channel := range hb.channels {
				fmt.Fprintf(hb.conn, "JOIN %s\r\n", channel)
				log.Printf("[HistoryBot] Joining channel: %s", channel)
			}
		} else {
			hb.registeredMu.Unlock()
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

	// JOIN and PART events are intentionally not logged to keep history clean
	case "JOIN":
		// Skip - we don't log join events
		return

	case "PART":
		// Skip - we don't log part events
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
		log.Printf("[HistoryBot] [%s] <%s> %s", channel, msg.Nick, msg.Content)
	}
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
