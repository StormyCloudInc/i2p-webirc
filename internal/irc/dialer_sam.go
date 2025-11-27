package irc

import (
	"fmt"
	"net"
	"strings"
	"sync"

	sam3 "github.com/eyedeekay/sam3"
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
	stream *sam3.StreamSession
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
		stream, err := client.NewStreamSession(tunnelName, keys, sam3.Options_Default)
		if err != nil {
			client.Close()
			d.client = nil
			return nil, fmt.Errorf("failed to create stream session: %w", err)
		}
		d.stream = stream
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

	// Use Lookup to resolve the I2P destination name/address
	addr, err := d.stream.Lookup(dest)
	if err != nil {
		return nil, fmt.Errorf("failed to lookup I2P destination %s: %w", dest, err)
	}

	// Dial with port if specified
	var conn net.Conn
	if port != "" {
		// addr.Base32() returns just the hash without .b32.i2p suffix
		b32Addr := addr.Base32()
		if !strings.HasSuffix(b32Addr, ".b32.i2p") {
			b32Addr = b32Addr + ".b32.i2p"
		}
		conn, err = d.stream.Dial("tcp", b32Addr+":"+port)
	} else {
		conn, err = d.stream.DialI2P(addr)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to dial IRC destination %s: %w", d.IRCDest, err)
	}

	return conn, nil
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
