package web

import (
	"bufio"
	"fmt"
	"github.com/dustinfields/i2p-irc/internal/irc"
	"html/template"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type gateDialer struct {
	gate  chan struct{}
	fail  bool
	calls atomic.Int32
}

func (d *gateDialer) Dial() (net.Conn, error) {
	d.calls.Add(1)
	<-d.gate
	if d.fail {
		return nil, fmt.Errorf("fixture failure")
	}
	a, b := net.Pipe()
	go func() {
		defer b.Close()
		scanner := bufio.NewScanner(b)
		nick := "Alice2"
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "NICK ") {
				nick = strings.TrimPrefix(line, "NICK ")
			}
			if strings.HasPrefix(line, "USER ") {
				fmt.Fprintf(b, ":server 001 %s :Welcome\r\n", nick)
			}
			if strings.HasPrefix(line, "JOIN ") {
				ch := strings.TrimPrefix(line, "JOIN ")
				fmt.Fprintf(b, ":%s!u@h JOIN %s\r\n", nick, ch)
			}
		}
	}()
	return a, nil
}
func (d *gateDialer) Close() error { return nil }
func testHandler(t *testing.T, d irc.IRCDialer) *Handler {
	t.Helper()
	tmpl := template.Must(template.New("").Funcs(template.FuncMap{"urlEscape": url.PathEscape}).ParseGlob("../../templates/*.html"))
	h := NewHandlerWithDialer(Config{MaxUsers: 2}, irc.NewSessionStore(), tmpl, nil, func(_, _, _ string) irc.IRCDialer { return d })
	t.Cleanup(func() {
		h.metrics.Stop()
		for _, s := range h.sessions.GetAll() {
			s.Close()
		}
	})
	return h
}
func postJoin(h *Handler, id, nick string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/join", strings.NewReader(url.Values{"nick": {nick}, "channel": {"#test"}, "server": {"postman"}}.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(&http.Cookie{Name: SessionCookieName, Value: id})
	w := httptest.NewRecorder()
	h.JoinHandler(w, r)
	return w
}
func TestImmediateJoinAndDuplicateAdmission(t *testing.T) {
	d := &gateDialer{gate: make(chan struct{})}
	defer close(d.gate)
	h := testHandler(t, d)
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			start := time.Now()
			w := postJoin(h, strings.Repeat("a", 64), "Alice2")
			if w.Code != 303 || !strings.HasPrefix(w.Header().Get("Location"), "/connecting?") {
				t.Errorf("join %d %s", w.Code, w.Body)
			}
			if time.Since(start) > time.Second {
				t.Error("Join waited for dial")
			}
		}()
	}
	wg.Wait()
	if h.sessions.Count() != 1 {
		t.Fatal("duplicate sessions")
	}
	deadline := time.Now().Add(time.Second)
	for d.calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if d.calls.Load() != 1 {
		t.Fatal("duplicate dials")
	}
}
func TestFreshCSRFFormAndValidation(t *testing.T) {
	h := testHandler(t, &gateDialer{gate: make(chan struct{})})
	w := httptest.NewRecorder()
	h.CSRFMiddleware(http.HandlerFunc(h.IndexHandler)).ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	var token, id string
	for _, c := range w.Result().Cookies() {
		if c.Name == "csrf_token" {
			token = c.Value
		}
		if c.Name == SessionCookieName {
			id = c.Value
		}
	}
	if token == "" || !strings.Contains(w.Body.String(), `value="`+token+`"`) {
		t.Fatal("first-page CSRF token missing")
	}
	r := httptest.NewRequest("POST", "/join", strings.NewReader(url.Values{"nick": {"9Alice"}, "csrf_token": {token}}.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(&http.Cookie{Name: SessionCookieName, Value: id})
	w = httptest.NewRecorder()
	h.CSRFMiddleware(http.HandlerFunc(h.JoinHandler)).ServeHTTP(w, r)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "9Alice") {
		t.Fatal("invalid nick not preserved", w.Code)
	}
	if !isValidNick("Alice123") {
		t.Fatal("embedded digits must remain valid")
	}
}
func TestPathAndSendInjection(t *testing.T) {
	h := testHandler(t, nil)
	id := strings.Repeat("b", 64)
	s := irc.NewIRCSession(id, nil, "Alice", "webirc", "Test")
	h.sessions.Set(id, s)
	for _, path := range []string{"/chan/%23test%0d%0aJOIN%20%23bad", "/connecting?channel=%23test%0d%0aJOIN%20%23bad", "/chan/%2523test"} {
		r := httptest.NewRequest("GET", path, nil)
		r.AddCookie(&http.Cookie{Name: SessionCookieName, Value: id})
		w := httptest.NewRecorder()
		if strings.HasPrefix(path, "/connecting") {
			h.ConnectingHandler(w, r)
		} else {
			h.ChannelHandler(w, r)
		}
		if w.Code != 400 {
			t.Errorf("%s accepted %d", path, w.Code)
		}
	}
}
func TestFailedSessionCanRetry(t *testing.T) {
	d := &gateDialer{gate: make(chan struct{}), fail: true}
	close(d.gate)
	h := testHandler(t, d)
	id := strings.Repeat("c", 64)
	postJoin(h, id, "Alice2")
	first := h.sessions.Get(id)
	deadline := time.Now().Add(time.Second)
	for !first.Closed() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !first.Closed() {
		t.Fatal("failure not closed")
	}
	postJoin(h, id, "Alice2")
	if h.sessions.Get(id) == first {
		t.Fatal("failed session reused")
	}
}
