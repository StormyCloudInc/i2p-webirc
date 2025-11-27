package web

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/dustinfields/i2p-irc/internal/bot"
	"github.com/dustinfields/i2p-irc/internal/irc"
)

const (
	SessionCookieName = "webirc_session"
	SessionIDLength   = 32
	PrefHideJoinPart  = "hide_joinpart"
	PrefTheme         = "theme"
	MaxNickLength     = 30
	MaxChannelLength  = 50
	MaxMessageLength  = 450 // IRC limit is 512 including headers, leave room
)

// Validation patterns for IRC names
var (
	// Nicknames: letters, numbers, and common IRC chars, no spaces or control chars
	validNickRegex = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_\-\[\]\\^{}|` + "`" + `]{0,29}$`)
	// Channels: must start with #, then letters/numbers/some punctuation
	validChannelRegex = regexp.MustCompile(`^#[a-zA-Z0-9_\-\.]{1,49}$`)
)

// IRCServer represents an available IRC server
type IRCServer struct {
	ID              string   // "postman", "simp"
	Name            string   // "Postman IRC", "Simp IRC"
	Address         string   // "irc.postman.i2p", "irc.simp.i2p"
	DefaultChannel  string   // Default channel for this server
	PopularChannels []string // Popular channels to show in sidebar
}

// AvailableServers is the whitelist of IRC servers users can connect to
var AvailableServers = []IRCServer{
	{
		ID:              "postman",
		Name:            "Postman IRC",
		Address:         "irc.postman.i2p",
		DefaultChannel:  "#i2p-chat",
		PopularChannels: []string{"#i2p-chat", "#i2p", "#i2pd", "#saltr", "#torrents", "#freedom", "#i2p-news", "#ai-chat"},
	},
	{
		ID:              "simp",
		Name:            "Simp IRC",
		Address:         "edg3okvdbjcsbeqrwsakn6jog4mw27i3ri4ychl7kdn3u4xhtf3a.b32.i2p:6667",
		DefaultChannel:  "#simp",
		PopularChannels: []string{"#simp", "#ru", "#en", "#guessthesong", "#gatheryourparty"},
	},
}

// getServerByID returns the server config or nil if not found
func getServerByID(id string) *IRCServer {
	for _, s := range AvailableServers {
		if s.ID == id {
			return &s
		}
	}
	return nil
}

// Config holds configuration for the web handlers
type Config struct {
	SAMAddress string
	IRCDest    string
	MaxUsers   int
	DebugMode  bool // Enable debug endpoints (/status, /debug/*)
}

// DialerFactory creates IRC dialers (allows mock injection for testing)
type DialerFactory func(samAddr, ircDest, sessionID string) irc.IRCDialer

// DefaultDialerFactory returns the default SAM-based dialer factory
func DefaultDialerFactory() DialerFactory {
	return func(samAddr, ircDest, sessionID string) irc.IRCDialer {
		return &irc.SamIRCDialer{
			SAMAddress: samAddr,
			IRCDest:    ircDest,
			SessionID:  sessionID,
		}
	}
}

// Handler holds the HTTP handler state
type Handler struct {
	config        Config
	sessions      *irc.SessionStore
	templates     *template.Template
	bots          map[string]*bot.HistoryBot // key is server ID (e.g., "postman", "simp")
	dialerFactory DialerFactory              // factory for creating IRC dialers (allows testing)
	metrics       *Metrics                   // application metrics for monitoring
}

// NewHandler creates a new HTTP handler with default dialer factory and new metrics
func NewHandler(config Config, sessions *irc.SessionStore, templates *template.Template, historyBots map[string]*bot.HistoryBot) *Handler {
	return NewHandlerWithMetrics(config, sessions, templates, historyBots, nil, nil)
}

// NewHandlerWithDialer creates a new HTTP handler with custom dialer factory (for testing)
func NewHandlerWithDialer(config Config, sessions *irc.SessionStore, templates *template.Template, historyBots map[string]*bot.HistoryBot, dialerFactory DialerFactory) *Handler {
	return NewHandlerWithMetrics(config, sessions, templates, historyBots, dialerFactory, nil)
}

// NewHandlerWithMetrics creates a new HTTP handler with metrics and optional custom dialer
func NewHandlerWithMetrics(config Config, sessions *irc.SessionStore, templates *template.Template, historyBots map[string]*bot.HistoryBot, dialerFactory DialerFactory, metrics *Metrics) *Handler {
	if dialerFactory == nil {
		dialerFactory = DefaultDialerFactory()
	}
	if metrics == nil {
		metrics = NewMetrics()
	}
	return &Handler{
		config:        config,
		sessions:      sessions,
		templates:     templates,
		bots:          historyBots,
		dialerFactory: dialerFactory,
		metrics:       metrics,
	}
}

// GetMetrics returns the handler's metrics instance
func (h *Handler) GetMetrics() *Metrics {
	return h.metrics
}

// getHistoryBot returns the history bot for a given server ID
func (h *Handler) getHistoryBot(serverID string) *bot.HistoryBot {
	if h.bots == nil {
		return nil
	}
	return h.bots[serverID]
}

// generateSessionID generates a random session ID
func generateSessionID() string {
	bytes := make([]byte, SessionIDLength)
	if _, err := rand.Read(bytes); err != nil {
		// Cryptographic failure is unrecoverable - fail hard rather than use weak randomness
		panic(fmt.Sprintf("crypto/rand.Read failed: %v", err))
	}
	return hex.EncodeToString(bytes)
}

// isValidNick checks if a nickname is valid for IRC
func isValidNick(nick string) bool {
	if nick == "" || len(nick) > MaxNickLength {
		return false
	}
	return validNickRegex.MatchString(nick)
}

// isValidChannel checks if a channel name is valid for IRC
func isValidChannel(channel string) bool {
	if channel == "" || len(channel) > MaxChannelLength {
		return false
	}
	return validChannelRegex.MatchString(channel)
}

// sanitizeNick cleans a nickname, returning a valid one or empty string
func sanitizeNick(nick string) string {
	nick = strings.TrimSpace(nick)
	if isValidNick(nick) {
		return nick
	}
	return ""
}

// sanitizeChannel cleans a channel name, returning a valid one or empty string
func sanitizeChannel(channel string) string {
	channel = strings.TrimSpace(channel)
	if !strings.HasPrefix(channel, "#") {
		channel = "#" + channel
	}
	if isValidChannel(channel) {
		return channel
	}
	return ""
}

// generateDefaultNick creates a random default nickname
func generateDefaultNick() string {
	bytes := make([]byte, 4)
	if _, err := rand.Read(bytes); err != nil {
		// Fallback to time-based if crypto fails (shouldn't happen)
		return fmt.Sprintf("user%d", time.Now().UnixNano()%100000)
	}
	// Use hex encoding for alphanumeric result
	return "user" + hex.EncodeToString(bytes)
}

// getMessagesWithHistory gets channel messages, prepending bot history if available and channel is new
func (h *Handler) getMessagesWithHistory(channel string, ch *irc.ChannelState, serverID string) []irc.ChatMessage {
	// If history already loaded for this channel, just return current messages
	if ch.IsHistoryLoaded() {
		return ch.GetMessages()
	}

	// Mark history as loaded (even if we don't find any, to avoid repeated lookups)
	ch.SetHistoryLoaded()

	// Get the history bot for this server
	historyBot := h.getHistoryBot(serverID)

	// If no bot or channel doesn't start with #, return messages as-is
	if historyBot == nil || !strings.HasPrefix(channel, "#") {
		return ch.GetMessages()
	}

	// Get bot history for this channel
	botHistory := historyBot.GetHistory(channel, 10)
	if len(botHistory) == 0 {
		return ch.GetMessages()
	}

	// Convert bot messages to IRC ChatMessages and add them to channel state
	// Add in reverse order so oldest messages appear first
	for i := len(botHistory) - 1; i >= 0; i-- {
		msg := botHistory[i]

		// Skip join/part events - users can see these from current activity
		if msg.Type == "join" || msg.Type == "part" {
			continue
		}

		kind := "privmsg"
		if msg.Type == "action" {
			kind = "action"
		}

		// Add to channel message buffer
		ch.AddMessage(irc.ChatMessage{
			Time:   msg.Timestamp,
			Prefix: msg.Nick,
			Text:   msg.Content,
			Kind:   kind,
		})
	}

	return ch.GetMessages()
}

// getOrCreateSessionID gets or creates a session ID from cookies
func (h *Handler) getOrCreateSessionID(w http.ResponseWriter, r *http.Request) string {
	cookie, err := r.Cookie(SessionCookieName)
	if err == nil && cookie.Value != "" {
		return cookie.Value
	}

	// Create new session ID
	sessionID := generateSessionID()
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    sessionID,
		Path:     "/",
		MaxAge:   86400 * 7, // 7 days
		HttpOnly: true,
		Secure:   isSecureRequest(r),
		SameSite: http.SameSiteLaxMode,
	})

	return sessionID
}

// IndexHandler handles GET /
func (h *Handler) IndexHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	sessionID := h.getOrCreateSessionID(w, r)
	session := h.sessions.Get(sessionID)

	theme := h.getTheme(w, r)
	defaultServer := getServerByID("postman")
	data := map[string]interface{}{
		"DefaultNick":    generateDefaultNick(),
		"DefaultChannel": defaultServer.DefaultChannel,
		"DefaultServer":  "postman",
		"Servers":        AvailableServers,
		"LastChannel":    "",
		"LastServer":     "",
		"Theme":          theme,
		"CSRFToken":      h.GetCSRFToken(r),
	}

	if session != nil {
		session.UpdateLastHTTP()
		lastChan := session.GetCurrentChannel()
		if lastChan != "" {
			data["LastChannel"] = lastChan
		}
		lastServer := session.GetServerID()
		if lastServer != "" {
			data["LastServer"] = lastServer
		}
	}

	if err := h.templates.ExecuteTemplate(w, "index.html", data); err != nil {
		log.Printf("Error rendering template: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
	}
}

// JoinHandler handles POST /join
func (h *Handler) JoinHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	sessionID := h.getOrCreateSessionID(w, r)

	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}

	nick := sanitizeNick(r.FormValue("nick"))
	channel := sanitizeChannel(r.FormValue("channel"))

	// Parse server selection (default to postman)
	serverID := r.FormValue("server")
	if serverID == "" {
		serverID = "postman"
	}

	server := getServerByID(serverID)
	if server == nil {
		http.Error(w, "Invalid server", http.StatusBadRequest)
		return
	}

	// Use defaults if validation failed
	if nick == "" {
		nick = generateDefaultNick()
	}
	if channel == "" {
		channel = server.DefaultChannel
	}

	// Get or create IRC session
	session := h.sessions.Get(sessionID)

	// If existing session is for a different server, close it
	if session != nil && session.GetServerID() != "" && session.GetServerID() != serverID {
		log.Printf("Session %s switching servers from %s to %s, closing old session",
			sessionID[:8], session.GetServerID(), serverID)
		session.Close()
		h.sessions.Delete(sessionID)
		session = nil
	}

	if session == nil {
		// Check if server is at capacity
		if h.sessions.Count() >= h.config.MaxUsers {
			log.Printf("Server at capacity: %d/%d users", h.sessions.Count(), h.config.MaxUsers)
			theme := h.getTheme(w, r)
			data := map[string]interface{}{
				"CurrentUsers": h.sessions.Count(),
				"MaxUsers":     h.config.MaxUsers,
				"Theme":        theme,
				"CSRFToken":    h.GetCSRFToken(r),
			}
			if err := h.templates.ExecuteTemplate(w, "full.html", data); err != nil {
				http.Error(w, "Server at capacity", http.StatusServiceUnavailable)
			}
			return
		}

		// Create new session with selected server using the dialer factory
		dialer := h.dialerFactory(h.config.SAMAddress, server.Address, sessionID)

		session = irc.NewIRCSession(sessionID, dialer, nick, nick, "WebIRC User")
		session.SetServerID(serverID)
		h.sessions.Set(sessionID, session)

		// Start the IRC connection (non-blocking)
		if err := session.Start(); err != nil {
			log.Printf("Failed to start IRC session: %v", err)
			http.Error(w, "Failed to connect to IRC", http.StatusInternalServerError)
			return
		}

		log.Printf("Started IRC session %s to %s (%s), redirecting to connecting page",
			sessionID[:8], server.Name, server.Address)
	}

	session.UpdateLastHTTP()
	session.SetCurrentChannel(channel)

	// If already registered, join the channel and go directly to chat
	if session.GetRegistered() {
		session.SendMessage(fmt.Sprintf("JOIN %s", channel))
		http.Redirect(w, r, "/chan/"+url.PathEscape(channel), http.StatusSeeOther)
		return
	}

	// Not yet registered - show connecting page
	// The connecting page will auto-refresh and redirect when ready
	http.Redirect(w, r, "/connecting?channel="+url.QueryEscape(channel), http.StatusSeeOther)
}

// ChannelHandler handles GET /chan/{channel}
func (h *Handler) ChannelHandler(w http.ResponseWriter, r *http.Request) {
	sessionID := h.getOrCreateSessionID(w, r)
	session := h.sessions.Get(sessionID)

	if session == nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	session.UpdateLastHTTP()

	// Extract channel from path
	channel := strings.TrimPrefix(r.URL.Path, "/chan/")
	channel, _ = url.PathUnescape(channel)

	if channel == "" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	session.SetCurrentChannel(channel)
	session.MarkChannelSeen(channel)

	// Get channel state
	ch := session.GetChannel(channel)
	if ch == nil {
		ch = session.GetOrCreateChannel(channel)
	}

	// Auto-join channel if not already joined (only for # channels)
	if strings.HasPrefix(channel, "#") && session.GetRegistered() {
		users := ch.GetUsers()
		// If no users yet, we probably haven't joined - send JOIN command
		if len(users) == 0 {
			log.Printf("Session %s auto-joining %s", sessionID[:8], channel)
			session.SendMessage(fmt.Sprintf("JOIN %s", channel))
		}
	}

	// Get messages (history loading happens in MessagesHandler iframe)
	messages := ch.GetMessages()
	users := ch.GetUsers()
	sort.Strings(users)

	// Get user preferences (hide join/part by default)
	hideJoinPart := h.getPref(r, PrefHideJoinPart, "true") == "true"
	theme := h.getTheme(w, r)

	// Get all channels for sidebar
	allChannels := session.GetAllChannels()

	data := map[string]interface{}{
		"Channel":      channel,
		"Nick":         session.GetNick(),
		"Status":       session.GetStatus(),
		"Messages":     messages,
		"Users":        users,
		"HideJoinPart": hideJoinPart,
		"Theme":        theme,
		"AllChannels":  allChannels,
		"CSRFToken":    h.GetCSRFToken(r),
	}

	if err := h.templates.ExecuteTemplate(w, "channel.html", data); err != nil {
		log.Printf("Error rendering template: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
	}
}

// MessagesHandler handles GET /chan/{channel}/messages - iframe for messages only
func (h *Handler) MessagesHandler(w http.ResponseWriter, r *http.Request) {
	sessionID := h.getOrCreateSessionID(w, r)
	session := h.sessions.Get(sessionID)

	if session == nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	session.UpdateLastHTTP()

	// Extract channel from path
	channel := strings.TrimPrefix(r.URL.Path, "/chan/")
	channel = strings.TrimSuffix(channel, "/messages")
	channel, _ = url.PathUnescape(channel)

	if channel == "" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	session.MarkChannelSeen(channel)

	// Get channel state
	ch := session.GetChannel(channel)
	if ch == nil {
		ch = session.GetOrCreateChannel(channel)
	}

	// Get server ID for this session
	serverID := session.GetServerID()

	// Get messages with bot history if available
	messages := h.getMessagesWithHistory(channel, ch, serverID)
	users := ch.GetUsers()
	sort.Strings(users)

	// Get user preferences (hide join/part by default)
	hideJoinPart := h.getPref(r, PrefHideJoinPart, "true") == "true"
	theme := h.getTheme(w, r)

	// Get all channels for sidebar
	allChannels := session.GetAllChannels()
	server := getServerByID(serverID)
	var popularChannels []string
	if server != nil {
		popularChannels = server.PopularChannels
	} else {
		// Fallback to postman channels
		popularChannels = AvailableServers[0].PopularChannels
	}

	data := map[string]interface{}{
		"Channel":         channel,
		"Nick":            session.GetNick(),
		"Status":          session.GetStatus(),
		"Messages":        messages,
		"Users":           users,
		"HideJoinPart":    hideJoinPart,
		"Theme":           theme,
		"AllChannels":     allChannels,
		"PopularChannels": popularChannels,
		"CSRFToken":       h.GetCSRFToken(r),
	}

	if err := h.templates.ExecuteTemplate(w, "messages.html", data); err != nil {
		log.Printf("Error rendering template: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
	}
}

// ConnectingHandler handles GET /connecting - shows connection progress
func (h *Handler) ConnectingHandler(w http.ResponseWriter, r *http.Request) {
	sessionID := h.getOrCreateSessionID(w, r)
	session := h.sessions.Get(sessionID)

	if session == nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	session.UpdateLastHTTP()

	// Get the target channel from query params
	channel := r.URL.Query().Get("channel")
	if channel == "" {
		channel = session.GetCurrentChannel()
	}
	if channel == "" {
		// Use server-specific default channel
		serverID := session.GetServerID()
		server := getServerByID(serverID)
		if server != nil {
			channel = server.DefaultChannel
		} else {
			channel = "#i2p-chat"
		}
	}

	// Check if registered
	if session.GetRegistered() {
		// Check if we've already joined the channel (has users or messages)
		ch := session.GetChannel(channel)
		if ch != nil {
			users := ch.GetUsers()
			messages := ch.GetMessages()
			// If we have users or messages, the channel is ready
			if len(users) > 0 || len(messages) > 0 {
				log.Printf("Session %s ready, channel %s has %d users, redirecting", sessionID[:8], channel, len(users))
				http.Redirect(w, r, "/chan/"+url.PathEscape(channel), http.StatusSeeOther)
				return
			}
		}

		// Registered but haven't joined yet - send JOIN command
		log.Printf("Session %s registered, sending JOIN for %s", sessionID[:8], channel)
		session.SendMessage(fmt.Sprintf("JOIN %s", channel))
		// Continue showing connecting page, will redirect on next refresh when users arrive
	}

	// Still connecting, show progress page
	theme := h.getTheme(w, r)
	data := map[string]interface{}{
		"Status":     session.GetStatus(),
		"Registered": session.GetRegistered(),
		"Nick":       session.GetNick(),
		"Channel":    channel,
		"Theme":      theme,
		"CSRFToken":  h.GetCSRFToken(r),
	}

	if err := h.templates.ExecuteTemplate(w, "connecting.html", data); err != nil {
		log.Printf("Error rendering template: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
	}
}

// StatusHandler handles GET /status - debugging endpoint (requires DebugMode)
func (h *Handler) StatusHandler(w http.ResponseWriter, r *http.Request) {
	if !h.config.DebugMode {
		http.NotFound(w, r)
		return
	}

	sessionID := h.getOrCreateSessionID(w, r)
	session := h.sessions.Get(sessionID)

	w.Header().Set("Content-Type", "text/plain")
	fmt.Fprintf(w, "I2P WebIRC Status\n")
	fmt.Fprintf(w, "=================\n\n")
	fmt.Fprintf(w, "SAM Address: %s\n", h.config.SAMAddress)
	fmt.Fprintf(w, "IRC Destination: %s\n", h.config.IRCDest)
	// Don't expose full session ID in debug output - show truncated version
	fmt.Fprintf(w, "Session ID: %s...\n", sessionID[:8])

	if session != nil {
		fmt.Fprintf(w, "Session Status: %s\n", session.GetStatus())
		fmt.Fprintf(w, "Session Nick: %s\n", session.GetNick())
		fmt.Fprintf(w, "Current Channel: %s\n", session.GetCurrentChannel())
	} else {
		fmt.Fprintf(w, "Session: Not initialized\n")
	}
}

// DebugHistoryHandler handles GET /debug/history?server=NAME&channel=NAME - shows what bot has recorded (requires DebugMode)
func (h *Handler) DebugHistoryHandler(w http.ResponseWriter, r *http.Request) {
	if !h.config.DebugMode {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")

	fmt.Fprintf(w, "History Bot Debug\n")
	fmt.Fprintf(w, "=================\n\n")

	if h.bots == nil || len(h.bots) == 0 {
		fmt.Fprintf(w, "ERROR: No history bots are running\n")
		return
	}

	// List available bots
	fmt.Fprintf(w, "Available bots:\n")
	for serverID := range h.bots {
		fmt.Fprintf(w, "  - %s\n", serverID)
	}
	fmt.Fprintf(w, "\n")

	serverID := r.URL.Query().Get("server")
	channel := r.URL.Query().Get("channel")

	if serverID == "" || channel == "" {
		fmt.Fprintf(w, "Usage: /debug/history?server=postman&channel=#i2p-chat\n")
		fmt.Fprintf(w, "       /debug/history?server=simp&channel=#simp\n")
		return
	}

	historyBot := h.getHistoryBot(serverID)
	if historyBot == nil {
		fmt.Fprintf(w, "ERROR: No history bot found for server '%s'\n", serverID)
		return
	}

	// Get up to 50 messages from bot history
	messages := historyBot.GetHistory(channel, 50)

	fmt.Fprintf(w, "Server: %s\n", serverID)
	fmt.Fprintf(w, "Channel: %s\n", channel)
	fmt.Fprintf(w, "Messages stored: %d\n\n", len(messages))

	if len(messages) == 0 {
		fmt.Fprintf(w, "No messages recorded for this channel.\n")
		fmt.Fprintf(w, "\nPossible reasons:\n")
		fmt.Fprintf(w, "- Bot hasn't joined this channel\n")
		fmt.Fprintf(w, "- No activity in channel yet\n")
		fmt.Fprintf(w, "- Channel name is case-sensitive (try lowercase)\n")
		return
	}

	fmt.Fprintf(w, "Recent messages:\n")
	fmt.Fprintf(w, "----------------\n\n")

	for i, msg := range messages {
		fmt.Fprintf(w, "[%d] %s\n", i+1, msg.Timestamp.Format("15:04:05"))
		fmt.Fprintf(w, "    Type: %s\n", msg.Type)
		fmt.Fprintf(w, "    Nick: %s\n", msg.Nick)
		if msg.Content != "" {
			fmt.Fprintf(w, "    Content: %s\n", msg.Content)
		} else {
			fmt.Fprintf(w, "    Content: (empty - %s event)\n", msg.Type)
		}
		fmt.Fprintf(w, "\n")
	}
}

// HealthHandler handles GET /health - health check endpoint for monitoring (UptimeRobot, etc.)
func (h *Handler) HealthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	healthy := true
	var issues []string

	// Check session store is accessible
	sessionCount := h.sessions.Count()

	// Check history bots health
	botsHealthy := 0
	botsTotal := 0
	botStatus := make(map[string]bool)

	if h.bots != nil {
		for serverID, bot := range h.bots {
			botsTotal++
			if bot.IsHealthy() {
				botsHealthy++
				botStatus[serverID] = true
			} else {
				botStatus[serverID] = false
				issues = append(issues, fmt.Sprintf("history bot '%s' unhealthy", serverID))
			}
		}
	}

	// If any bot is unhealthy, mark overall health as degraded
	if botsTotal > 0 && botsHealthy < botsTotal {
		healthy = false
	}

	// Build response
	status := "ok"
	statusCode := http.StatusOK
	if !healthy {
		status = "unhealthy"
		statusCode = http.StatusServiceUnavailable
	}

	w.WriteHeader(statusCode)
	fmt.Fprintf(w, `{"status":"%s","sessions":%d,"bots":{"total":%d,"healthy":%d,"details":{`,
		status, sessionCount, botsTotal, botsHealthy)

	// Add bot details
	first := true
	for serverID, isHealthy := range botStatus {
		if !first {
			fmt.Fprint(w, ",")
		}
		first = false
		fmt.Fprintf(w, `"%s":%t`, serverID, isHealthy)
	}

	fmt.Fprint(w, "}}")

	if len(issues) > 0 {
		fmt.Fprintf(w, `,"issues":[`)
		for i, issue := range issues {
			if i > 0 {
				fmt.Fprint(w, ",")
			}
			fmt.Fprintf(w, `"%s"`, issue)
		}
		fmt.Fprint(w, "]")
	}

	fmt.Fprint(w, "}")
}

// SendHandler handles POST /send
func (h *Handler) SendHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	sessionID := h.getOrCreateSessionID(w, r)
	session := h.sessions.Get(sessionID)

	if session == nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	session.UpdateLastHTTP()

	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}

	channel := r.FormValue("channel")
	message := strings.TrimSpace(r.FormValue("message"))

	if message == "" {
		// Empty message, just redirect back
		http.Redirect(w, r, "/chan/"+url.PathEscape(channel), http.StatusSeeOther)
		return
	}

	// Enforce message length limit to prevent IRC protocol issues
	if len(message) > MaxMessageLength {
		message = message[:MaxMessageLength]
	}

	// Handle commands
	if strings.HasPrefix(message, "/") {
		parts := strings.Fields(message)
		command := strings.ToLower(parts[0])

		switch command {
		case "/nick":
			if len(parts) >= 2 {
				newNick := sanitizeNick(parts[1])
				if newNick == "" {
					ch := session.GetOrCreateChannel(channel)
					ch.AddMessage(irc.ChatMessage{
						Time:   time.Now(),
						Prefix: "system",
						Text:   "Invalid nickname. Use letters, numbers, and _ - [ ] \\ ^ { } |",
						Kind:   "system",
					})
				} else {
					session.SendMessage(fmt.Sprintf("NICK %s", newNick))
					session.SetNick(newNick)
				}
			}

		case "/join":
			if len(parts) >= 2 {
				newChannel := sanitizeChannel(parts[1])
				if newChannel == "" {
					ch := session.GetOrCreateChannel(channel)
					ch.AddMessage(irc.ChatMessage{
						Time:   time.Now(),
						Prefix: "system",
						Text:   "Invalid channel name. Use #channel with letters, numbers, _ - .",
						Kind:   "system",
					})
				} else {
					session.SendMessage(fmt.Sprintf("JOIN %s", newChannel))
					// Redirect to new channel - use target="_parent" in the redirect page
					http.Redirect(w, r, "/chan/"+url.PathEscape(newChannel), http.StatusSeeOther)
					return
				}
			}

		case "/part", "/leave":
			session.SendMessage(fmt.Sprintf("PART %s", channel))
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return

		case "/list":
			session.SendMessage("LIST")

		case "/msg", "/query":
			// Handle /msg NICKNAME MESSAGE - send DM and switch to DM view
			if len(parts) >= 3 {
				targetNick := parts[1]
				dmText := strings.TrimPrefix(message, parts[0]+" "+parts[1]+" ")

				// Send the message
				session.SendMessage(fmt.Sprintf("PRIVMSG %s :%s", targetNick, dmText))

				// Add to local DM buffer
				ch := session.GetOrCreateChannel(targetNick)
				ch.AddMessage(irc.ChatMessage{
					Time:   time.Now(),
					Prefix: session.GetNick(),
					Text:   dmText,
					Kind:   "privmsg",
				})

				// Redirect to DM view
				http.Redirect(w, r, "/chan/"+url.PathEscape(targetNick), http.StatusSeeOther)
				return
			}

		case "/me":
			// Handle /me as CTCP ACTION
			if len(parts) >= 2 {
				action := strings.TrimPrefix(message, "/me ")
				session.SendMessage(fmt.Sprintf("PRIVMSG %s :\x01ACTION %s\x01", channel, action))

				// Add to local buffer
				ch := session.GetOrCreateChannel(channel)
				ch.AddMessage(irc.ChatMessage{
					Time:   time.Now(),
					Prefix: session.GetNick(),
					Text:   action,
					Kind:   "action",
				})
			}

		default:
			// Unknown command - do NOT send to IRC (prevents protocol injection)
			// Show error message to user
			ch := session.GetOrCreateChannel(channel)
			ch.AddMessage(irc.ChatMessage{
				Time:   time.Now(),
				Prefix: "system",
				Text:   fmt.Sprintf("Unknown command: %s. Available: /nick, /join, /part, /msg, /me, /list", command),
				Kind:   "system",
			})
		}
	} else {
		// Regular message
		session.SendMessage(fmt.Sprintf("PRIVMSG %s :%s", channel, message))

		// Add to local buffer immediately so it shows up
		ch := session.GetOrCreateChannel(channel)
		ch.AddMessage(irc.ChatMessage{
			Time:   time.Now(),
			Prefix: session.GetNick(),
			Text:   message,
			Kind:   "privmsg",
		})
	}

	// Redirect back to channel messages (iframe content)
	http.Redirect(w, r, "/chan/"+url.PathEscape(channel)+"/messages", http.StatusSeeOther)
}

// getPref gets a preference from cookies
func (h *Handler) getPref(r *http.Request, name, defaultValue string) string {
	cookie, err := r.Cookie(name)
	if err != nil || cookie.Value == "" {
		return defaultValue
	}
	return cookie.Value
}

// getTheme gets theme from URL query param, then cookie, then default
// If URL param is present, also sets the cookie for future visits
func (h *Handler) getTheme(w http.ResponseWriter, r *http.Request) string {
	// Check URL query parameter first
	themeParam := r.URL.Query().Get("theme")
	if themeParam == "light" || themeParam == "dark" {
		// Set cookie so theme persists for future visits
		h.setPref(w, PrefTheme, themeParam)
		return themeParam
	}

	// Fall back to cookie preference
	return h.getPref(r, PrefTheme, "dark")
}

// setPref sets a preference cookie
func (h *Handler) setPref(w http.ResponseWriter, name, value string) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		MaxAge:   86400 * 365, // 1 year
		HttpOnly: true,
		Secure:   true, // Enforce secure cookies
		SameSite: http.SameSiteLaxMode,
	})
}

// SettingsHandler handles POST /settings - update user preferences
func (h *Handler) SettingsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}

	// Update hide join/part preference
	hideJoinPart := r.FormValue("hide_joinpart")
	if hideJoinPart == "on" {
		h.setPref(w, PrefHideJoinPart, "true")
	} else {
		h.setPref(w, PrefHideJoinPart, "false")
	}

	// Update theme preference
	theme := r.FormValue("theme")
	if theme == "light" || theme == "dark" {
		h.setPref(w, PrefTheme, theme)
	}

	// Redirect back to channel
	channel := r.FormValue("channel")
	if channel != "" {
		http.Redirect(w, r, "/chan/"+url.PathEscape(channel), http.StatusSeeOther)
	} else {
		http.Redirect(w, r, "/", http.StatusSeeOther)
	}
}
