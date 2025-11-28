package irc

import (
	"fmt"
	"net"
	"strings"
	"sync"

	sam3 "github.com/go-i2p/go-sam-go"
	"github.com/go-i2p/go-sam-go/stream"
)

// IRCDialer is an interface for creating IRC connections
type IRCDialer interface {
	Dial() (net.Conn, error)
	Close() error
}

// SamIRCDialer implements IRCDialer using I2P SAMv3
type SamIRCDialer struct {
	SAMAddress string // e.g. "127.0.0.1:7656"
	IRCDest    string // e.g. I2P IRC destination
	SessionID  string // used as tunnel name

	mu     sync.Mutex
	client *sam3.SAM
	stream *stream.StreamSession
}

// Dial creates a new I2P streaming connection to the IRC destination
// On first call, it initializes the SAM client and stream session
func (d *SamIRCDialer) Dial() (net.Conn, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	// Lazy initialization on first dial
	if d.client == nil {
		client, err := sam3.NewSAM(d.SAMAddress)
		if err != nil {
			return nil, fmt.Errorf("failed to connect to SAM bridge: %w", err)
		}
		d.client = client

		keys, err := client.NewKeys()
		if err != nil {
			client.Close()
			d.client = nil
			return nil, fmt.Errorf("failed to generate I2P keys: %w", err)
		}

		tunnelName := "webirc-" + d.SessionID
		// Use nil for default options in go-sam-go
		streamSession, err := client.NewStreamSession(tunnelName, keys, nil)
		if err != nil {
			client.Close()
			d.client = nil
			return nil, fmt.Errorf("failed to create stream session: %w", err)
		}
		d.stream = streamSession
	}

	// Parse destination and port (format: "host" or "host:port")
	dest := d.IRCDest
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
	var conn net.Conn
	var err error
	if port != "" {
		// Dial with port appended
		conn, err = d.stream.Dial(dest + ":" + port)
	} else {
		conn, err = d.stream.Dial(dest)
	}
	if err != nil {
		// Dial failed - reset the SAM session so next attempt creates fresh connection
		d.resetSession()
		return nil, fmt.Errorf("failed to dial IRC destination %s: %w", d.IRCDest, err)
	}

	return conn, nil
}

// resetSession closes and resets the SAM session for a fresh connection attempt
// Must be called with d.mu held
func (d *SamIRCDialer) resetSession() {
	if d.stream != nil {
		d.stream.Close()
		d.stream = nil
	}
	if d.client != nil {
		d.client.Close()
		d.client = nil
	}
}

// Close shuts down the SAM session and client
func (d *SamIRCDialer) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()

	var errs []error

	if d.stream != nil {
		if err := d.stream.Close(); err != nil {
			errs = append(errs, err)
		}
		d.stream = nil
	}

	if d.client != nil {
		if err := d.client.Close(); err != nil {
			errs = append(errs, err)
		}
		d.client = nil
	}

	if len(errs) > 0 {
		return fmt.Errorf("errors closing SAM: %v", errs)
	}

	return nil
}
