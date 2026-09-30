package irc

import (
	"fmt"
	"net"
	"sync"
	"time"

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

	closed bool
	mu     sync.Mutex
	client *sam3.SAM
	stream *stream.StreamSession
}

// Dial creates a new I2P streaming connection to the IRC destination
// On first call, it initializes the SAM client and stream session
func (d *SamIRCDialer) Dial() (net.Conn, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return nil, fmt.Errorf("dialer closed")
	}

	// Lazy initialization on first dial
	if d.client == nil {
		client, err := NewBoundedSAM(d.SAMAddress, 90*time.Second)
		if err != nil {
			return nil, fmt.Errorf("failed to connect to SAM bridge: %w", err)
		}
		d.client = client
		client.Conn.SetDeadline(time.Now().Add(90 * time.Second))

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
		client.Conn.SetDeadline(time.Time{})
	}

	conn, err := DialSAMStream(d.client, d.stream, d.IRCDest, 90*time.Second)
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
	d.closed = true

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
