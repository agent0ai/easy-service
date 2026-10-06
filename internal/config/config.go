package config

import (
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	GitURL, GitToken, GitBranch, UpdateMethod, UpdatePattern    string
	RuntimeImage, SetupCommand, RunCommand, HealthPath, DataDir string
	ConfigDir, LogDir                                           string
	LogRetentionDays                                            int
	LogMaxFileSize, LogMaxTotalSize                             uint64
	PollInterval, StartupTimeout, HealthInterval                time.Duration
	DrainTimeout, SetupTimeout                                  time.Duration
	HealthFailures                                              int
	ServiceMemoryLimit                                          uint64
	AppEnv                                                      []string
}

// Defaults are also the public configuration surface; APP_* values are explicit workload inputs.
var Defaults = map[string]string{
	"GIT_URL": "", "GIT_TOKEN": "", "GIT_BRANCH": "main", "UPDATE_METHOD": "commit", "UPDATE_PATTERN": "*",
	"RUNTIME_IMAGE": "debian:bookworm-slim", "SETUP_COMMAND": "", "RUN_COMMAND": "", "HEALTH_PATH": "/",
	"DATA_DIR": "/data", "CONFIG_DIR": "/config", "LOG_DIR": "/logs",
	"POLL_INTERVAL": "300", "STARTUP_TIMEOUT": "60", "HEALTH_INTERVAL": "10", "HEALTH_FAILURES": "3",
	"SERVICE_MEMORY_LIMIT": "512M", "DRAIN_TIMEOUT": "30", "SETUP_TIMEOUT": "900",
	"LOG_RETENTION_DAYS": "30", "LOG_MAX_FILE_SIZE": "10M", "LOG_MAX_TOTAL_SIZE": "1G",
}

func ValidName(name string) bool {
	if _, ok := Defaults[name]; ok {
		return true
	}
	if !strings.HasPrefix(name, "APP_") || len(name) == 4 {
		return false
	}
	for i, r := range name[4:] {
		if !(r == '_' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || i > 0 && r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

func Environment() map[string]string {
	values := make(map[string]string)
	for k, v := range Defaults {
		values[k] = v
	}
	for _, item := range os.Environ() {
		k, v, ok := strings.Cut(item, "=")
		if ok && (ValidName(k) || strings.HasPrefix(k, "APP_")) {
			values[k] = v
		}
	}
	return values
}
func Load() (Config, error) { return Parse(Environment(), true) }

func statePathsOverlap(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	return a == b || strings.HasPrefix(a, b+string(filepath.Separator)) || strings.HasPrefix(b, a+string(filepath.Separator))
}

func Parse(values map[string]string, required bool) (Config, error) {
	merged := make(map[string]string, len(Defaults)+len(values))
	for k, v := range Defaults {
		merged[k] = v
	}
	for k, v := range values {
		merged[k] = v
	}
	values = merged
	get := func(k string) string { return values[k] }
	val := func(k, d string) string {
		if v := values[k]; v != "" {
			return v
		}
		return d
	}
	duration := func(k string, d int) (time.Duration, error) { return parseDuration(k, val(k, strconv.Itoa(d))) }
	integer := func(k string, d int) (int, error) { return parseInteger(k, val(k, strconv.Itoa(d))) }
	c := Config{GitURL: get("GIT_URL"), GitToken: get("GIT_TOKEN"), GitBranch: val("GIT_BRANCH", "main"), UpdateMethod: val("UPDATE_METHOD", "commit"), UpdatePattern: val("UPDATE_PATTERN", "*"), RuntimeImage: val("RUNTIME_IMAGE", "debian:bookworm-slim"), SetupCommand: get("SETUP_COMMAND"), RunCommand: get("RUN_COMMAND"), HealthPath: val("HEALTH_PATH", "/"), DataDir: val("DATA_DIR", "/data")}
	var err error
	c.ConfigDir, c.LogDir = val("CONFIG_DIR", "/config"), val("LOG_DIR", "/logs")
	if c.LogRetentionDays, err = integer("LOG_RETENTION_DAYS", 30); err != nil {
		return c, err
	}
	if c.LogRetentionDays > int(math.MaxInt64/int64(24*time.Hour)) {
		return c, fmt.Errorf("LOG_RETENTION_DAYS is too large")
	}
	if c.LogMaxFileSize, err = parseSize("LOG_MAX_FILE_SIZE", val("LOG_MAX_FILE_SIZE", "10M"), false); err != nil {
		return c, err
	}
	if c.LogMaxTotalSize, err = parseSize("LOG_MAX_TOTAL_SIZE", val("LOG_MAX_TOTAL_SIZE", "1G"), false); err != nil {
		return c, err
	}
	if c.LogMaxFileSize < 1024 || c.LogMaxTotalSize < c.LogMaxFileSize {
		return c, fmt.Errorf("log file size must be at least 1K and total size must be at least the file size")
	}
	if c.DrainTimeout, err = duration("DRAIN_TIMEOUT", 30); err != nil {
		return c, err
	}
	if c.DrainTimeout > time.Duration(math.MaxInt64)-15*time.Second {
		return c, fmt.Errorf("DRAIN_TIMEOUT is too large")
	}
	if c.SetupTimeout, err = duration("SETUP_TIMEOUT", 900); err != nil {
		return c, err
	}
	for _, p := range []string{c.DataDir, c.ConfigDir, c.LogDir} {
		if !filepath.IsAbs(p) || filepath.Clean(p) == "/" {
			return c, fmt.Errorf("DATA_DIR, CONFIG_DIR and LOG_DIR must be absolute directories below /")
		}
	}
	if statePathsOverlap(c.DataDir, c.ConfigDir) || statePathsOverlap(c.DataDir, c.LogDir) || statePathsOverlap(c.ConfigDir, c.LogDir) {
		return c, fmt.Errorf("DATA_DIR, CONFIG_DIR and LOG_DIR must be separate directories")
	}

	if c.PollInterval, err = duration("POLL_INTERVAL", 300); err != nil {
		return c, err
	}
	if c.StartupTimeout, err = duration("STARTUP_TIMEOUT", 60); err != nil {
		return c, err
	}
	if c.HealthInterval, err = duration("HEALTH_INTERVAL", 10); err != nil {
		return c, err
	}
	if c.HealthFailures, err = integer("HEALTH_FAILURES", 3); err != nil {
		return c, err
	}
	if c.ServiceMemoryLimit, err = parseSize("SERVICE_MEMORY_LIMIT", values["SERVICE_MEMORY_LIMIT"], true); err != nil {
		return c, err
	}
	for k, v := range values {
		if strings.HasPrefix(k, "APP_") {
			if !ValidName(k) || strings.IndexByte(v, 0) >= 0 {
				return c, fmt.Errorf("APP_ requires a non-empty variable name")
			}
			c.AppEnv = append(c.AppEnv, strings.TrimPrefix(k, "APP_")+"="+v)
		}
	}
	sort.Strings(c.AppEnv)
	if required && (c.GitURL == "" || c.RunCommand == "") {
		return c, fmt.Errorf("GIT_URL and RUN_COMMAND are required")
	}
	if c.GitURL != "" {
		u, e := url.Parse(c.GitURL)
		if e == nil && u.Scheme == "" && u.Host == "" && u.RawQuery == "" && u.Fragment == "" {
			path, err := filepath.Abs(c.GitURL)
			if err != nil {
				return c, fmt.Errorf("invalid local GIT_URL path: %w", err)
			}
			st, err := os.Stat(path)
			if err != nil || !st.IsDir() {
				return c, fmt.Errorf("local GIT_URL must be an existing repository directory")
			}
			if c.UpdateMethod == "release" {
				return c, fmt.Errorf("UPDATE_METHOD=release requires a GitHub repository")
			}
			c.GitURL = path
		} else if e != nil || u.Scheme != "https" || u.Host != "github.com" || u.User != nil {
			return c, fmt.Errorf("GIT_URL must be an HTTPS github.com URL without embedded credentials")
		} else {
			parts := strings.Split(strings.TrimSuffix(strings.TrimPrefix(u.Path, "/"), ".git"), "/")
			if len(parts) != 2 || parts[0] == "" || parts[1] == "" || u.RawQuery != "" || u.Fragment != "" {
				return c, fmt.Errorf("GIT_URL must identify exactly one GitHub owner/repository")
			}
		}
	}
	if c.UpdateMethod != "commit" && c.UpdateMethod != "tag" && c.UpdateMethod != "release" {
		return c, fmt.Errorf("UPDATE_METHOD must be commit, tag, or release")
	}
	if c.HealthPath == "" || c.HealthPath[0] != '/' {
		return c, fmt.Errorf("HEALTH_PATH must begin with /")
	}
	return c, nil
}

func parseSize(name, raw string, zero bool) (uint64, error) {
	value := strings.ToUpper(strings.TrimSpace(raw))
	if zero && (value == "" || value == "0") {
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
		return 0, fmt.Errorf("%s must be a positive byte size with optional K, M, G, or T suffix (SERVICE_MEMORY_LIMIT also accepts 0 or empty)", name)
	}
	return n * multiplier, nil
}
func parseDuration(k, raw string) (time.Duration, error) {
	v, e := strconv.ParseFloat(raw, 64)
	if e != nil || math.IsNaN(v) || math.IsInf(v, 0) || v*float64(time.Second) < 1 || v*float64(time.Second) >= float64(math.MaxInt64) {
		return 0, fmt.Errorf("%s must be positive numeric seconds", k)
	}
	return time.Duration(v * float64(time.Second)), nil
}
func parseInteger(k, raw string) (int, error) {
	v, e := strconv.Atoi(raw)
	if e != nil || v <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", k)
	}
	return v, nil
}
