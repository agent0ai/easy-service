package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/example/easy-service/internal/supervisor"
)

func cli(args []string, out io.Writer) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: easy-service [status|redeploy|restart|help]")
	}
	action := args[0]
	switch action {
	case "help", "--help", "-h":
		_, err := fmt.Fprintln(out, "easy-service                 Start the supervisor\neasy-service status          Show the running revision and memory usage\neasy-service redeploy        Check Git now and deploy, even if unchanged\neasy-service restart         Restart the current writable installation")
		return err
	case "status", "redeploy", "restart":
	default:
		return fmt.Errorf("unknown command %q; use easy-service help", action)
	}
	data := os.Getenv("DATA_DIR")
	if data == "" {
		data = "/data"
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "unix", supervisor.ControlPath(data))
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 30 * time.Minute}
	method := http.MethodPost
	if action == "status" {
		method = http.MethodGet
		client.Timeout = 5 * time.Second
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, method, "http://local/"+action, nil)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("cannot contact easy-service: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		if err != nil {
			return err
		}
		return fmt.Errorf("%s failed: %s", action, strings.TrimSpace(string(body)))
	}
	var status supervisor.Status
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&status); err != nil {
		return err
	}
	var report []byte
	if action != "status" {
		report = fmt.Appendf(report, "%s completed\n", action)
	}
	report = fmt.Appendf(report, "State: %s\nRevision: %s\nImage: %s\nDigest: %s\nMemory: %.1f MiB\n", status.State, status.Revision, status.RuntimeImage, status.Digest, float64(status.MemoryBytes)/(1<<20))
	if status.MemoryLimit > 0 {
		report = fmt.Appendf(report, "Memory limit: %.1f MiB (soft)\n", float64(status.MemoryLimit)/(1<<20))
	} else {
		report = fmt.Appendf(report, "Memory limit: disabled\n")
	}
	if status.MemoryError != "" {
		report = fmt.Appendf(report, "Memory sample: %s\n", status.MemoryError)
	}
	_, err = out.Write(report)
	return err
}
