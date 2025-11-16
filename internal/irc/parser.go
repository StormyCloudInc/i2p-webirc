package irc

import (
	"fmt"
	"log"
	"strings"
	"time"
)

// IRCMessage represents a parsed IRC protocol message
type IRCMessage struct {
	Raw     string
	Prefix  string
	Command string
	Params  []string
}

// parseIRCLine parses a raw IRC line into an IRCMessage
func parseIRCLine(line string) *IRCMessage {
	msg := &IRCMessage{Raw: line}

	// Parse prefix (optional)
	if strings.HasPrefix(line, ":") {
		parts := strings.SplitN(line[1:], " ", 2)
		msg.Prefix = parts[0]
		if len(parts) > 1 {
			line = parts[1]
		} else {
			return msg
		}
	}

	// Parse command and params
	if idx := strings.Index(line, " :"); idx != -1 {
		// Trailing parameter
		parts := strings.Fields(line[:idx])
		if len(parts) > 0 {
			msg.Command = strings.ToUpper(parts[0])
			msg.Params = append(parts[1:], line[idx+2:])
		}
	} else {
		// No trailing parameter
		parts := strings.Fields(line)
		if len(parts) > 0 {
			msg.Command = strings.ToUpper(parts[0])
			msg.Params = parts[1:]
		}
	}

	return msg
}

// extractNick extracts the nickname from a prefix like "nick!user@host"
func extractNick(prefix string) string {
	if idx := strings.Index(prefix, "!"); idx != -1 {
		return prefix[:idx]
	}
	return prefix
}

// handleIRCLine processes a single IRC protocol line
func (s *IRCSession) handleIRCLine(line string) {
	msg := parseIRCLine(line)

	switch msg.Command {
	case "PING":
		// Respond to PING
		if len(msg.Params) > 0 {
			s.SendMessage(fmt.Sprintf("PONG :%s", msg.Params[0]))
		}

	case "PRIVMSG":
		// Private message or channel message
		if len(msg.Params) >= 2 {
			target := msg.Params[0]
			text := msg.Params[1]
			nick := extractNick(msg.Prefix)

			kind := "privmsg"
			// Check for CTCP ACTION (/me)
			if strings.HasPrefix(text, "\x01ACTION ") && strings.HasSuffix(text, "\x01") {
				text = strings.TrimPrefix(text, "\x01ACTION ")
				text = strings.TrimSuffix(text, "\x01")
				kind = "action"
			}

			// For DMs (target is our nick), store under sender's nick instead
			channelName := target
			if target == s.GetNick() {
				channelName = nick
			}

			ch := s.GetOrCreateChannel(channelName)
			ch.AddMessage(ChatMessage{
				Time:   time.Now(),
				Prefix: nick,
				Text:   text,
				Kind:   kind,
			})
		}

	case "NOTICE":
		// Notice message
		if len(msg.Params) >= 2 {
			target := msg.Params[0]
			text := msg.Params[1]
			nick := extractNick(msg.Prefix)

			// For DMs (target is our nick), store under sender's nick instead
			channelName := target
			if target == s.GetNick() {
				channelName = nick
			}

			ch := s.GetOrCreateChannel(channelName)
			ch.AddMessage(ChatMessage{
				Time:   time.Now(),
				Prefix: nick,
				Text:   text,
				Kind:   "notice",
			})
		}

	case "JOIN":
		// User joined channel
		if len(msg.Params) >= 1 {
			channel := msg.Params[0]
			nick := extractNick(msg.Prefix)

			ch := s.GetOrCreateChannel(channel)
			ch.AddUser(nick)

			// Only show join messages for other users
			if nick != s.GetNick() {
				ch.AddMessage(ChatMessage{
					Time:   time.Now(),
					Prefix: nick,
					Text:   "joined",
					Kind:   "join",
				})
			}
		}

	case "PART":
		// User left channel
		if len(msg.Params) >= 1 {
			channel := msg.Params[0]
			reason := ""
			if len(msg.Params) >= 2 {
				reason = msg.Params[1]
			}
			nick := extractNick(msg.Prefix)

			ch := s.GetChannel(channel)
			if ch != nil {
				ch.RemoveUser(nick)
				text := "left"
				if reason != "" {
					text = fmt.Sprintf("left (%s)", reason)
				}
				ch.AddMessage(ChatMessage{
					Time:   time.Now(),
					Prefix: nick,
					Text:   text,
					Kind:   "part",
				})
			}
		}

	case "QUIT":
		// User quit IRC
		reason := ""
		if len(msg.Params) >= 1 {
			reason = msg.Params[0]
		}
		nick := extractNick(msg.Prefix)

		// Remove from all channels
		s.mu.RLock()
		channels := make([]*ChannelState, 0, len(s.channels))
		for _, ch := range s.channels {
			channels = append(channels, ch)
		}
		s.mu.RUnlock()

		text := "quit"
		if reason != "" {
			text = fmt.Sprintf("quit (%s)", reason)
		}

		for _, ch := range channels {
			if ch.users[nick] {
				ch.RemoveUser(nick)
				ch.AddMessage(ChatMessage{
					Time:   time.Now(),
					Prefix: nick,
					Text:   text,
					Kind:   "quit",
				})
			}
		}

	case "NICK":
		// User changed nickname
		if len(msg.Params) >= 1 {
			oldNick := extractNick(msg.Prefix)
			newNick := msg.Params[0]

			// Update own nick if it's us
			if oldNick == s.GetNick() {
				s.SetNick(newNick)
			}

			// Update in all channels
			s.mu.RLock()
			channels := make([]*ChannelState, 0, len(s.channels))
			for _, ch := range s.channels {
				channels = append(channels, ch)
			}
			s.mu.RUnlock()

			for _, ch := range channels {
				if ch.users[oldNick] {
					ch.RenameUser(oldNick, newNick)
					ch.AddMessage(ChatMessage{
						Time:   time.Now(),
						Prefix: oldNick,
						Text:   fmt.Sprintf("is now known as %s", newNick),
						Kind:   "system",
					})
				}
			}
		}

	case "353": // RPL_NAMREPLY - names list
		// :server 353 nick = #channel :nick1 nick2 nick3
		if len(msg.Params) >= 4 {
			channel := msg.Params[2]
			names := strings.Fields(msg.Params[3])

			ch := s.GetOrCreateChannel(channel)
			for _, name := range names {
				// Strip channel modes (@, +, etc.)
				name = strings.TrimLeft(name, "@+")
				ch.AddUser(name)
			}
		}

	case "366": // RPL_ENDOFNAMES
		// End of NAMES list - we could add a system message here if desired
		if len(msg.Params) >= 2 {
			channel := msg.Params[1]
			ch := s.GetChannel(channel)
			if ch != nil {
				users := ch.GetUsers()
				ch.AddMessage(ChatMessage{
					Time:   time.Now(),
					Prefix: "system",
					Text:   fmt.Sprintf("Joined %s (%d users)", channel, len(users)),
					Kind:   "system",
				})
			}
		}

	case "322": // RPL_LIST - channel list entry
		// :server 322 nick #channel user_count :topic
		if len(msg.Params) >= 3 {
			channel := msg.Params[1]
			userCount := msg.Params[2]
			topic := ""
			if len(msg.Params) >= 4 {
				topic = msg.Params[3]
			}

			// Add to current channel as a system message
			currentChan := s.GetCurrentChannel()
			if currentChan != "" {
				ch := s.GetChannel(currentChan)
				if ch != nil {
					text := fmt.Sprintf("%s [%s users]", channel, userCount)
					if topic != "" {
						text = fmt.Sprintf("%s: %s", text, topic)
					}
					ch.AddMessage(ChatMessage{
						Time:   time.Now(),
						Prefix: "server",
						Text:   text,
						Kind:   "system",
					})
				}
			}
		}

	case "323": // RPL_LISTEND
		// End of LIST
		currentChan := s.GetCurrentChannel()
		if currentChan != "" {
			ch := s.GetChannel(currentChan)
			if ch != nil {
				ch.AddMessage(ChatMessage{
					Time:   time.Now(),
					Prefix: "server",
					Text:   "End of channel list",
					Kind:   "system",
				})
			}
		}

	case "001": // RPL_WELCOME - IRC registration complete
		// Mark as registered when we receive the first welcome message
		s.setRegistered(true)
		log.Printf("Session %s: IRC registration complete", s.ID)

	case "002", "003", "004", "005": // Other welcome messages
		// Server welcome - could log or display
		log.Printf("Session %s: %s", s.ID, msg.Raw)

	case "372", "375", "376": // MOTD
		// Message of the day - could display in a system channel
		log.Printf("Session %s: %s", s.ID, msg.Raw)

	default:
		// Log unknown commands for debugging
		if msg.Command != "" {
			log.Printf("Session %s: Unhandled IRC command %s: %s", s.ID, msg.Command, msg.Raw)
		}
	}
}
