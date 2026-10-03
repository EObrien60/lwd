package controller

import (
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
)

// DefaultListen is loopback-only: the controller is reached over SSH/VPN or a
// reverse proxy, never exposed directly.
const DefaultListen = "127.0.0.1:7470"

// Config is the controller's environment.
type Config struct {
	DatabaseURL        string   // LWD_DATABASE_URL (required)
	Listen             string   // LWD_LISTEN
	APIToken           string   // LWD_API_TOKEN (required)
	SecretKeyFile      string   // LWD_SECRET_KEY_FILE
	InsecureRegistries []string // LWD_INSECURE_REGISTRIES, comma separated; dev only
	BackupHour         int      // LWD_BACKUP_HOUR, local hour 0-23 of the daily database backups (default 3)
}

// ConfigFromEnv reads Config. It refuses to proceed without an API token:
// an unauthenticated controller would hand out deploys and secrets.
func ConfigFromEnv(getenv func(string) string) (Config, error) {
	cfg := Config{
		DatabaseURL:   getenv("LWD_DATABASE_URL"),
		Listen:        getenv("LWD_LISTEN"),
		APIToken:      strings.TrimSpace(getenv("LWD_API_TOKEN")),
		SecretKeyFile: getenv("LWD_SECRET_KEY_FILE"),
	}
	var errs []error
	if cfg.DatabaseURL == "" {
		errs = append(errs, errors.New("LWD_DATABASE_URL is required"))
	}
	if cfg.APIToken == "" {
		errs = append(errs, errors.New("LWD_API_TOKEN is required (refusing to serve an unauthenticated API)"))
	}
	if cfg.Listen == "" {
		cfg.Listen = DefaultListen
	}
	if cfg.SecretKeyFile == "" {
		dir := getenv("XDG_CONFIG_HOME")
		if dir == "" {
			dir = filepath.Join(getenv("HOME"), ".config")
		}
		cfg.SecretKeyFile = filepath.Join(dir, "lwd", "secret.key")
	}
	cfg.BackupHour = 3
	if s := strings.TrimSpace(getenv("LWD_BACKUP_HOUR")); s != "" {
		h, err := strconv.Atoi(s)
		if err != nil || h < 0 || h > 23 {
			errs = append(errs, fmt.Errorf("LWD_BACKUP_HOUR must be an hour 0-23, got %q", s))
		} else {
			cfg.BackupHour = h
		}
	}
	for _, r := range strings.Split(getenv("LWD_INSECURE_REGISTRIES"), ",") {
		if r = strings.TrimSpace(r); r != "" {
			cfg.InsecureRegistries = append(cfg.InsecureRegistries, r)
		}
	}
	return cfg, errors.Join(errs...)
}
