package service

import (
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
)

// launchd runs the daemon as a macOS user agent.
//
// A LaunchAgent rather than a LaunchDaemon: agents run as the logged-in user,
// which is the only account whose credentials and config directory the daemon
// should be able to read.
type launchd struct{}

func (launchd) Name() string { return "launchd" }

func (launchd) Path() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", LaunchdLabel+".plist"), nil
}

func (launchd) Render(cfg Config) (string, error) {
	// path, not filepath: what goes inside a plist is a macOS path however
	// this binary was compiled, and a cross-compiled render should say the
	// same thing as a native one.
	logFile := path.Join(cfg.LogDir, "agentswap.log")

	var env strings.Builder
	if cfg.ConfigDir != "" {
		env.WriteString("\n\t<key>EnvironmentVariables</key>\n\t<dict>\n")
		fmt.Fprintf(&env, "\t\t<key>AGENTSWAP_HOME</key>\n\t\t<string>%s</string>\n", escape(cfg.ConfigDir))
		env.WriteString("\t</dict>")
	}

	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>%s</string>
	<key>ProgramArguments</key>
	<array>
		<string>%s</string>
		<string>serve</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>ProcessType</key>
	<string>Background</string>
	<key>StandardOutPath</key>
	<string>%s</string>
	<key>StandardErrorPath</key>
	<string>%s</string>%s
</dict>
</plist>
`, LaunchdLabel, escape(cfg.Binary), escape(logFile), escape(logFile), env.String()), nil
}

func (l launchd) Install(cfg Config) error {
	path, err := l.Path()
	if err != nil {
		return err
	}
	body, err := l.Render(cfg)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.LogDir, 0o700); err != nil {
		return err
	}
	if err := writeFile(path, body); err != nil {
		return err
	}

	// Replacing a loaded service means unloading first; ignore the failure,
	// which is just "it was not loaded".
	_ = l.Uninstall()
	if err := writeFile(path, body); err != nil {
		return err
	}

	target := fmt.Sprintf("gui/%d", os.Getuid())
	if err := run("launchctl", "bootstrap", target, path); err != nil {
		// bootstrap is the modern spelling; load -w still works everywhere it
		// does not, and on some versions is the only one that does.
		if legacy := run("launchctl", "load", "-w", path); legacy != nil {
			return fmt.Errorf("bootstrap failed (%w), and `load -w` also failed: %w", err, legacy)
		}
	}
	return nil
}

func (l launchd) Uninstall() error {
	path, err := l.Path()
	if err != nil {
		return err
	}
	target := fmt.Sprintf("gui/%d/%s", os.Getuid(), LaunchdLabel)
	if err := run("launchctl", "bootout", target); err != nil {
		_ = run("launchctl", "unload", "-w", path)
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (launchd) Running() (bool, error) {
	// `print` exits non-zero when the service is not loaded, which is the
	// question rather than an error.
	out, err := exec.Command("launchctl", "print",
		fmt.Sprintf("gui/%d/%s", os.Getuid(), LaunchdLabel)).CombinedOutput()
	if err != nil {
		// `print` exits non-zero for a service that is not loaded, which is
		// the answer to the question rather than a failure to answer it.
		return false, nil //nolint:nilerr // a non-zero exit means "not loaded"
	}
	// A loaded-but-crashed agent has no pid.
	return strings.Contains(string(out), "pid = "), nil
}

func (launchd) Logs(cfg Config) string {
	return "tail -f " + path.Join(cfg.LogDir, "agentswap.log")
}

// escape makes a value safe inside a plist string element.
func escape(s string) string {
	var b strings.Builder
	if err := xml.EscapeText(&b, []byte(s)); err != nil {
		return s
	}
	return b.String()
}
