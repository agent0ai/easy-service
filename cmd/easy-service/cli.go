package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/example/easy-service/internal/supervisor"
)

func cli(args []string, in io.Reader, out io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: easy-service [status|redeploy|restart|kill-draining|config|help]")
	}
	action := args[0]
	var requestBody io.Reader
	var setValues map[string]string
	path := "/" + action
	configShow, staged := false, false
	if action == "config" {
		if len(args) < 2 {
			return fmt.Errorf("usage: easy-service config [show [NAME...]|set NAME=value...|unset NAME...|apply]")
		}
		switch args[1] {
		case "show":
			configShow = true
			action = "status"
			query := url.Values{}
			for _, name := range args[2:] {
				query.Add("name", name)
			}
			path = "/config?" + query.Encode()
		case "set", "unset":
			staged = true
			action = "config"
			path = "/config"
			items := args[2:]
			fromInput := args[1] == "set" && len(items) == 0
			if fromInput {
				body, err := io.ReadAll(io.LimitReader(in, (1<<20)+1))
				if err != nil {
					return fmt.Errorf("read configuration input: %w", err)
				}
				if len(body) > 1<<20 {
					return fmt.Errorf("configuration input exceeds 1 MiB")
				}
				items = strings.Split(string(body), "\n")
			}
			patch := struct {
				Values map[string]string `json:"values,omitempty"`
				Unset  []string          `json:"unset,omitempty"`
			}{Values: map[string]string{}}
			if args[1] == "unset" {
				if len(items) == 0 {
					return fmt.Errorf("config unset requires variables")
				}
				patch.Unset = items
			} else {
				for _, item := range items {
					if fromInput {
						item = strings.TrimSuffix(item, "\r")
						if strings.TrimSpace(item) == "" {
							continue
						}
					}
					name, value, ok := strings.Cut(item, "=")
					if !ok || name == "" {
						return fmt.Errorf("config set requires NAME=value arguments")
					}
					if fromInput && strings.HasPrefix(value, `"`) {
						var err error
						value, err = strconv.Unquote(value)
						if err != nil {
							return fmt.Errorf("invalid quoted value for %s", name)
						}
					}
					if _, duplicate := patch.Values[name]; duplicate {
						return fmt.Errorf("duplicate variable %q", name)
					}
					patch.Values[name] = value
				}
				if len(patch.Values) == 0 {
					return fmt.Errorf("config set requires NAME=value assignments")
				}
				setValues = patch.Values
			}
			body, err := json.Marshal(patch)
			if err != nil {
				return err
			}
			requestBody = bytes.NewReader(body)
		case "apply":
			if len(args) != 2 {
				return fmt.Errorf("config apply takes no arguments")
			}
			action, path = "apply", "/apply"
		default:
			return fmt.Errorf("unknown config command %q", args[1])
		}
	} else if len(args) != 1 {
		return fmt.Errorf("usage: easy-service [status|redeploy|restart|kill-draining|config|help]")
	}
	switch action {
	case "help", "--help", "-h":
		_, err := fmt.Fprintln(out, "easy-service                 Start the supervisor\neasy-service status          Show the running revision and memory usage\neasy-service redeploy        Check Git now and deploy, even if unchanged\neasy-service restart         Restart the current writable installation\neasy-service kill-draining    Immediately kill all retired instances\neasy-service config show [NAME...]  Show all or selected pending settings\neasy-service config set [NAME=value...]  Stage arguments or read show-format lines from stdin; apply separately\neasy-service config unset NAME...  Remove overrides and use Docker defaults\neasy-service config apply     Validate and activate pending settings")
		return err
	case "status", "redeploy", "restart", "kill-draining", "config", "apply":
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
	request, err := http.NewRequestWithContext(ctx, method, "http://local"+path, requestBody)
	if err != nil {
		return err
	}
	if requestBody != nil {
		request.Header.Set("Content-Type", "application/json")
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
		label := action
		if args[0] == "config" {
			label = "config " + args[1]
		}
		return fmt.Errorf("%s failed: %s", label, strings.TrimSpace(string(body)))
	}
	if action == "kill-draining" {
		var result struct {
			Killed int `json:"killed"`
		}
		if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&result); err != nil {
			return err
		}
		_, err := fmt.Fprintf(out, "Killed %d draining instances\n", result.Killed)
		return err
	}
	if configShow {
		var values map[string]string
		if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&values); err != nil {
			return err
		}
		return printSettings(out, values)
	}
	var status supervisor.Status
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&status); err != nil {
		return err
	}
	if staged {
		if setValues != nil {
			return printSettings(out, setValues)
		}
		_, err := fmt.Fprintln(out, "Settings saved. Run easy-service config apply to activate them.")
		return err
	}
	var report []byte
	if action != "status" {
		report = fmt.Appendf(report, "%s completed\n", action)
	}
	report = fmt.Appendf(report, "State: %s\nRevision: %s\nImage: %s\nDigest: %s\nMemory: %.1f MiB\n", status.State, status.Revision, status.RuntimeImage, status.Digest, float64(status.MemoryBytes)/(1<<20))
	if status.Instance != "" {
		report = fmt.Appendf(report, "Instance: %s\n", status.Instance)
	}
	if status.ConfigPending {
		report = fmt.Appendf(report, "Config: pending changes (run config apply)\n")
	}
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

func printSettings(out io.Writer, values map[string]string) error {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		value := values[name]
		if strings.IndexFunc(value, func(r rune) bool {
			return r == '"' || r == '\\' || unicode.IsSpace(r) || unicode.IsControl(r)
		}) >= 0 {
			value = strconv.Quote(value)
		}
		if _, err := fmt.Fprintf(out, "%s=%s\n", name, value); err != nil {
			return err
		}
	}
	return nil
}
