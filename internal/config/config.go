package config

import (
	"fmt"
	"math"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	GitURL, GitToken, GitBranch, UpdateMethod, UpdatePattern    string
	RuntimeImage, SetupCommand, RunCommand, HealthPath, DataDir string
	PollInterval, StartupTimeout, HealthInterval                time.Duration
	ServicePort, HealthFailures                                 int
	ServiceMemoryLimit                                          uint64
	AppEnv                                                      []string
}

func Load() (Config, error) {
	c := Config{GitURL: os.Getenv("GIT_URL"), GitToken: os.Getenv("GIT_TOKEN"), GitBranch: val("GIT_BRANCH", "main"), UpdateMethod: val("UPDATE_METHOD", "commit"), UpdatePattern: val("UPDATE_PATTERN", "*"), RuntimeImage: os.Getenv("RUNTIME_IMAGE"), SetupCommand: os.Getenv("SETUP_COMMAND"), RunCommand: os.Getenv("RUN_COMMAND"), HealthPath: val("HEALTH_PATH", "/"), DataDir: val("DATA_DIR", "/data")}
	var err error
	if c.PollInterval, err = duration("POLL_INTERVAL", 300); err != nil {
		return c, err
	}
	if c.StartupTimeout, err = duration("STARTUP_TIMEOUT", 60); err != nil {
		return c, err
	}
	if c.HealthInterval, err = duration("HEALTH_INTERVAL", 10); err != nil {
		return c, err
	}
	if c.ServicePort, err = integer("SERVICE_PORT", 80); err != nil {
		return c, err
	}
	if c.HealthFailures, err = integer("HEALTH_FAILURES", 3); err != nil {
		return c, err
	}
	if c.ServiceMemoryLimit, err = memoryLimit(); err != nil {
		return c, err
	}
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "APP_") {
			if strings.HasPrefix(e, "APP_=") {
				return c, fmt.Errorf("APP_ requires a non-empty variable name")
			}
			c.AppEnv = append(c.AppEnv, strings.TrimPrefix(e, "APP_"))
		}
	}
	sort.Strings(c.AppEnv)
	if c.GitURL == "" || c.RuntimeImage == "" || c.RunCommand == "" {
		return c, fmt.Errorf("GIT_URL, RUNTIME_IMAGE, and RUN_COMMAND are required")
	}
	u, e := url.Parse(c.GitURL)
	if e != nil || u.Scheme != "https" || u.Host != "github.com" || u.User != nil {
		return c, fmt.Errorf("GIT_URL must be an HTTPS github.com URL without embedded credentials")
	}
	parts := strings.Split(strings.TrimSuffix(strings.TrimPrefix(u.Path, "/"), ".git"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || u.RawQuery != "" || u.Fragment != "" {
		return c, fmt.Errorf("GIT_URL must identify exactly one GitHub owner/repository")
	}
	if c.UpdateMethod != "commit" && c.UpdateMethod != "tag" && c.UpdateMethod != "release" {
		return c, fmt.Errorf("UPDATE_METHOD must be commit, tag, or release")
	}
	if c.HealthPath == "" || c.HealthPath[0] != '/' {
		return c, fmt.Errorf("HEALTH_PATH must begin with /")
	}
	if c.ServicePort > 65535 {
		return c, fmt.Errorf("SERVICE_PORT must be <= 65535")
	}
	return c, nil
}

func memoryLimit() (uint64, error) {
	raw, set := os.LookupEnv("SERVICE_MEMORY_LIMIT")
	if !set {
		raw = "512M"
	}
	value := strings.ToUpper(strings.TrimSpace(raw))
	if value == "" || value == "0" {
		return 0, nil
	}
	multiplier := uint64(1)
	for suffix, factor := range map[string]uint64{"K": 1 << 10, "M": 1 << 20, "G": 1 << 30, "T": 1 << 40} {
		if strings.HasSuffix(value, suffix) {
			value = strings.TrimSuffix(value, suffix)
			multiplier = factor
			break
		}
	}
	n, err := strconv.ParseUint(value, 10, 64)
	if err != nil || n == 0 || n > math.MaxUint64/multiplier {
		return 0, fmt.Errorf("SERVICE_MEMORY_LIMIT must be 0, empty, or a positive byte size with optional K, M, G, or T suffix")
	}
	return n * multiplier, nil
}
func val(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
func duration(k string, d int) (time.Duration, error) {
	v, e := strconv.ParseFloat(val(k, strconv.Itoa(d)), 64)
	if e != nil || math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 || v > float64(math.MaxInt64)/float64(time.Second) {
		return 0, fmt.Errorf("%s must be positive numeric seconds", k)
	}
	return time.Duration(v * float64(time.Second)), nil
}
func integer(k string, d int) (int, error) {
	v, e := strconv.Atoi(val(k, strconv.Itoa(d)))
	if e != nil || v <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", k)
	}
	return v, nil
}
