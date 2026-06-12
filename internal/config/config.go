// Package config loads and validates diskmon configuration from YAML files.
package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// ClientConfig is the configuration for the diskmon client (Windows agent).
type ClientConfig struct {
	Postgres              PostgresConfig  `yaml:"postgres"`
	ServerID              string          `yaml:"server_id"`
	Name                  string          `yaml:"name"`           // display name shown in server UI
	SmbUser               string          `yaml:"smb_user"`       // Windows account for SMB (info only)
	CheckpointPath        string          `yaml:"checkpoint_path"`
	LogPath               string          `yaml:"log_path"`
	API                   ClientAPIConfig `yaml:"api"`
	PollIntervalMs        int             `yaml:"poll_interval_ms"`
	RenameWindowMs        int             `yaml:"rename_window_ms"`
	PathResolverCacheSize int             `yaml:"path_resolver_cache_size"`
	Volumes               []VolumeConfig  `yaml:"volumes"`
	Filters               FiltersConfig   `yaml:"filters"`
}

// ClientAPIConfig is the management HTTP server embedded in the client.
type ClientAPIConfig struct {
	Listen string `yaml:"listen"` // e.g. ":19090"
	Token  string `yaml:"token"`  // bearer token for auth
}

// ServerConfig is the configuration for the diskmon server (API service).
type ServerConfig struct {
	Postgres      PostgresConfig               `yaml:"postgres"`
	Listen        string                       `yaml:"listen"`
	SmbMounts     map[string]map[string]string `yaml:"smb_mounts"` // server_id → volume → local mount path
	KkFileViewURL string                       `yaml:"kkfileview_url"`
	// KkFileViewPublicURL is the browser-reachable kkFileView address used for
	// preview redirects (the internal kkfileview_url is not resolvable from a
	// user's browser). Falls back to KkFileViewURL when empty.
	KkFileViewPublicURL string      `yaml:"kkfileview_public_url"`
	AList               AListConfig `yaml:"alist"`
	Admin               AdminConfig `yaml:"admin"`
}

// AdminConfig holds credentials for the internal management UI (server/client
// configuration). The management page and its config APIs sit behind a login;
// the public file-data APIs stay anonymous. Password can be overridden by the
// ADMIN_PASSWORD environment variable (injected from a k8s secret).
type AdminConfig struct {
	User     string `yaml:"user"`     // default "admin"
	Password string `yaml:"password"` // injected from env ADMIN_PASSWORD in k8s
}

// AListConfig holds connection info for the AList admin API.
// Password can be overridden by the ALIST_PASSWORD environment variable.
type AListConfig struct {
	URL      string `yaml:"url"`      // internal admin/API URL, e.g. http://alist.middleware.svc.cluster.local:5244
	Username string `yaml:"username"` // AList admin username, default "admin"
	Password string `yaml:"password"` // injected from env ALIST_PASSWORD in k8s
	// PublicURL is the browser-reachable AList address (NodePort/ingress) used
	// as the base for file download/preview links. Falls back to URL when empty.
	PublicURL string `yaml:"public_url"`
}

// PostgresConfig holds the database connection string.
type PostgresConfig struct {
	DSN string `yaml:"dsn"`
}

// VolumeConfig describes a single NTFS volume to monitor.
type VolumeConfig struct {
	Name        string `yaml:"name"`         // e.g. "D:"
	JournalSize int64  `yaml:"journal_size"` // bytes, default 128 MB
	BizRule     string `yaml:"biz_rule"`     // name of the VolumeRule implementation to use
}

// FiltersConfig controls which events and paths are processed.
type FiltersConfig struct {
	IncludeDirs []string `yaml:"include_dirs"`
	ExcludeDirs []string `yaml:"exclude_dirs"`
	Extensions  []string `yaml:"extensions"` // empty = all
	Events      []string `yaml:"events"`      // empty = all
}

func (c *ClientConfig) setDefaults() {
	if c.PollIntervalMs == 0 {
		c.PollIntervalMs = 1000
	}
	if c.RenameWindowMs == 0 {
		c.RenameWindowMs = 500
	}
	if c.PathResolverCacheSize == 0 {
		c.PathResolverCacheSize = 200_000
	}
	if c.CheckpointPath == "" {
		c.CheckpointPath = `C:\diskmon\checkpoints`
	}
	if c.LogPath == "" {
		c.LogPath = `C:\diskmon\logs\client.log`
	}
	if c.API.Listen == "" {
		c.API.Listen = ":19090"
	}
	for i := range c.Volumes {
		if c.Volumes[i].JournalSize == 0 {
			c.Volumes[i].JournalSize = 128 * 1024 * 1024
		}
	}
}

func (c *ClientConfig) validate() error {
	if c.ServerID == "" {
		return fmt.Errorf("server_id is required")
	}
	if c.Postgres.DSN == "" {
		return fmt.Errorf("postgres.dsn is required")
	}
	if len(c.Volumes) == 0 {
		return fmt.Errorf("at least one volume must be configured")
	}
	return nil
}

// LoadClient reads and validates a ClientConfig from the given YAML file path.
func LoadClient(path string) (*ClientConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	var cfg ClientConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	cfg.setDefaults()
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}
	return &cfg, nil
}

// ValidateClient applies defaults and validates a ClientConfig (used before
// persisting a remotely-edited config so an invalid one is never written).
func ValidateClient(c *ClientConfig) error {
	c.setDefaults()
	return c.validate()
}

// SaveClient writes the client config back to path as YAML, atomically (write to
// a temp file then rename). Comments in the original file are not preserved.
func SaveClient(path string, c *ClientConfig) error {
	data, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write temp config %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("replace config %s: %w", path, err)
	}
	return nil
}

// LoadServer reads and validates a ServerConfig from the given YAML file path.
// Environment variable overrides:
//   - POSTGRES_DSN  → postgres.dsn
//   - ALIST_PASSWORD → alist.password
func LoadServer(path string) (*ServerConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	var cfg ServerConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	if dsn := os.Getenv("POSTGRES_DSN"); dsn != "" {
		cfg.Postgres.DSN = dsn
	}
	if pw := os.Getenv("ALIST_PASSWORD"); pw != "" {
		cfg.AList.Password = pw
	}
	if pw := os.Getenv("ADMIN_PASSWORD"); pw != "" {
		cfg.Admin.Password = pw
	}
	if cfg.Postgres.DSN == "" {
		return nil, fmt.Errorf("postgres.dsn is required (set via config or POSTGRES_DSN env var)")
	}
	if cfg.Listen == "" {
		cfg.Listen = ":8080"
	}
	if cfg.AList.Username == "" {
		cfg.AList.Username = "admin"
	}
	if cfg.Admin.User == "" {
		cfg.Admin.User = "admin"
	}
	if cfg.Admin.Password == "" {
		return nil, fmt.Errorf("admin.password is required (set via config or ADMIN_PASSWORD env var)")
	}
	return &cfg, nil
}
