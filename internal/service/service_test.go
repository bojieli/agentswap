package service

import (
	"encoding/xml"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func testConfig() Config {
	return Config{
		Binary:    "/usr/local/bin/agentswap",
		ConfigDir: "/home/someone/.config/agentswap",
		LogDir:    "/home/someone/.config/agentswap/logs",
	}
}

// A malformed plist fails at load with a message that says nothing about what
// is wrong with it, so the shape is worth checking here.
func TestLaunchdPlistIsWellFormed(t *testing.T) {
	body, err := launchd{}.Render(testConfig())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	var parsed any
	if err := xml.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("the plist is not valid XML: %v\n%s", err, body)
	}

	for _, want := range []string{
		"<string>" + LaunchdLabel + "</string>",
		"<string>/usr/local/bin/agentswap</string>",
		"<string>serve</string>",
		"<key>RunAtLoad</key>", // start at login
		"<key>KeepAlive</key>", // and again if it dies
		"AGENTSWAP_HOME",       // the same pool the CLI edits
		"/home/someone/.config/agentswap/logs/agentswap.log",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("plist is missing %q:\n%s", want, body)
		}
	}
}

// A path with an ampersand or a quote in it is legal on both platforms and
// would otherwise produce a file the manager cannot parse.
func TestLaunchdEscapesPaths(t *testing.T) {
	cfg := testConfig()
	cfg.ConfigDir = `/home/some & one/"config"`
	cfg.Binary = `/opt/a&b/agentswap`

	body, err := launchd{}.Render(cfg)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if strings.Contains(body, "& one") || strings.Contains(body, `"config"`) {
		t.Errorf("raw XML metacharacters survived into the plist:\n%s", body)
	}
	var parsed any
	if err := xml.Unmarshal([]byte(body), &parsed); err != nil {
		t.Errorf("the plist is not valid XML with an awkward path: %v\n%s", err, body)
	}
}

func TestSystemdUnit(t *testing.T) {
	body, err := systemd{}.Render(testConfig())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	for _, want := range []string{
		"ExecStart=/usr/local/bin/agentswap serve",
		"Environment=AGENTSWAP_HOME=/home/someone/.config/agentswap",
		"Restart=on-failure",
		"WantedBy=default.target",
		"ReadWritePaths=/home/someone/.config/agentswap",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("unit is missing %q:\n%s", want, body)
		}
	}

	// An empty value for a directive is a syntax error, not an omission, and
	// systemd refuses the whole unit over it.
	for _, line := range strings.Split(body, "\n") {
		if strings.HasSuffix(line, "=") {
			t.Errorf("unit has a directive with no value: %q", line)
		}
	}

	// Every line is a section header, a comment, blank, or key=value.
	for _, line := range strings.Split(strings.TrimSpace(body), "\n") {
		switch {
		case line == "", strings.HasPrefix(line, "#"),
			strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]"),
			strings.Contains(line, "="):
		default:
			t.Errorf("unit has a line that is not a directive: %q", line)
		}
	}
}

// The daemon reads and writes one person's credentials, so it runs as them.
func TestServicesAreUserScoped(t *testing.T) {
	unit, err := systemd{}.Render(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	for _, unwanted := range []string{"User=", "Group=", "[Install]\nWantedBy=multi-user.target"} {
		if strings.Contains(unit, unwanted) {
			t.Errorf("the unit looks system-scoped: found %q", unwanted)
		}
	}

	path, err := systemd{}.Path()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(path, filepath.Join("systemd", "user")) {
		t.Errorf("unit path = %q, want it under systemd/user", path)
	}

	plist, err := launchd{}.Path()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plist, "LaunchAgents") {
		t.Errorf("plist path = %q, want a LaunchAgent rather than a LaunchDaemon", plist)
	}
}

// A service pointing at a binary that has been deleted fails at boot, long
// after anyone would connect it to how they ran the install.
func TestResolveBinaryRejectsATemporaryBuild(t *testing.T) {
	path, err := ResolveBinary()
	if err != nil {
		// The test binary itself lives under the temp directory, which is
		// exactly the case being guarded against.
		if !strings.Contains(err.Error(), "temporary build") {
			t.Fatalf("unexpected error: %v", err)
		}
		return
	}
	if !filepath.IsAbs(path) {
		t.Errorf("ResolveBinary = %q, want an absolute path", path)
	}
}

func TestPathsAreUnderTheUsersHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory here")
	}
	for _, mgr := range []Manager{launchd{}, systemd{}} {
		path, err := mgr.Path()
		if err != nil {
			t.Fatalf("%s: %v", mgr.Name(), err)
		}
		if !strings.HasPrefix(path, home) {
			t.Errorf("%s path = %q, want it under %q", mgr.Name(), path, home)
		}
	}
}

// recordCommands substitutes the command runner for the length of a test, so
// the invocations can be checked without a service being installed on whatever
// machine is running them.
func recordCommands(t *testing.T) *[]string {
	t.Helper()
	var issued []string
	saved := runner
	runner = func(name string, args ...string) error {
		issued = append(issued, name+" "+strings.Join(args, " "))
		return nil
	}
	t.Cleanup(func() { runner = saved })
	return &issued
}

// withHome points the managers at a scratch home, so an install writes there.
func withHome(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("these managers are for macOS and Linux")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	return home
}

func TestLaunchdInstallWritesAndLoads(t *testing.T) {
	withHome(t)
	issued := recordCommands(t)

	cfg := testConfig()
	cfg.LogDir = t.TempDir()
	if err := (launchd{}).Install(cfg); err != nil {
		t.Fatalf("Install: %v", err)
	}

	path, _ := launchd{}.Path()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the plist was not written: %v", err)
	}

	joined := strings.Join(*issued, "\n")
	if !strings.Contains(joined, "launchctl bootstrap gui/") {
		t.Errorf("commands were:\n%s\nwant a bootstrap", joined)
	}
	if !strings.Contains(joined, path) {
		t.Errorf("commands were:\n%s\nwant the plist path", joined)
	}
	// Replacing a loaded agent means booting it out first, or bootstrap fails
	// with "service already loaded".
	if !strings.Contains(joined, "launchctl bootout") {
		t.Errorf("commands were:\n%s\nwant the old one removed first", joined)
	}
}

func TestLaunchdUninstallRemovesBoth(t *testing.T) {
	withHome(t)
	path, _ := launchd{}.Path()
	if err := writeFile(path, "<plist/>"); err != nil {
		t.Fatal(err)
	}
	issued := recordCommands(t)

	if err := (launchd{}).Uninstall(); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the plist survived uninstall: %v", err)
	}
	if !strings.Contains(strings.Join(*issued, "\n"), "launchctl bootout gui/") {
		t.Errorf("commands were %v, want a bootout", *issued)
	}
}

func TestSystemdInstallEnablesTheUnit(t *testing.T) {
	withHome(t)
	issued := recordCommands(t)

	if err := (systemd{}).Install(testConfig()); err != nil {
		t.Fatalf("Install: %v", err)
	}

	path, _ := systemd{}.Path()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the unit was not written: %v", err)
	}

	joined := strings.Join(*issued, "\n")
	// daemon-reload has to come first, or systemd enables the version it had
	// already read.
	if !strings.HasPrefix(joined, "systemctl --user daemon-reload") {
		t.Errorf("commands were:\n%s\nwant daemon-reload first", joined)
	}
	if !strings.Contains(joined, "systemctl --user enable --now "+SystemdUnit) {
		t.Errorf("commands were:\n%s\nwant it enabled and started", joined)
	}
}

func TestSystemdUninstallDisablesAndReloads(t *testing.T) {
	withHome(t)
	path, _ := systemd{}.Path()
	if err := writeFile(path, "[Unit]\n"); err != nil {
		t.Fatal(err)
	}
	issued := recordCommands(t)

	if err := (systemd{}).Uninstall(); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the unit file survived uninstall: %v", err)
	}
	joined := strings.Join(*issued, "\n")
	for _, want := range []string{"disable --now", "daemon-reload"} {
		if !strings.Contains(joined, want) {
			t.Errorf("commands were:\n%s\nwant %q", joined, want)
		}
	}
}

// Uninstalling something that was never installed is what a second uninstall
// does, and it should not be an error.
func TestUninstallIsIdempotent(t *testing.T) {
	withHome(t)
	recordCommands(t)
	for _, mgr := range []Manager{launchd{}, systemd{}} {
		if err := mgr.Uninstall(); err != nil {
			t.Errorf("%s: uninstalling nothing failed: %v", mgr.Name(), err)
		}
	}
}
