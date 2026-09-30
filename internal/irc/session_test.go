package irc

import (
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

type delayedDialer struct {
	gate    chan struct{}
	started chan struct{}
	conn    net.Conn
}

func (d *delayedDialer) Dial() (net.Conn, error) {
	close(d.started)
	<-d.gate
	return d.conn, nil
}
func (d *delayedDialer) Close() error { return nil }
func TestCloseDuringDial(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	d := &delayedDialer{make(chan struct{}), make(chan struct{}), a}
	s := NewIRCSession("test", d, "Alice2", "webirc", "Test")
	result := make(chan error, 1)
	go func() { result <- s.Start() }()
	<-d.started
	s.Close()
	close(d.gate)
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("closed session restarted")
		}
	case <-time.After(time.Second):
		t.Fatal("blocked start")
	}
	if s.GetStatus() == "connected" {
		t.Fatal("connected after close")
	}
	if _, err := b.Write([]byte("x")); err == nil {
		t.Fatal("late connection not closed")
	}
}
func TestNickAcknowledgementAndErrors(t *testing.T) {
	s := NewIRCSession("test", nil, "Alice2", "webirc", "Test")
	s.SetCurrentChannel("#test")
	s.handleIRCLine(":server 001 Alice2 :Welcome")
	s.handleIRCLine(":server 433 Alice2 Bob2 :Nickname in use")
	if s.GetNick() != "Alice2" || !s.GetRegistered() {
		t.Fatal("rejection changed nickname or registration")
	}
	s.handleIRCLine(":Alice2!u@h NICK :Bob2")
	if s.GetNick() != "Bob2" {
		t.Fatal("ACK ignored")
	}
	s.Close()
	for _, code := range []string{"432", "433", "464", "465", "ERROR"} {
		s := NewIRCSession("test", nil, "Alice2", "webirc", "Test")
		s.SetCurrentChannel("#test")
		s.handleIRCLine(":server " + code + " * Alice2 :Rejected")
		if s.GetStatus() != "failed" || !s.Closed() {
			t.Fatalf("%s not terminal", code)
		}
	}
}
func TestConfirmedMembershipAndJoinDedup(t *testing.T) {
	s := NewIRCSession("test", nil, "Alice", "webirc", "Test")
	s.setRegistered(true)
	defer s.Close()
	for i := 0; i < 20; i++ {
		s.JoinChannel("#test")
	}
	if len(s.outgoing) != 1 {
		t.Fatal("repeated JOIN")
	}
	c := s.GetChannel("#test")
	s.handleIRCLine(":Other!u@h JOIN #test")
	if c.Joined() {
		t.Fatal("other user grants membership")
	}
	s.handleIRCLine(":Alice!u@h JOIN #test")
	if !c.Joined() {
		t.Fatal("own JOIN missing")
	}
	s.handleIRCLine(":Alice!u@h PART #test :Leaving")
	if s.GetChannel("#test") != nil {
		t.Fatal("own PART retained channel")
	}
}
func TestHistoryAtomicChronologicalAndPrivate(t *testing.T) {
	s := NewIRCSession("test", nil, "Alice", "webirc", "Test")
	defer s.Close()
	c := s.GetOrCreateChannel("#test")
	old := time.Now().Add(-time.Hour)
	h := []ChatMessage{{Time: old, Text: "old"}, {Time: old.Add(time.Second), Text: "newer"}}
	c.AddMessage(ChatMessage{Time: time.Now(), Text: "live"})
	if len(c.ImportHistory(h)) != 1 {
		t.Fatal("history before JOIN")
	}
	c.SetJoined(true)
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); c.ImportHistory(h) }()
	}
	wg.Wait()
	msgs := c.GetMessages()
	if len(msgs) != 3 || msgs[0].Text != "old" || msgs[1].Text != "newer" || msgs[2].Text != "live" {
		t.Fatalf("bad history %#v", msgs)
	}
}
func TestChannelSnapshotDoesNotDeadlock(t *testing.T) {
	s := NewIRCSession("test", nil, "Alice", "webirc", "Test")
	defer s.Close()
	s.GetOrCreateChannel("#test")
	done := make(chan struct{})
	go func() {
		var wg sync.WaitGroup
		for i := 0; i < 4; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := 0; j < 2000; j++ {
					s.SetCurrentChannel("#test")
					s.GetAllChannels()
					s.GetOrCreateChannel(fmt.Sprint("#", j%10))
				}
			}()
		}
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("nested-lock deadlock")
	}
}
func TestProtocolInjectionBlocked(t *testing.T) {
	s := NewIRCSession("test", nil, "Alice", "webirc", "Test")
	defer s.Close()
	for _, bad := range []string{"PRIVMSG #test :ok\r\nJOIN #bad", "PING :\x00", strings.Repeat("a", 511)} {
		s.SendMessage(bad)
		if len(s.outgoing) != 0 {
			t.Fatal("invalid IRC line queued")
		}
		if s.sendRaw(bad) == nil {
			t.Fatal("invalid IRC line accepted")
		}
	}
}
func TestRegistrationTimeout(t *testing.T) {
	s := NewIRCSession("test", nil, "Alice", "webirc", "Test")
	s.SetCurrentChannel("#test")
	s.registrationTimeout(time.Millisecond)
	if s.GetStatus() != "failed" || !s.Closed() {
		t.Fatal("registration timeout not terminal")
	}
}

func TestCleanupCannotDeleteReplacement(t *testing.T) {
	store := NewSessionStore()
	old := NewIRCSession("same", nil, "Old", "webirc", "Test")
	replacement := NewIRCSession("same", nil, "New", "webirc", "Test")
	defer old.Close()
	defer replacement.Close()
	store.Set("same", old)
	store.Set("same", replacement)
	if store.DeleteIfCurrent("same", old) || store.Get("same") != replacement {
		t.Fatal("stale cleanup deleted replacement")
	}
	if !store.DeleteIfCurrent("same", replacement) {
		t.Fatal("current cleanup failed")
	}
}
