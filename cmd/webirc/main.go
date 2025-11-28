/*
I2P WebIRC - JavaScript-free Web IRC Client for I2P

PREREQUISITES:
--------------
1. I2P router must be running with SAM bridge enabled on 127.0.0.1:7656
   - To enable SAM: edit i2p router config and enable SAM application
   - Default SAM port is 7656

2. Install dependencies:
   go get github.com/go-i2p/go-sam3

BUILD:
------
go build -o webirc ./cmd/webirc

RUN:
----
./webirc -listen :8080 -sam-addr 127.0.0.1:7656 -irc-dest irc.example.i2p

FLAGS:
------
  -listen string
        HTTP server listen address (default ":8080")
  -sam-addr string
        SAM bridge address (default "127.0.0.1:7656")
  -irc-dest string
        I2P IRC server destination (default "irc.example.i2p")

USAGE:
------
1. Start the application
2. Open http://localhost:8080 in your browser
3. Enter a nickname and channel name
4. Chat without JavaScript!

Each web user gets their own I2P streaming session with unique keys.
The page auto-refreshes every 5 seconds to show new messages.

COMMANDS:
---------
  /nick NEWNICK    - Change your nickname
  /join #channel   - Join another channel
  /me ACTION       - Send an action message
  /list            - List available channels
  /part            - Leave current channel
*/

package main

import (
	"flag"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dustinfields/i2p-irc/internal/bot"
	"github.com/dustinfields/i2p-irc/internal/irc"
	"github.com/dustinfields/i2p-irc/internal/web"
)

const (
	SessionInactivityTimeout = 30 * time.Minute
	CleanupInterval          = 1 * time.Minute
	MaxConcurrentUsers       = 80
)

var (
	listenAddr         = flag.String("listen", ":8080", "HTTP server listen address")
	samAddr            = flag.String("sam-addr", "127.0.0.1:7656", "SAM bridge address")
	ircDest            = flag.String("irc-dest", "", "I2P IRC server destination (required, for backwards compat)")
	botChannels        = flag.String("bot-channels", "#loadtest,#stormycloud", "Comma-separated list of channels for Postman history bot")
	simpBotChannels    = flag.String("simp-bot-channels", "#simp,#ru,#en,#guessthesong,#gatheryourparty", "Comma-separated list of channels for Simp IRC history bot")
	botLocalAddr       = flag.String("bot-local-addr", "127.0.0.1:6668", "Local TCP address for Postman bot")
	simpBotLocalAddr   = flag.String("simp-bot-local-addr", "127.0.0.1:6667", "Local TCP address for Simp bot")
	botNick            = flag.String("bot-nick", "StormyBot", "Bot nickname")
	postmanNickServPass = flag.String("postman-nickserv-pass", "", "NickServ password for Postman server (optional)")
	simpNickServPass   = flag.String("simp-nickserv-pass", "", "NickServ password for Simp server (optional)")
	debugMode          = flag.Bool("debug", false, "Enable debug endpoints (/status, /debug/*)")
)

func main() {
	flag.Parse()

	// Validate required flags
	if *ircDest == "" {
		log.Fatal("Error: -irc-dest flag is required. Please specify the I2P IRC server destination.")
	}

	log.Printf("I2P WebIRC starting...")
	log.Printf("SAM address: %s", *samAddr)
	log.Printf("IRC destination: %s", *ircDest)
	log.Printf("HTTP listen: %s", *listenAddr)

	// Create session store
	sessions := irc.NewSessionStore()

	// Start session cleanup goroutine
	go sessionCleanup(sessions)

	// Load templates
	templates, err := loadTemplates()
	if err != nil {
		log.Fatalf("Failed to load templates: %v", err)
	}

	// Create history bots for each IRC server
	historyBots := make(map[string]*bot.HistoryBot)

	// Parse and start Postman history bot
	if *botChannels != "" {
		var channels []string
		for _, ch := range strings.Split(*botChannels, ",") {
			ch = strings.TrimSpace(ch)
			if ch != "" {
				channels = append(channels, ch)
			}
		}
		if len(channels) > 0 {
			log.Printf("Starting Postman history bot (local TCP: %s) as %s for channels: %v", *botLocalAddr, *botNick, channels)
			postmanBot := bot.NewHistoryBotWithConfig(bot.BotConfig{
				Nick:         *botNick,
				LocalAddr:    *botLocalAddr,
				Channels:     channels,
				HistorySize:  50,
				NickServPass: *postmanNickServPass,
				UseBot:       true, // +B mode
				ServerName:   "postman",
			})
			if err := postmanBot.Start(); err != nil {
				log.Printf("Warning: Failed to start Postman history bot: %v", err)
			} else {
				log.Printf("Postman history bot started successfully")
				historyBots["postman"] = postmanBot
			}
		}
	}

	// Parse and start Simp history bot
	if *simpBotChannels != "" {
		var channels []string
		for _, ch := range strings.Split(*simpBotChannels, ",") {
			ch = strings.TrimSpace(ch)
			if ch != "" {
				channels = append(channels, ch)
			}
		}
		if len(channels) > 0 {
			log.Printf("Starting Simp history bot (local TCP: %s) as %s for channels: %v", *simpBotLocalAddr, *botNick, channels)
			simpBot := bot.NewHistoryBotWithConfig(bot.BotConfig{
				Nick:         *botNick,
				LocalAddr:    *simpBotLocalAddr,
				Channels:     channels,
				HistorySize:  50,
				NickServPass: *simpNickServPass,
				UseBot:       true, // +B mode
				ServerName:   "simp",
			})
			if err := simpBot.Start(); err != nil {
				log.Printf("Warning: Failed to start Simp history bot: %v", err)
			} else {
				log.Printf("Simp history bot started successfully")
				historyBots["simp"] = simpBot
			}
		}
	}

	if len(historyBots) == 0 {
		log.Printf("No history bots configured")
	}

	// Create handler
	config := web.Config{
		SAMAddress: *samAddr,
		IRCDest:    *ircDest,
		MaxUsers:   MaxConcurrentUsers,
		DebugMode:  *debugMode,
	}
	handler := web.NewHandler(config, sessions, templates, historyBots)

	// Setup routes
	mux := http.NewServeMux()
	mux.HandleFunc("/", handler.IndexHandler)
	mux.HandleFunc("/join", handler.JoinHandler)
	mux.HandleFunc("/connecting", handler.ConnectingHandler)
	mux.HandleFunc("/chan/", func(w http.ResponseWriter, r *http.Request) {
		// Check if this is a messages iframe request
		if strings.HasSuffix(r.URL.Path, "/messages") {
			handler.MessagesHandler(w, r)
		} else {
			handler.ChannelHandler(w, r)
		}
	})
	mux.HandleFunc("/send", handler.SendHandler)
	mux.HandleFunc("/settings", handler.SettingsHandler)
	mux.HandleFunc("/health", handler.HealthHandler)
	mux.HandleFunc("/status", handler.StatusHandler)
	mux.HandleFunc("/debug/history", handler.DebugHistoryHandler)

	// Metrics endpoints
	metrics := handler.GetMetrics()
	mux.HandleFunc("/metrics", metrics.Handler(sessions))
	mux.HandleFunc("/metrics/prometheus", metrics.PrometheusHandler(sessions))

	// Serve static files
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir("static"))))

	// Create rate limiter: 60 requests per minute per session
	rateLimiter := web.NewRateLimiter(60, time.Minute)

	// Wrap with middleware
	var rootHandler http.Handler = mux
	rootHandler = handler.RateLimitMiddleware(rateLimiter)(rootHandler)
	rootHandler = handler.CSRFMiddleware(rootHandler)
	rootHandler = handler.SecurityHeadersMiddleware(rootHandler)

	// Start server
	log.Printf("Server starting on %s", *listenAddr)
	log.Printf("Open http://localhost%s in your browser", *listenAddr)
	if err := http.ListenAndServe(*listenAddr, rootHandler); err != nil {
		log.Fatalf("Server failed: %v", err)
	}
}

// loadTemplates loads all HTML templates
func loadTemplates() (*template.Template, error) {
	templateDir := "templates"

	// Check if templates directory exists
	if _, err := os.Stat(templateDir); os.IsNotExist(err) {
		return nil, err
	}

	// Create template with custom functions
	funcMap := template.FuncMap{
		"urlEscape": url.PathEscape,
	}

	// Parse all templates with custom functions
	pattern := filepath.Join(templateDir, "*.html")
	return template.New("").Funcs(funcMap).ParseGlob(pattern)
}

// sessionCleanup periodically cleans up inactive sessions
func sessionCleanup(sessions *irc.SessionStore) {
	log.Printf("Session cleanup goroutine started (timeout: %v, interval: %v)",
		SessionInactivityTimeout, CleanupInterval)

	ticker := time.NewTicker(CleanupInterval)
	defer ticker.Stop()

	for range ticker.C {
		now := time.Now()
		allSessions := sessions.GetAll()

		if len(allSessions) > 0 {
			log.Printf("Cleanup check: %d sessions to check", len(allSessions))
		}

		cleaned := 0
		for _, session := range allSessions {
			status := session.GetStatus()
			lastHTTP := session.GetLastHTTP()
			idleTime := now.Sub(lastHTTP)

			// Clean up sessions that are:
			// 1. Inactive for too long (normal timeout)
			// 2. In "abandoned" state (reconnect aborted due to inactivity)
			// 3. In "failed" state (reconnect failed after all retries)
			shouldCleanup := false
			reason := ""

			if idleTime > SessionInactivityTimeout {
				shouldCleanup = true
				reason = "inactive"
			} else if status == "abandoned" || status == "failed" {
				shouldCleanup = true
				reason = status
			}

			if shouldCleanup {
				log.Printf("Cleaning up %s session: %s (status: %s, last activity: %v ago)",
					reason, session.ID[:8], status, idleTime.Round(time.Second))

				// Close the session (this also closes the SAM dialer)
				session.Close()

				// Remove from store
				sessions.Delete(session.ID)
				cleaned++
			}
		}

		if cleaned > 0 {
			log.Printf("Cleanup completed: removed %d sessions", cleaned)
		}
	}
}
