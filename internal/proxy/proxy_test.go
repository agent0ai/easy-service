package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type benchmarkTransport struct{}

func (benchmarkTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok"))}, nil
}

func BenchmarkProxy(b *testing.B) {
	p := New()
	p.transport = benchmarkTransport{}
	backend, err := NewBackend("http://127.0.0.1:8080")
	if err != nil {
		b.Fatal(err)
	}
	p.Set(backend)
	r := httptest.NewRequest("GET", "/", nil)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p.ServeHTTP(httptest.NewRecorder(), r)
	}
}

func TestDrainAfterEarlierRequestCompleted(t *testing.T) {
	b, err := NewBackend("http://127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	b.enter()
	b.leave()
	b.enter()
	if b.Drain(context.Background(), time.Millisecond) {
		t.Fatal("drain ignored a request after an earlier request completed")
	}
	b.leave()
	if !b.Drain(context.Background(), time.Second) {
		t.Fatal("completed backend did not drain")
	}
}

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
