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

	"github.com/dustinfields/i2p-irc/internal/irc"
	"github.com/dustinfields/i2p-irc/internal/web"
)

const (
	SessionInactivityTimeout = 30 * time.Minute
	CleanupInterval          = 1 * time.Minute
	MaxConcurrentUsers       = 80
)

var (
	listenAddr = flag.String("listen", ":8080", "HTTP server listen address")
	samAddr    = flag.String("sam-addr", "127.0.0.1:7656", "SAM bridge address")
	ircDest    = flag.String("irc-dest", "irc.example.i2p", "I2P IRC server destination")
)

func main() {
	flag.Parse()

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

	// Create handler
	config := web.Config{
		SAMAddress: *samAddr,
		IRCDest:    *ircDest,
		MaxUsers:   MaxConcurrentUsers,
	}
	handler := web.NewHandler(config, sessions, templates)

	// Setup routes
	http.HandleFunc("/", handler.IndexHandler)
	http.HandleFunc("/join", handler.JoinHandler)
	http.HandleFunc("/connecting", handler.ConnectingHandler)
	http.HandleFunc("/chan/", func(w http.ResponseWriter, r *http.Request) {
		// Check if this is a messages iframe request
		if strings.HasSuffix(r.URL.Path, "/messages") {
			handler.MessagesHandler(w, r)
		} else {
			handler.ChannelHandler(w, r)
		}
	})
	http.HandleFunc("/send", handler.SendHandler)
	http.HandleFunc("/settings", handler.SettingsHandler)
	http.HandleFunc("/status", handler.StatusHandler)

	// Serve static files
	http.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir("static"))))

	// Start server
	log.Printf("Server starting on %s", *listenAddr)
	log.Printf("Open http://localhost%s in your browser", *listenAddr)
	if err := http.ListenAndServe(*listenAddr, nil); err != nil {
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
	ticker := time.NewTicker(CleanupInterval)
	defer ticker.Stop()

	for range ticker.C {
		now := time.Now()
		allSessions := sessions.GetAll()

		for _, session := range allSessions {
			lastHTTP := session.GetLastHTTP()
			if now.Sub(lastHTTP) > SessionInactivityTimeout {
				log.Printf("Cleaning up inactive session: %s (last activity: %v ago)",
					session.ID, now.Sub(lastHTTP))

				// Close the session
				session.Close()

				// Remove from store
				sessions.Delete(session.ID)
			}
		}
	}
}
