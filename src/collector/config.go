package main

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config represents the collector configuration file
type Config struct {
	Input     ConfigInput
	Storage   ConfigStorage
	Collector ConfigCollector
	Retention ConfigRetention
	Logging   ConfigLogging
	Auth      ConfigAuth
	Server    ConfigServer
	Cloud     CloudConfig // [cloud] — shipping to a PHPRay console (docs/spec/ingest-v1.md)
}

type ConfigInput struct {
	JSONLPath   string
	SHMPath     string
	ControlPath string // shared-memory control table written by the collector, read by the extension
}

type ConfigStorage struct {
	DBPath string
}

type ConfigCollector struct {
	PollIntervalMs int
	FlushIntervalS int
	BatchSize      int
	AggIntervalS   int
	Mode           string // "auto", "ring", "jsonl"
}

type ConfigRetention struct {
	Days  int
	MaxMB int // hard cap on the SQLite file: oldest traces are purged when exceeded (0 = off)
}

type ConfigLogging struct {
	Level string
	Quiet bool
}

type ConfigAuth struct {
	Enabled bool
	Secret  string
}

type ConfigServer struct {
	DADataPath string // Path to DirectAdmin data/users/ directory (empty = default /usr/local/directadmin/data/users)
}

// DefaultConfig returns a config with sensible defaults
func DefaultConfig() *Config {
	return &Config{
		Input: ConfigInput{
			JSONLPath:   "/tmp/phpray.jsonl",
			SHMPath:     "/dev/shm/phpray",
			ControlPath: "/dev/shm/phpray-control",
		},
		Storage: ConfigStorage{
			DBPath: "/var/lib/phpray/traces.db",
		},
		Collector: ConfigCollector{
			PollIntervalMs: 100,
			FlushIntervalS: 5,
			BatchSize:      50,
			AggIntervalS:   60,
			Mode:           "auto",
		},
		Retention: ConfigRetention{
			Days: 30, MaxMB: 4096,
		},
		Logging: ConfigLogging{
			Level: "info",
			Quiet: false,
		},
		Cloud: DefaultCloudConfig(),
	}
}

// LoadConfig reads a TOML-like config file (simple key=value with [sections])
// We use a minimal parser to avoid external dependencies.
func LoadConfig(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	cfg := DefaultConfig()
	section := ""

	scanner := bufio.NewScanner(f)
	lineNum := 0

	for scanner.Scan() {
		lineNum++
		line := strings.TrimSpace(scanner.Text())

		// Skip empty lines and comments
		if line == "" || line[0] == '#' {
			continue
		}

		// Section header
		if line[0] == '[' {
			end := strings.IndexByte(line, ']')
			if end < 0 {
				return nil, fmt.Errorf("line %d: unclosed section bracket", lineNum)
			}
			section = strings.TrimSpace(line[1:end])
			continue
		}

		// Key = value
		eqIdx := strings.IndexByte(line, '=')
		if eqIdx < 0 {
			return nil, fmt.Errorf("line %d: expected key = value", lineNum)
		}

		key := strings.TrimSpace(line[:eqIdx])
		val := stripInlineComment(strings.TrimSpace(line[eqIdx+1:]))

		// Remove quotes from string values
		if len(val) >= 2 && val[0] == '"' && val[len(val)-1] == '"' {
			val = val[1 : len(val)-1]
		}

		fullKey := key
		if section != "" {
			fullKey = section + "." + key
		}

		switch fullKey {
		case "input.jsonl_path":
			cfg.Input.JSONLPath = val
		case "input.shm_path":
			cfg.Input.SHMPath = val
		case "input.control_path":
			cfg.Input.ControlPath = val
		case "storage.db_path":
			cfg.Storage.DBPath = val
		case "collector.poll_interval_ms":
			cfg.Collector.PollIntervalMs, _ = strconv.Atoi(val)
		case "collector.flush_interval_s":
			cfg.Collector.FlushIntervalS, _ = strconv.Atoi(val)
		case "collector.batch_size":
			cfg.Collector.BatchSize, _ = strconv.Atoi(val)
		case "collector.agg_interval_s":
			cfg.Collector.AggIntervalS, _ = strconv.Atoi(val)
		case "collector.mode":
			cfg.Collector.Mode = val
		case "retention.max_mb":
			cfg.Retention.MaxMB, _ = strconv.Atoi(val)
		case "retention.days":
			cfg.Retention.Days, _ = strconv.Atoi(val)
		case "logging.level":
			cfg.Logging.Level = val
		case "logging.quiet":
			cfg.Logging.Quiet = val == "true" || val == "1" || val == "yes"
		case "auth.enabled":
			cfg.Auth.Enabled = val == "true" || val == "1" || val == "yes"
		case "auth.secret":
			cfg.Auth.Secret = val
		case "server.da_data_path":
			cfg.Server.DADataPath = val
		case "cloud.enabled":
			cfg.Cloud.Enabled = val == "true" || val == "1" || val == "yes"
		case "cloud.endpoint":
			cfg.Cloud.Endpoint = val
		case "cloud.server_key":
			cfg.Cloud.ServerKey = val
		case "cloud.buffer.max_mb", "cloud.buffer_max_mb":
			cfg.Cloud.BufferMaxMB, _ = strconv.Atoi(val)
		case "cloud.buffer.dir", "cloud.buffer_dir":
			cfg.Cloud.BufferDir = val
		case "cloud.traces.sample_rate", "cloud.traces_sample_rate":
			cfg.Cloud.TraceSampleRate, _ = strconv.Atoi(val)
		case "cloud.privacy.mask_host", "cloud.mask_host":
			cfg.Cloud.MaskHost = val == "true" || val == "1" || val == "yes"
		case "cloud.control":
			cfg.Cloud.ControlEnabled = val == "true" || val == "1" || val == "yes"
		default:
			return nil, fmt.Errorf("line %d: unknown config key: %s", lineNum, fullKey)
		}
	}

	return cfg, scanner.Err()
}

// ResolveAuthSecret returns the effective JWT secret.
// Priority: config file → PHPRAY_JWT_SECRET env → "" (auth disabled).
func (c *Config) ResolveAuthSecret() string {
	if c.Auth.Secret != "" {
		return c.Auth.Secret
	}
	return os.Getenv("PHPRAY_JWT_SECRET")
}

// ToDaemonConfig converts Config to DaemonConfig
func (c *Config) ToDaemonConfig() *DaemonConfig {
	stateDir := strings.TrimSuffix(c.Storage.DBPath, ".db")
	if idx := strings.LastIndexByte(c.Storage.DBPath, '/'); idx >= 0 {
		stateDir = c.Storage.DBPath[:idx]
	}

	mode := c.Collector.Mode
	if mode == "" {
		mode = "auto"
	}

	cloud := c.Cloud
	if cloud.BufferDir == "" {
		cloud.BufferDir = stateDir + "/cloud-buffer"
	}

	return &DaemonConfig{
		Cloud:          cloud,
		ControlPath:    c.Input.ControlPath,
		JSONLPath:      c.Input.JSONLPath,
		SHMPath:        c.Input.SHMPath,
		DBPath:         c.Storage.DBPath,
		PollIntervalMs: c.Collector.PollIntervalMs,
		FlushInterval:  time.Duration(c.Collector.FlushIntervalS) * time.Second,
		BatchSize:      c.Collector.BatchSize,
		AggInterval:    time.Duration(c.Collector.AggIntervalS) * time.Second,
		RetentionDays:  c.Retention.Days,
		RetentionMaxMB: c.Retention.MaxMB,
		StateFile:      stateDir + "/collector.state",
		LogLevel:       c.Logging.Level,
		Quiet:          c.Logging.Quiet,
		Mode:           mode,
	}
}


// stripInlineComment drops a trailing "# comment" from a TOML value, leaving
// '#' inside quoted strings alone (`buffer.dir = ""   # default` used to yield
// the comment as the value and broke the cloud buffer directory).
func stripInlineComment(val string) string {
	inStr := false
	for i := 0; i < len(val); i++ {
		switch val[i] {
		case '\\':
			if inStr {
				i++ // skip the escaped character
			}
		case '"':
			inStr = !inStr
		case '#':
			if !inStr {
				return strings.TrimSpace(val[:i])
			}
		}
	}
	return val
}
