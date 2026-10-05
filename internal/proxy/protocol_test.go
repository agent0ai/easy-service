package proxy

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/textproto"
	"strings"
	"testing"
	"time"
)

func proxyServer(t *testing.T, handler http.HandlerFunc) (*Proxy, *Backend, *httptest.Server) {
	t.Helper()
	upstream := httptest.NewServer(handler)
	t.Cleanup(upstream.Close)
	p := New()
	t.Cleanup(p.transport.(*http.Transport).CloseIdleConnections)
	b, err := NewBackend(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	p.Set(b)
	front := httptest.NewServer(p)
	front.Client().Timeout = 2 * time.Second
	t.Cleanup(front.Close)
	return p, b, front
}

func TestSSEFlushCutoverAndDisconnect(t *testing.T) {
	const event = "id: 7\nevent: update\ndata: first\ndata: second\n\n"
	disconnected := make(chan struct{})
	p, old, front := proxyServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		// A known length must not suppress SSE flushing.
		w.Header().Set("Content-Length", fmt.Sprint(2*len(event)))
		fmt.Fprint(w, event)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(disconnected)
	})
	res, err := front.Client().Get(front.URL)
	if err != nil {
		t.Fatal("SSE headers were not delivered before completion:", err)
	}
	defer res.Body.Close()
	b := make([]byte, len(event))
	if _, err := io.ReadFull(res.Body, b); err != nil || string(b) != event {
		t.Fatalf("SSE event: %q %v", b, err)
	}
	next := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "new") }))
	defer next.Close()
	nextBackend, _ := NewBackend(next.URL)
	p.Set(nextBackend)
	if old.Drain(context.Background(), 10*time.Millisecond) {
		t.Fatal("cutover lost the open SSE response")
	}
	newRes, err := front.Client().Get(front.URL)
	if err != nil {
		t.Fatal(err)
	}
	newBody, err := io.ReadAll(newRes.Body)
	_ = newRes.Body.Close()
	if err != nil || string(newBody) != "new" {
		t.Fatalf("new requests did not cut over: %q %v", newBody, err)
	}
	_ = res.Body.Close()
	select {
	case <-disconnected:
	case <-time.After(time.Second):
		t.Fatal("client disconnect did not cancel the upstream SSE request")
	}
	if !old.Drain(context.Background(), time.Second) {
		t.Fatal("disconnected SSE response did not leave the drain count")
	}
}

func TestStreamingRequestAndResponse(t *testing.T) {
	_, b, front := proxyServer(t, func(w http.ResponseWriter, r *http.Request) {
		if err := http.NewResponseController(w).EnableFullDuplex(); err != nil {
			t.Error(err)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Trailer", "X-Checksum")
		reader := bufio.NewReader(r.Body)
		for {
			chunk, err := reader.ReadString('\n')
			if chunk != "" {
				fmt.Fprint(w, chunk)
				w.(http.Flusher).Flush()
			}
			if err != nil {
				if err == io.EOF {
					w.Header().Set("X-Checksum", "complete")
				}
				return
			}
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	reader, writer := io.Pipe()
	defer writer.Close()
	req, _ := http.NewRequestWithContext(ctx, "POST", front.URL, reader)
	responses := make(chan *http.Response, 1)
	errors := make(chan error, 1)
	go func() {
		res, err := front.Client().Do(req)
		if err != nil {
			errors <- err
			return
		}
		responses <- res
	}()
	if _, err := io.WriteString(writer, "one\n"); err != nil {
		t.Fatal(err)
	}
	var res *http.Response
	select {
	case res = <-responses:
	case err := <-errors:
		t.Fatal("response blocked on completion of the streaming request:", err)
	case <-ctx.Done():
		t.Fatal("response blocked on completion of the streaming request")
	}
	defer res.Body.Close()
	body := bufio.NewReader(res.Body)
	for _, want := range []string{"one\n", "two\n"} {
		if want == "two\n" {
			if _, err := io.WriteString(writer, want); err != nil {
				t.Fatal(err)
			}
		}
		if got, err := body.ReadString('\n'); err != nil || got != want {
			t.Fatalf("streaming echo: %q %v", got, err)
		}
	}
	_ = writer.Close()
	if _, err := io.Copy(io.Discard, body); err != nil {
		t.Fatal(err)
	}
	if res.Trailer.Get("X-Checksum") != "complete" {
		t.Fatal("response trailer lost:", res.Trailer)
	}
	if !b.Drain(ctx, time.Second) {
		t.Fatal("completed stream did not drain")
	}
}

func TestKnownLengthStreamingFlush(t *testing.T) {
	_, _, front := proxyServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "8")
		fmt.Fprint(w, "one\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", front.URL, nil)
	res, err := front.Client().Do(req)
	if err != nil {
		t.Fatal("flushed response with a known length was buffered:", err)
	}
	defer res.Body.Close()
	if got, err := bufio.NewReader(res.Body).ReadString('\n'); err != nil || got != "one\n" {
		t.Fatalf("first chunk: %q %v", got, err)
	}
}

func TestResponseSemantics(t *testing.T) {
	var compressed bytes.Buffer
	gz := gzip.NewWriter(&compressed)
	_, _ = gz.Write([]byte("compressed payload"))
	_ = gz.Close()
	binary := bytes.Repeat([]byte{0, 255, 128, 10}, 1<<15)
	_, _, front := proxyServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Set-Cookie", "a=1; Path=/")
		w.Header().Add("Set-Cookie", "b=2; Path=/")
		switch r.URL.Path {
		case "/binary":
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(binary)
		case "/gzip":
			w.Header().Set("Content-Encoding", "gzip")
			_, _ = w.Write(compressed.Bytes())
		case "/head":
			w.Header().Set("Content-Length", "123")
		case "/empty":
			w.WriteHeader(http.StatusNoContent)
		case "/error":
			w.WriteHeader(http.StatusUnprocessableEntity)
			fmt.Fprint(w, "application error")
		default:
			fmt.Fprint(w, r.RequestURI)
		}
	})
	for _, tc := range []struct {
		method, path string
		status       int
		body         []byte
	}{
		{"GET", "/binary", 200, binary},
		{"GET", "/gzip", 200, compressed.Bytes()},
		{"HEAD", "/head", 200, nil},
		{"GET", "/empty", 204, nil},
		{"GET", "/error", 422, []byte("application error")},
		{"GET", "/a%2Fb?x=%2B&x=two", 200, []byte("/a%2Fb?x=%2B&x=two")},
	} {
		t.Run(tc.path, func(t *testing.T) {
			req, _ := http.NewRequest(tc.method, front.URL+tc.path, nil)
			req.Header.Set("Accept-Encoding", "gzip")
			res, err := front.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			body, err := io.ReadAll(res.Body)
			if err != nil || res.StatusCode != tc.status || !bytes.Equal(body, tc.body) {
				t.Fatalf("response: status=%d bytes=%d err=%v", res.StatusCode, len(body), err)
			}
			if len(res.Header.Values("Set-Cookie")) != 2 {
				t.Fatal("multiple response headers lost")
			}
			if tc.path == "/gzip" && res.Header.Get("Content-Encoding") != "gzip" {
				t.Fatal("compression metadata lost")
			}
			if tc.method == "HEAD" && res.ContentLength != 123 {
				t.Fatal("HEAD content length lost")
			}
		})
	}
}

func TestContinueEarlyHintsAndRequestTrailers(t *testing.T) {
	const payload = "streamed upload"
	_, _, front := proxyServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Link", "</style.css>; rel=preload")
		w.WriteHeader(http.StatusEarlyHints)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		fmt.Fprintf(w, "%s:%s", body, r.Trailer.Get("X-Upload-Checksum"))
	})
	hints := make(chan int, 4)
	trace := &httptrace.ClientTrace{Got1xxResponse: func(code int, header textproto.MIMEHeader) error {
		if code == http.StatusEarlyHints && header.Get("Link") != "</style.css>; rel=preload" {
			t.Error("early hint header lost")
		}
		hints <- code
		return nil
	}}
	req, _ := http.NewRequestWithContext(httptrace.WithClientTrace(context.Background(), trace), "POST", front.URL, strings.NewReader(payload))
	req.Header.Set("Expect", "100-continue")
	req.ContentLength = -1
	req.Trailer = http.Header{"X-Upload-Checksum": {"complete"}}
	res, err := front.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil || string(body) != payload+":complete" {
		t.Fatalf("request body/trailer: %q %v", body, err)
	}
	seen := map[int]bool{}
	for len(hints) > 0 {
		seen[<-hints] = true
	}
	if !seen[100] || !seen[103] {
		t.Fatal("informational responses lost:", seen)
	}
}

func TestInterruptedResponseDoesNotCrashOrLeak(t *testing.T) {
	_, b, front := proxyServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthy" {
			fmt.Fprint(w, "ok")
			return
		}
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		_, _ = rw.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 10\r\n\r\nabc")
		_ = rw.Flush()
	})
	res, err := front.Client().Get(front.URL)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if err == nil || string(body) != "abc" {
		t.Fatalf("truncated response reported success: %q %v", body, err)
	}
	if !b.Drain(context.Background(), time.Second) {
		t.Fatal("failed response retained an in-flight request")
	}
	res, err = front.Client().Get(front.URL + "/healthy")
	if err != nil {
		t.Fatal("proxy did not survive interrupted response:", err)
	}
	defer res.Body.Close()
	body, err = io.ReadAll(res.Body)
	if err != nil || string(body) != "ok" {
		t.Fatalf("later response: %q %v", body, err)
	}
}

func TestWebSocketFramesCutoverAndClose(t *testing.T) {
	// These are valid RFC 6455 frames: masked client text, binary, ping,
	// fragmented text and close; server replies are unmasked.
	frames := [][]byte{
		{0x81, 5, 'h', 'e', 'l', 'l', 'o'},
		{0x82, 4, 0, 255, 128, 10},
		{0x89, 1, 'p'},
		{0x01, 2, 'a', 'b'},
		{0x80, 2, 'c', 'd'},
		{0x88, 2, 3, 232}, // normal closure, code 1000
	}
	closed := make(chan struct{})
	p, old, front := proxyServer(t, func(w http.ResponseWriter, r *http.Request) {
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer close(closed)
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
		accept := sha1.Sum([]byte(r.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
		fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Accept: %s\r\n\r\n", base64.StdEncoding.EncodeToString(accept[:]))
		_ = rw.Flush()
		for _, frame := range frames {
			got := make([]byte, len(frame)+4)
			if _, err := io.ReadFull(rw, got); err != nil {
				t.Error(err)
				return
			}
			if !bytes.Equal(got, maskedFrame(frame)) {
				t.Errorf("WebSocket client frame changed: %x", got)
				return
			}
			reply := append([]byte(nil), frame...)
			if reply[0] == 0x89 {
				reply[0] = 0x8a // pong
			}
			_, _ = rw.Write(reply)
			_ = rw.Flush()
		}
	})
	conn, err := net.DialTimeout("tcp", strings.TrimPrefix(front.URL, "http://"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	const key = "dGhlIHNhbXBsZSBub25jZQ=="
	fmt.Fprintf(conn, "GET /socket HTTP/1.1\r\nHost: example.test\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: %s\r\n\r\n", key)
	reader := bufio.NewReader(conn)
	res, err := http.ReadResponse(reader, &http.Request{Method: "GET"})
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != 101 || res.Header.Get("Sec-WebSocket-Accept") != "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=" {
		t.Fatal("invalid WebSocket handshake:", res.Status, res.Header)
	}
	next := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "new") }))
	defer next.Close()
	nextBackend, _ := NewBackend(next.URL)
	p.Set(nextBackend)
	if old.Drain(context.Background(), 10*time.Millisecond) {
		t.Fatal("open WebSocket was not tracked during drain")
	}
	for _, frame := range frames {
		if _, err := conn.Write(maskedFrame(frame)); err != nil {
			t.Fatal(err)
		}
		want := append([]byte(nil), frame...)
		if want[0] == 0x89 {
			want[0] = 0x8a
		}
		got := make([]byte, len(want))
		if _, err := io.ReadFull(reader, got); err != nil || !bytes.Equal(got, want) {
			t.Fatalf("WebSocket server frame changed: %x want %x err=%v", got, want, err)
		}
	}
	_ = conn.Close()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("WebSocket close did not reach the backend")
	}
	if !old.Drain(context.Background(), time.Second) {
		t.Fatal("closed WebSocket did not drain")
	}
}

func maskedFrame(frame []byte) []byte {
	mask := []byte{1, 2, 3, 4}
	b := append([]byte{frame[0], frame[1] | 0x80}, mask...)
	for n, v := range frame[2:] {
		b = append(b, v^mask[n%4])
	}
	return b
}
