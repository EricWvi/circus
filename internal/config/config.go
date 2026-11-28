// Package config loads the control plane's strict YAML configuration.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/EricWvi/circus/internal/auth"
	"github.com/EricWvi/circus/internal/controlplane"
	"github.com/EricWvi/circus/internal/loopback"
	"gopkg.in/yaml.v3"
)

// Config contains all server settings. Zero values select the server defaults.
type Config struct {
	Listen            string        `yaml:"listen"`
	DB                string        `yaml:"db"`
	ProviderKeyFile   string        `yaml:"provider_key_file"`
	TLSCert           string        `yaml:"tls_cert"`
	TLSKey            string        `yaml:"tls_key"`
	BehindProxy       bool          `yaml:"behind_proxy"`
	DevInsecure       bool          `yaml:"dev_insecure"`
	PublicURL         string        `yaml:"public_url"`
	ShutdownTimeout   time.Duration `yaml:"shutdown_timeout"`
	TimerPollInterval time.Duration `yaml:"timer_poll_interval"`
	MaxMessageBytes   int           `yaml:"max_message_bytes"`
	DeviceGrace       time.Duration `yaml:"device_grace"`
	HeartbeatInterval time.Duration `yaml:"heartbeat_interval"`
	HeartbeatTimeout  time.Duration `yaml:"heartbeat_timeout"`
	SendPoolBytes     int           `yaml:"send_pool_bytes"`
	ReceivePoolBytes  int           `yaml:"receive_pool_bytes"`
	WorkerConnections int           `yaml:"worker_connections"`
	Debug             bool          `yaml:"debug"`
	// ReleaseCheck reads the latest circus release for Web machine
	// updates; absent means on.
	ReleaseCheck *bool            `yaml:"release_check"`
	OIDC         *auth.OIDCConfig `yaml:"oidc"`
}

// Load requires a file, rejects unknown keys and multiple YAML documents,
// and applies the OIDC secret environment override, when it is non-empty.
func Load(path string) (Config, error) {
	var c Config
	raw, err := os.ReadFile(path)
	if err != nil {
		return c, fmt.Errorf("read config %q: %w", path, err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil && !errors.Is(err, io.EOF) {
		return c, fmt.Errorf("parse config %q: %w", path, err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return c, fmt.Errorf("config %q: expected one YAML document", path)
	}
	if c.Listen == "" {
		c.Listen = ":8443"
	}
	if c.OIDC != nil {
		if secret := os.Getenv("CIRCUS_OIDC_CLIENT_SECRET"); secret != "" {
			c.OIDC.ClientSecret = secret
		}
		if c.PublicURL == "" {
			return c, errors.New("oidc requires public_url")
		}
		if c.OIDC.Issuer == "" || c.OIDC.ClientID == "" {
			return c, errors.New("oidc.issuer and oidc.client_id are required")
		}
	}
	if c.PublicURL != "" {
		u, err := url.Parse(c.PublicURL)
		if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil ||
			(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
			return c, errors.New("public_url must be an origin such as https://circus.example.com")
		}
		if _, err := loopback.CheckScheme(u); err != nil {
			return c, fmt.Errorf("public_url: %w", err)
		}
		c.PublicURL = strings.TrimSuffix(c.PublicURL, "/")
	}
	if c.WorkerConnections < 0 {
		return c, errors.New("worker_connections must be at least 1 (0 uses the default)")
	}
	return c, nil
}

// ReleaseCheckEnabled reports whether the server reads the latest release
// (release_check, on unless false).
func (c Config) ReleaseCheckEnabled() bool { return c.ReleaseCheck == nil || *c.ReleaseCheck }

// Server returns the programmatic server configuration; runtime hooks stay
// outside the YAML file.
func (c Config) Server() controlplane.Config {
	return controlplane.Config{
		Listen: c.Listen, DBPath: c.DB, ProviderKeyFile: c.ProviderKeyFile, TLSCert: c.TLSCert, TLSKey: c.TLSKey,
		BehindProxy: c.BehindProxy, DevInsecure: c.DevInsecure, PublicURL: c.PublicURL,
		ShutdownTimeout: c.ShutdownTimeout, TimerPollInterval: c.TimerPollInterval,
		MaxMessageBytes: c.MaxMessageBytes, GracePeriod: c.DeviceGrace,
		HeartbeatInterval: c.HeartbeatInterval, HeartbeatTimeout: c.HeartbeatTimeout,
		SendPoolBytes: c.SendPoolBytes, ReceivePoolBytes: c.ReceivePoolBytes,
		WorkerConnections: c.WorkerConnections, OIDC: c.OIDC,
	}
}
