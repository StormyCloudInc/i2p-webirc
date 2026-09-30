# I2P WebIRC

A JavaScript-free web IRC client for the I2P network using SAMv3. Features multi-channel support, direct messages, channel history bots, and a retro terminal interface with no client-side JavaScript required.

<div align="center">

### 🌐 **[Try it live at https://irc.i2p.net](https://irc.i2p.net)**

</div>

## Features

- **Zero JavaScript**: Pure HTML/CSS with auto-refresh using iframes and meta tags
- **Multi-Channel Support**: Switch between multiple IRC channels seamlessly
- **Direct Messages**: Private messaging support via `/msg` command
- **Multi-User**: Each web user gets their own I2P streaming session with unique keys
- **I2P Native**: Connects to I2P IRC servers over SAMv3 bridge
- **Auto-Refresh Messages**: Chat updates every 10 seconds without clearing typed text
- **Theme Support**: Switch between dark and light themes
- **Session Management**: Automatic cleanup of inactive sessions (30-minute timeout)
- **Customizable Display**: Hide/show join/part messages
- **Auto-Reconnect**: Handles network interruptions with exponential backoff
- **Dark Terminal Theme**: Retro monospace design with green-on-black styling
- **History Bots**: Optional IRC bots that maintain channel history for new users

## Prerequisites

1. **I2P Router** running with SAM bridge enabled
   - Default SAM address: `127.0.0.1:7656`
   - To enable SAM: Configure your I2P router to enable the SAM application
   - [Download I2P](https://geti2p.net/)

2. **Go** 1.26.8 or higher
   - [Download Go](https://golang.org/dl/)

## Installation

```bash
# Clone the repository
git clone https://github.com/StormyCloudInc/i2p-webirc.git
cd i2p-webirc

# Download dependencies
go mod download

# Build
go build -o webirc ./cmd/webirc
```

## Usage

```bash
# Basic usage (uses defaults)
./webirc

# Custom configuration
./webirc -listen :8080 -sam-addr 127.0.0.1:7656 -irc-dest irc.postman.i2p
```

Then open `http://localhost:8080` in your browser.

### Command-line Flags

| Flag | Description | Default |
|------|-------------|---------|
| `-listen` | HTTP server listen address | `:8080` |
| `-sam-addr` | SAM bridge address | `127.0.0.1:7656` |
| `-irc-dest` | I2P IRC server destination (required) | - |
| `-debug` | Enable debug endpoints (`/status`, `/debug/*`) | `false` |

#### History Bot Flags

| Flag | Description | Default |
|------|-------------|---------|
| `-bot-nick` | Bot nickname | `StormyBot` |
| `-bot-channels` | Comma-separated channels for Postman bot | `#i2p-chat,#i2p,#i2pd,#saltr,#torrents,#freedom,#i2p-news,#ai-chat` |
| `-bot-local-addr` | Local TCP address for Postman bot | `127.0.0.1:6668` |
| `-postman-nickserv-pass` | NickServ password for Postman server | - |
| `-simp-bot-channels` | Comma-separated channels for Simp bot | `#simp,#ru,#en,...` |
| `-simp-bot-local-addr` | Local TCP address for Simp bot | `127.0.0.1:6667` |
| `-simp-nickserv-pass` | NickServ password for Simp server | - |


### IRC Commands

Once connected, you can use these IRC commands:

| Command | Description | Example |
|---------|-------------|---------|
| `/nick NEWNICK` | Change your nickname | `/nick Alice` |
| `/join #channel` | Join another channel | `/join #general` |
| `/msg USER TEXT` | Send a direct message | `/msg Bob Hello!` |
| `/me ACTION` | Send an action message | `/me waves` |
| `/list` | List available channels | `/list` |
| `/part` | Leave the current channel | `/part` |

## User Interface

### Channel Sidebar
- View all joined channels
- Unread message indicators
- DM conversations marked with @ icon
- Click to switch between channels
- Collapsible user list

### Settings
- **Hide join/part messages**: Toggle visibility of user join/leave notifications
- **Theme**: Switch between dark and light themes
- Settings persist via browser cookies

### Auto-Refresh
- Messages refresh automatically every 10 seconds
- Input box never loses typed text (uses iframe architecture)
- Seamless experience without JavaScript

## How It Works

1. **Session Creation**: Each web user is assigned a unique session ID via HTTP-only cookie
2. **I2P Connection**: When joining IRC, a new I2P streaming session is created with generated keys
3. **IRC Protocol**: Session connects to the configured I2P IRC destination using SAM bridge
4. **Message Buffering**: Messages are buffered server-side per channel
5. **Auto-Refresh**: Messages iframe refreshes every 10 seconds while input stays static
6. **Multi-Channel**: Users can join multiple channels and switch between them
7. **Direct Messages**: DMs are treated as special channels named after the recipient
8. **Session Cleanup**: Sessions automatically expire after 30 minutes of inactivity

## History Bots

History bots are optional IRC bots that maintain channel message history. When a new user joins a channel, they can see recent messages that were sent before they connected.

### How It Works

1. The bot connects to IRC servers via local I2P tunnels (or SAM bridge)
2. It joins configured channels and listens for all messages
3. Messages are stored in a ring buffer (default: last 50 messages per channel)
4. When users join the web interface, they receive the buffered history
5. The bot auto-reconnects with exponential backoff if disconnected

### Configuration

History bots use local TCP tunnels to I2P IRC servers. You need to configure I2P tunnels that forward to the IRC servers:

- **Postman IRC**: Create a client tunnel to `irc.postman.i2p` → local port 6668
- **Simp IRC**: Create a client tunnel to `irc.simp.i2p` → local port 6667

Example startup with history bots:

```bash
./webirc \
  -listen :8080 \
  -sam-addr 127.0.0.1:7656 \
  -irc-dest irc.postman.i2p \
  -bot-nick MyBot \
  -bot-channels "#general,#help" \
  -bot-local-addr 127.0.0.1:6668 \
  -postman-nickserv-pass "your-password"
```

### Features

- **NickServ Integration**: Automatically identifies with NickServ if password is provided
- **Bot Mode (+B)**: Sets bot mode to identify as a bot to the network
- **Nick Collision Handling**: Automatically tries alternate nicks if primary is taken
- **Auto-Reconnect**: Reconnects with exponential backoff (5s to 2min) on disconnect
- **Multi-Server Support**: Run separate bots for different IRC networks

### Security Note

Bot passwords are passed via command-line flags (`-postman-nickserv-pass`, `-simp-nickserv-pass`) and are **never stored in the repository**. For production deployments, consider using environment variables or a secrets manager.

## Architecture

```
i2p-irc/
├── cmd/webirc/
│   └── main.go                 - Application entry point, HTTP server setup
├── internal/
│   ├── bot/
│   │   └── history.go          - History bot implementation
│   ├── irc/
│   │   ├── dialer_sam.go       - I2P SAM connection handling
│   │   ├── session.go          - IRC session and channel management
│   │   └── parser.go           - IRC protocol parser
│   └── web/
│       └── handlers.go         - HTTP request handlers
├── templates/
│   ├── index.html              - Landing page / nick selection
│   ├── connecting.html         - Connection status page
│   ├── channel.html            - Main chat interface (iframe container)
│   └── messages.html           - Auto-refreshing messages view
└── static/
    └── style.css               - Styling (dark/light themes)
```

### Key Components

- **SAM Bridge**: I2P's Simple Anonymous Messaging protocol for creating I2P destinations
- **Session Store**: In-memory storage of active IRC sessions with thread-safe access
- **IRC Parser**: Handles PRIVMSG, JOIN, PART, NICK, and other IRC protocol messages
- **Channel Management**: Per-session channel buffers with message history
- **Iframe Architecture**: Separates auto-refreshing content from static input

## Known I2P IRC Servers

Replace `irc.example.i2p` with actual I2P IRC destinations:

- `irc.postman.i2p` - Postman's I2P IRC network
- `irc.echelon.i2p` - Popular I2P IRC network
- `irc.dg.i2p` - DuckGo IRC network
- Or use a `.b32.i2p` address for specific destinations

## Configuration

The application uses sensible defaults but can be customized:

- **Max Concurrent Users**: 80 (defined in `main.go`)
- **Session Timeout**: 30 minutes of inactivity
- **Cleanup Interval**: Checks for inactive sessions every 1 minute
- **Message Refresh**: iframe refreshes every 10 seconds

## Development

### Tech Stack

- **Backend**: Go 1.26.8+ with `net/http` standard library
- **Templating**: `html/template` for server-side rendering
- **I2P Integration**: `github.com/go-i2p/go-sam-go` for SAM bridge connectivity
- **No Frontend Dependencies**: Pure HTML5/CSS3, zero JavaScript

### Project Structure

- `cmd/webirc/main.go` - Entry point, server initialization
- `internal/bot/` - History bot implementation
- `internal/irc/` - I2P and IRC protocol handling
- `internal/web/` - HTTP handlers and web logic
- `templates/` - HTML templates
- `static/` - CSS stylesheets

### Building from Source

```bash
# Install dependencies
go mod download

# Run tests (if available)
go test ./...

# Build for your platform
go build -o webirc ./cmd/webirc

# Build for Linux
GOOS=linux GOARCH=amd64 go build -o webirc-linux ./cmd/webirc

# Build for Windows
GOOS=windows GOARCH=amd64 go build -o webirc.exe ./cmd/webirc
```

## Security Notes

- **Privacy**: Each user gets their own I2P tunnel (destination) with unique keys
- **No Persistence**: No session data is stored on disk
- **Cookie Security**: HTTP-only cookies with SameSite=Lax
- **Auto-Expiry**: Sessions automatically expire after 30 minutes
- **I2P Anonymity**: All IRC traffic routed through I2P's anonymity network
- **No JavaScript**: Eliminates XSS attack surface

## Troubleshooting

### Connection Issues

**Problem**: "Failed to connect to SAM bridge"
- **Solution**: Ensure I2P router is running and SAM bridge is enabled on port 7656

**Problem**: "Failed to connect to IRC server"
- **Solution**: Verify the IRC destination is correct and reachable through I2P

### Performance

**Problem**: Messages are slow to appear
- **Solution**: This is normal for I2P; connections may take time to establish tunnels

**Problem**: Page refreshes are too slow/fast
- **Solution**: Modify the refresh interval in `templates/messages.html` (line 9)

## Contributing

Contributions are welcome! Please ensure:

1. Code follows Go best practices
2. No JavaScript dependencies are introduced
3. Templates remain JavaScript-free
4. Changes maintain I2P anonymity principles

## License

MIT License - See LICENSE file for details

## Acknowledgments

- [I2P Project](https://geti2p.net/) - Anonymous network layer
- [go-sam-go](https://github.com/go-i2p/go-sam-go) - Go library for I2P SAM bridge
- IRC Protocol - RFC 1459 and extensions

## Related Projects

- [irc.postman.i2p](http://irc.postman.i2p/) - Active I2P IRC network
- [I2P Documentation](https://geti2p.net/en/docs) - Learn more about I2P

---

**Note**: This is a web-based IRC client designed for I2P. It requires an I2P router to function. Performance depends on I2P network conditions and tunnel establishment times.

## Reliability and verification

The browser remains JavaScript-free; CSP forbids scripts. Connecting uses a fast POST/303 transition followed by HTML refresh while SAM dialing runs in the background. Invalid supplied nicknames are shown back to the user, not silently replaced. Nonleading nickname digits are supported by IRC; registration failures are displayed and stop refresh. The IRC USER ident is `webirc`, independent of nickname characters. Nickname changes update the display only after a server acknowledgement.

Channel history is the latest ten messages recorded by the configured history bot, imported once in chronological order before live messages, after the user's own JOIN confirmation. History is in memory and is lost on process restart. Bots retry initial tunnel failures; no history is available for channels they do not monitor. The messages iframe refreshes every 12 seconds with a distinct document URL and preserves the outer composer. `/join` moves the entire composer; `/quit` and Disconnect close the session. SAM TCP, HELLO, lookup and stream operations have deadlines; registration times out after two minutes.

Go 1.26.8 is required by the updated `go-i2p/common`, `crypto`, and `logger` dependencies. Production can run a prebuilt static binary without installing Go. There is no Dockerfile or CI configuration in this repository to update.

```sh
go test -race ./... -timeout 45s
go vet ./...
go run golang.org/x/vuln/cmd/govulncheck@latest ./...
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o webirc-linux ./cmd/webirc
```

For repeatable real-browser checks with JavaScript disabled, run the opt-in loopback fixture in one terminal and the browser tests in another. These tests never contact public IRC or I2P. The fixture listens on 127.0.0.1:18181 and exits after three minutes.

```sh
WEBIRC_BROWSER_FIXTURE=1 go test -race ./internal/web -run '^TestBrowserFixture$' -v -timeout 4m
npm install --prefix /tmp/webirc-browser playwright
/tmp/webirc-browser/node_modules/.bin/playwright install chromium
NODE_PATH=/tmp/webirc-browser/node_modules node scripts/browser_test.cjs
```

Before deployment, preserve the whole existing app directory and systemd unit, retain the old binary/templates/static assets together, and record hashes. Keep current SAM/tunnel addresses, monitored channels and NickServ configuration. Stage on a separate loopback port with history bots disabled before replacing the live app. Restarting the app disconnects users and clears in-memory history; routers and nginx do not need a restart. Verify `/health`, public HTTPS headers, first-visit Join, numeric nicknames, and channel readiness. Restore the matching backed-up directory and restart the app service if any required check fails.
