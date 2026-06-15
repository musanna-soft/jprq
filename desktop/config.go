package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// remoteConfigURL mirrors the CLI: the server publishes the event-server
// address + base domain at /config.json. Override with JPRQ_REMOTE_CONFIG.
func remoteConfigURL() string {
	if v := os.Getenv("JPRQ_REMOTE_CONFIG"); v != "" {
		return v
	}
	return "https://tulki.uz/config.json"
}

type localConf struct {
	AuthToken string `json:"auth_token"`
}

type remoteConf struct {
	Domain string `json:"domain"`
	Events string `json:"events"`
}

// configFile is the same path the CLI uses, so token is shared between them.
func configFile() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("user config dir: %w", err)
	}
	return filepath.Join(dir, "jprq", ".jprq-config"), nil
}

func loadToken() string {
	path, err := configFile()
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var c localConf
	if json.Unmarshal(data, &c) != nil {
		return ""
	}
	return c.AuthToken
}

func saveToken(token string) error {
	path, err := configFile()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	data, err := json.Marshal(localConf{AuthToken: token})
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return nil
}

func fetchRemote() (remoteConf, error) {
	var rc remoteConf
	httpClient := &http.Client{Timeout: 10 * time.Second}
	resp, err := httpClient.Get(remoteConfigURL())
	if err != nil {
		return rc, fmt.Errorf("fetch remote config: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return rc, fmt.Errorf("remote config http %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&rc); err != nil {
		return rc, fmt.Errorf("decode remote config: %w", err)
	}
	if rc.Events == "" {
		return rc, fmt.Errorf("remote config missing events address")
	}
	return rc, nil
}
