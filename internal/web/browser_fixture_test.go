package web

import (
	"bufio"
	"fmt"
	"github.com/dustinfields/i2p-irc/internal/bot"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// Opt-in, loopback-only fixture for scripts/browser_test.cjs. Never dials I2P.
func TestBrowserFixture(t *testing.T) {
	if os.Getenv("WEBIRC_BROWSER_FIXTURE") != "1" {
		t.Skip("opt-in browser fixture")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go serveFixtureIRC(conn)
		}
	}()
	hb := bot.NewHistoryBotLocal("HistoryBot", listener.Addr().String(), []string{"#test", "#i2p-chat"}, 50)
	if err := hb.Start(); err != nil {
		t.Fatal(err)
	}
	defer hb.Stop()
	deadline := time.Now().Add(time.Second)
	for len(hb.GetHistory("#test", 10)) < 10 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	h := testHandler(t, &fixtureDialer{listener.Addr().String()})
	h.bots = map[string]*bot.HistoryBot{"postman": hb}
	mux := http.NewServeMux()
	mux.HandleFunc("/", h.IndexHandler)
	mux.HandleFunc("/join", h.JoinHandler)
	mux.HandleFunc("/connecting", h.ConnectingHandler)
	mux.HandleFunc("/disconnect", h.DisconnectHandler)
	mux.HandleFunc("/send", h.SendHandler)
	mux.HandleFunc("/settings", h.SettingsHandler)
	mux.HandleFunc("/chan/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/messages") {
			h.MessagesHandler(w, r)
		} else {
			h.ChannelHandler(w, r)
		}
	})
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir("../../static"))))
	server := &http.Server{Addr: "127.0.0.1:18181", Handler: h.SecurityHeadersMiddleware(h.CSRFMiddleware(mux))}
	defer server.Close()
	go func() { time.Sleep(3 * time.Minute); server.Close() }()
	fmt.Println("Browser fixture: http://127.0.0.1:18181 (loopback IRC, no public traffic)")
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		t.Fatal(err)
	}
}

type fixtureDialer struct{ addr string }

func (d *fixtureDialer) Dial() (net.Conn, error) {
	time.Sleep(4 * time.Second)
	return net.DialTimeout("tcp", d.addr, time.Second)
}
func (d *fixtureDialer) Close() error { return nil }
func serveFixtureIRC(conn net.Conn) {
	defer conn.Close()
	nick := ""
	registered := false
	scanner := bufio.NewScanner(conn)
	for scanner.Scan() {
		line := scanner.Text()
		parts := strings.SplitN(line, " ", 2)
		arg := ""
		if len(parts) > 1 {
			arg = parts[1]
		}
		switch parts[0] {
		case "NICK":
			if arg == "Taken" {
				fmt.Fprintf(conn, ":fixture 433 %s Taken :Nickname in use\r\n", nick)
				continue
			}
			if registered {
				fmt.Fprintf(conn, ":%s!u@fixture NICK :%s\r\n", nick, arg)
			}
			nick = arg
		case "USER":
			if nick == "Reject" {
				fmt.Fprint(conn, ":fixture 432 * Reject :Erroneous nickname\r\n")
				continue
			}
			registered = true
			fmt.Fprintf(conn, ":fixture 001 %s :Welcome\r\n", nick)
		case "JOIN":
			if arg == "#locked" {
				fmt.Fprintf(conn, ":fixture 473 %s #locked :Invite only\r\n", nick)
				continue
			}
			fmt.Fprintf(conn, ":%s!u@fixture JOIN %s\r\n:fixture 353 %s = %s :%s Other\r\n:fixture 366 %s %s :End\r\n", nick, arg, nick, arg, nick, nick, arg)
			if nick == "HistoryBot" {
				for i := 0; i < 60; i++ {
					fmt.Fprintf(conn, ":Past!u@fixture PRIVMSG %s :history-%02d\r\n", arg, i)
				}
			} else {
				fmt.Fprintf(conn, ":Other!u@fixture PRIVMSG %s :live-message\r\n", arg)
			}
		case "PART":
			fmt.Fprintf(conn, ":%s!u@fixture PART %s :Leaving\r\n", nick, arg)
		case "QUIT":
			return
		case "PRIVMSG":
			target, text, _ := strings.Cut(arg, " :")
			if !strings.HasPrefix(target, "#") {
				fmt.Fprintf(conn, ":%s!u@fixture PRIVMSG %s :reply to %s\r\n", target, nick, text)
			}
		}
	}
}
