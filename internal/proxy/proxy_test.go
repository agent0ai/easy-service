package proxy

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestUnavailableAndAtomicPerRequestCutover(t *testing.T) {
	p := New()
	r := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	p.ServeHTTP(w, r)
	if w.Code != 503 {
		t.Fatal(w.Code)
	}
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "a") }))
	defer a.Close()
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "b") }))
	defer b.Close()
	ba, _ := NewBackend(a.URL)
	bb, _ := NewBackend(b.URL)
	p.Set(ba)
	w = httptest.NewRecorder()
	p.ServeHTTP(w, r)
	if w.Body.String() != "a" {
		t.Fatal()
	}
	if old := p.Set(bb); old != ba {
		t.Fatal()
	}
	w = httptest.NewRecorder()
	p.ServeHTTP(w, r)
	if w.Body.String() != "b" {
		t.Fatal()
	}
}
func TestStreamingAndDrain(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f := w.(http.Flusher)
		fmt.Fprint(w, "one\n")
		f.Flush()
		close(started)
		<-release
		fmt.Fprint(w, "two\n")
	}))
	defer s.Close()
	p := New()
	b, _ := NewBackend(s.URL)
	p.Set(b)
	go http.Get(server(p))
	<-started
	if b.Drain(nilContext{}, 10*time.Millisecond) {
		t.Fatal("drained in flight")
	}
	close(release)
	time.Sleep(20 * time.Millisecond)
	if !b.Drain(nilContext{}, time.Second) {
		t.Fatal("did not drain")
	}
}
func TestWebSocketUpgrade(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.(http.Hijacker)
		c, rw, _ := h.Hijack()
		defer c.Close()
		rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: test\r\n\r\npong")
		rw.Flush()
	}))
	defer s.Close()
	p := New()
	b, _ := NewBackend(s.URL)
	p.Set(b)
	u := server(p)
	c, _ := net.Dial("tcp", strings.TrimPrefix(u, "http://"))
	defer c.Close()
	fmt.Fprintf(c, "GET / HTTP/1.1\r\nHost: x\r\nConnection: Upgrade\r\nUpgrade: test\r\n\r\n")
	line, _ := bufio.NewReader(c).ReadString('\n')
	if !strings.Contains(line, "101") {
		t.Fatal(line)
	}
}
func server(h http.Handler) string { s := httptest.NewServer(h); return s.URL }

type nilContext struct{}

func (nilContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (nilContext) Done() <-chan struct{}       { return nil }
func (nilContext) Err() error                  { return nil }
func (nilContext) Value(any) any               { return nil }
