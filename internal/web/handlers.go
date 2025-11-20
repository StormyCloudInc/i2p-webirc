package web

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"html"
	"html/template"
	"log"
	"net/http"
	"net/url"
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
)

// Config holds configuration for the web handlers
type Config struct {
	SAMAddress string
	IRCDest    string
	MaxUsers   int
}

// Handler holds the HTTP handler state
type Handler struct {
	config    Config
	sessions  *irc.SessionStore
	templates *template.Template
	bot       *bot.HistoryBot
}

// NewHandler creates a new HTTP handler
func NewHandler(config Config, sessions *irc.SessionStore, templates *template.Template, historyBot *bot.HistoryBot) *Handler {
	return &Handler{
		config:    config,
		sessions:  sessions,
		templates: templates,
		bot:       historyBot,
	}
}

// generateSessionID generates a random session ID
func generateSessionID() string {
	bytes := make([]byte, SessionIDLength)
	rand.Read(bytes)
	return hex.EncodeToString(bytes)
}

// getMessagesWithHistory gets channel messages, prepending bot history if available and channel is new
func (h *Handler) getMessagesWithHistory(channel string, ch *irc.ChannelState) []irc.ChatMessage {
	// If history already loaded for this channel, just return current messages
	if ch.IsHistoryLoaded() {
		return ch.GetMessages()
	}

	// Mark history as loaded (even if we don't find any, to avoid repeated lookups)
	ch.SetHistoryLoaded()

	// If no bot or channel doesn't start with #, return messages as-is
	if h.bot == nil || !strings.HasPrefix(channel, "#") {
		return ch.GetMessages()
	}

	// Get bot history for this channel
	botHistory := h.bot.GetHistory(channel, 10)
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
		Secure:   true, // Enforce secure cookies
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
	data := map[string]interface{}{
		"DefaultNick":    fmt.Sprintf("webuser%d", time.Now().Unix()%10000),
		"DefaultChannel": "#i2p-chat",
		"LastChannel":    "",
		"Theme":          theme,
		"CSRFToken":      h.GetCSRFToken(r),
	}

	if session != nil {
		session.UpdateLastHTTP()
		lastChan := session.GetCurrentChannel()
		if lastChan != "" {
			data["LastChannel"] = lastChan
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

	nick := strings.TrimSpace(r.FormValue("nick"))
	channel := strings.TrimSpace(r.FormValue("channel"))

	if nick == "" {
		nick = fmt.Sprintf("webuser%d", time.Now().Unix()%10000)
	}
	if channel == "" || !strings.HasPrefix(channel, "#") {
		channel = "#i2p-chat"
	}

	// Get or create IRC session
	session := h.sessions.Get(sessionID)
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

		// Create new session
		dialer := &irc.SamIRCDialer{
			SAMAddress: h.config.SAMAddress,
			IRCDest:    h.config.IRCDest,
			SessionID:  sessionID,
		}

		session = irc.NewIRCSession(sessionID, dialer, nick, nick, "WebIRC User")
		h.sessions.Set(sessionID, session)

		// Start the IRC connection (non-blocking)
		if err := session.Start(); err != nil {
			log.Printf("Failed to start IRC session: %v", err)
			http.Error(w, "Failed to connect to IRC", http.StatusInternalServerError)
			return
		}

		log.Printf("Started IRC session %s, redirecting to connecting page", sessionID)
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

	// Get messages (history loading happens in MessagesHandler iframe)
	messages := ch.GetMessages()
	users := ch.GetUsers()
	sort.Strings(users)

	// Get user preferences
	hideJoinPart := h.getPref(r, PrefHideJoinPart, "false") == "true"
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

	// Get messages with bot history if available
	messages := h.getMessagesWithHistory(channel, ch)
	users := ch.GetUsers()
	sort.Strings(users)

	// Get user preferences
	hideJoinPart := h.getPref(r, PrefHideJoinPart, "false") == "true"
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
		channel = "#i2p-chat"
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
				log.Printf("Session %s ready, channel %s has %d users, redirecting", sessionID, channel, len(users))
				http.Redirect(w, r, "/chan/"+url.PathEscape(channel), http.StatusSeeOther)
				return
			}
		}

		// Registered but haven't joined yet - send JOIN command
		log.Printf("Session %s registered, sending JOIN for %s", sessionID, channel)
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

// StatusHandler handles GET /status - debugging endpoint
func (h *Handler) StatusHandler(w http.ResponseWriter, r *http.Request) {
	sessionID := h.getOrCreateSessionID(w, r)
	session := h.sessions.Get(sessionID)

	w.Header().Set("Content-Type", "text/plain")
	fmt.Fprintf(w, "I2P WebIRC Status\n")
	fmt.Fprintf(w, "=================\n\n")
	fmt.Fprintf(w, "SAM Address: %s\n", h.config.SAMAddress)
	fmt.Fprintf(w, "IRC Destination: %s\n", h.config.IRCDest)
	fmt.Fprintf(w, "Session ID: %s\n", sessionID)

	if session != nil {
		fmt.Fprintf(w, "Session Status: %s\n", session.GetStatus())
		fmt.Fprintf(w, "Session Nick: %s\n", session.GetNick())
		fmt.Fprintf(w, "Current Channel: %s\n", session.GetCurrentChannel())
	} else {
		fmt.Fprintf(w, "Session: Not initialized\n")
	}
}

// DebugHistoryHandler handles GET /debug/history?channel=NAME - shows what bot has recorded
func (h *Handler) DebugHistoryHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")

	if h.bot == nil {
		fmt.Fprintf(w, "History Bot Debug\n")
		fmt.Fprintf(w, "=================\n\n")
		fmt.Fprintf(w, "ERROR: History bot is not running\n")
		return
	}

	channel := r.URL.Query().Get("channel")
	if channel == "" {
		fmt.Fprintf(w, "History Bot Debug\n")
		fmt.Fprintf(w, "=================\n\n")
		fmt.Fprintf(w, "Usage: /debug/history?channel=#i2p-chat\n")
		fmt.Fprintf(w, "\nAvailable channels: #i2p-chat, #i2p\n")
		return
	}

	// Get up to 50 messages from bot history
	messages := h.bot.GetHistory(channel, 50)

	fmt.Fprintf(w, "History Bot Debug\n")
	fmt.Fprintf(w, "=================\n\n")
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

	// Handle commands
	if strings.HasPrefix(message, "/") {
		parts := strings.Fields(message)
		command := strings.ToLower(parts[0])

		switch command {
		case "/nick":
			if len(parts) >= 2 {
				newNick := parts[1]
				session.SendMessage(fmt.Sprintf("NICK %s", newNick))
				session.SetNick(newNick)
			}

		case "/join":
			if len(parts) >= 2 {
				newChannel := parts[1]
				if !strings.HasPrefix(newChannel, "#") {
					newChannel = "#" + newChannel
				}
				session.SendMessage(fmt.Sprintf("JOIN %s", newChannel))
				// Render HTML to navigate parent frame
				w.Header().Set("Content-Type", "text/html")
				fmt.Fprintf(w, `<!DOCTYPE html><html><head><meta http-equiv="refresh" content="0;url=/chan/%s"><script>if(window.parent){window.parent.location.href='/chan/%s';}</script></head><body>Joining %s...</body></html>`,
					url.PathEscape(newChannel), url.PathEscape(newChannel), html.EscapeString(newChannel))
				return
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

				// Render HTML to navigate parent frame to DM view
				w.Header().Set("Content-Type", "text/html")
				fmt.Fprintf(w, `<!DOCTYPE html><html><head><meta http-equiv="refresh" content="0;url=/chan/%s"><script>if(window.parent){window.parent.location.href='/chan/%s';}</script></head><body>Opening DM with %s...</body></html>`,
					url.PathEscape(targetNick), url.PathEscape(targetNick), html.EscapeString(targetNick))
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
			// Unknown command - send as-is to IRC
			session.SendMessage(strings.TrimPrefix(message, "/"))
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
