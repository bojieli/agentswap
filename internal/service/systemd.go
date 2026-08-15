package service

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// systemd runs the daemon as a systemd user unit.
//
// User rather than system: the daemon reads one person's config directory and
// holds their credentials. A system unit would run as root and need to be told
// whose account to use, which is a worse answer to the same question.
type systemd struct{}

func (systemd) Name() string { return "systemd" }

func (systemd) Path() (string, error) {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "systemd", "user", SystemdUnit), nil
}

func (systemd) Render(cfg Config) (string, error) {
	var env string
	if cfg.ConfigDir != "" {
		env = fmt.Sprintf("Environment=AGENTSWAP_HOME=%s\n", cfg.ConfigDir)
	}
	return fmt.Sprintf(`[Unit]
Description=agentswap — keeps Claude Code and Codex going when the upstream says no
Documentation=https://github.com/bojieli/agentswap
After=network-online.target

[Service]
Type=simple
ExecStart=%s serve
%sRestart=on-failure
RestartSec=5

# The daemon holds live OAuth tokens and needs nothing but its own config
# directory and the network.
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=read-only
ReadWritePaths=%s

[Install]
WantedBy=default.target
`, cfg.Binary, env, cfg.ConfigDir), nil
}

func (s systemd) Install(cfg Config) error {
	path, err := s.Path()
	if err != nil {
		return err
	}
	body, err := s.Render(cfg)
	if err != nil {
		return err
	}
	if err := writeFile(path, body); err != nil {
		return err
	}
	if err := run("systemctl", "--user", "daemon-reload"); err != nil {
		return err
	}
	return run("systemctl", "--user", "enable", "--now", SystemdUnit)
}

func (s systemd) Uninstall() error {
	path, err := s.Path()
	if err != nil {
		return err
	}
	_ = run("systemctl", "--user", "disable", "--now", SystemdUnit)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return run("systemctl", "--user", "daemon-reload")
}

func (systemd) Running() (bool, error) {
	out, err := exec.Command("systemctl", "--user", "is-active", SystemdUnit).CombinedOutput()
	// is-active exits non-zero for anything that is not active, which is an
	// answer rather than a failure.
	return strings.TrimSpace(string(out)) == "active", err //nolint:nilerr // the exit code is the answer
}

func (systemd) Logs(Config) string {
	return "journalctl --user -u " + SystemdUnit + " -f"
}
