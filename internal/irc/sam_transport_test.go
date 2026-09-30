package irc

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

func samListener(t *testing.T, handler func(net.Conn)) *net.TCPListener {
	t.Helper()
	addr, _ := net.ResolveTCPAddr("tcp", "127.0.0.1:0")
	l, err := net.ListenTCP("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go handler(c)
		}
	}()
	return l
}
func TestSAMHandshakeBounded(t *testing.T) {
	l := samListener(t, func(c net.Conn) { defer c.Close(); io.Copy(io.Discard, c) })
	start := time.Now()
	if c, err := NewBoundedSAM(l.Addr().String(), 40*time.Millisecond); err == nil {
		c.Close()
		t.Fatal("silent bridge accepted")
	}
	if time.Since(start) > time.Second {
		t.Fatal("HELLO timeout not bounded")
	}
}
func TestSAMStreamPortAndBufferedData(t *testing.T) {
	commands := make(chan string, 1)
	l := samListener(t, func(c net.Conn) {
		defer c.Close()
		r := bufio.NewReader(c)
		r.ReadString('\n')
		fmt.Fprint(c, "HELLO REPLY RESULT=OK VERSION=3.1\n")
		command, _ := r.ReadString('\n')
		commands <- command
		fmt.Fprint(c, "STREAM STATUS RESULT=OK\n:server 001 Alice :Welcome\r\n")
	})
	conn, err := connectSAMStream(l.Addr().String(), "fixture", "PUBLICDEST", "6667", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil || !strings.Contains(line, "001 Alice") {
		t.Fatal("buffered IRC bytes lost", line, err)
	}
	command := <-commands
	if !strings.Contains(command, "TO_PORT=6667") || !strings.Contains(command, "ID=fixture") {
		t.Fatal("bad stream command", command)
	}
}
func TestSAMErrorAndClosedDialer(t *testing.T) {
	if samResultOK("STREAM STATUS RESULT=I2P_ERROR MESSAGE=OK", "STREAM STATUS") {
		t.Fatal("false positive")
	}
	d := &SamIRCDialer{SAMAddress: "127.0.0.1:1"}
	d.Close()
	if _, err := d.Dial(); err == nil || err.Error() != "dialer closed" {
		t.Fatal("closed dialer reopened", err)
	}
}
