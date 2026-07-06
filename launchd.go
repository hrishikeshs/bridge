package main

// launchd.go — `bridge install-daemon`: put the daemon under launchd so it
// survives logout, reboot, and crashes (KeepAlive restarts it), retiring the
// nohup deploy dance whose in-flight permission prompt caused the 2026-07-06
// outage. macOS only; a no-op elsewhere.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"

	"github.com/spf13/cobra"
)

const launchdLabel = "com.hrishikeshs.bridge"

func launchdPlistPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist")
}

// launchdPlist renders the LaunchAgent for the given bridge executable. Two
// environment details are load-bearing: PATH (launchd agents inherit a bare
// environment, but the daemon shells out to tmux, ps, caffeinate, tailscale,
// which live under Homebrew / system bins) and TMPDIR — the daemon finds the
// running tmux server through its socket under $TMPDIR/tmux-$UID, so a launchd
// agent with a different TMPDIR than the user's shell would see every contact
// as offline. tmpdir is captured from the installing shell's environment; when
// empty the entry is omitted (launchd's per-user default usually matches).
func launchdPlist(exe, outLog, errLog, tmpdir string) string {
	tmpEnv := ""
	if tmpdir != "" {
		tmpEnv = fmt.Sprintf("\n    <key>TMPDIR</key><string>%s</string>", tmpdir)
	}
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>%s</string>
  <key>ProgramArguments</key>
  <array>
    <string>%s</string>
    <string>serve</string>
  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>ProcessType</key><string>Interactive</string>
  <key>StandardOutPath</key><string>%s</string>
  <key>StandardErrorPath</key><string>%s</string>
  <key>EnvironmentVariables</key>
  <dict>
    <key>PATH</key><string>/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>%s
  </dict>
</dict>
</plist>
`, launchdLabel, exe, outLog, errLog, tmpEnv)
}

// installDaemonCmd writes the LaunchAgent and (re)loads it under launchd.
func installDaemonCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "install-daemon",
		Short: "Supervise the daemon with launchd (survives reboot + crash)",
		Long: `Install the bridge daemon as a launchd LaunchAgent, so macOS keeps it
running: it starts at login, restarts if it crashes, and survives a reboot —
retiring the manual 'nohup bridge serve' dance.

After this, redeploy a new build with:
  launchctl kickstart -k gui/$(id -u)/` + launchdLabel + `

Stop supervising with: bridge install-daemon --uninstall`,
		RunE: func(cmd *cobra.Command, args []string) error {
			uninstall, _ := cmd.Flags().GetBool("uninstall")
			return runInstallDaemon(uninstall)
		},
	}
	cmd.Flags().Bool("uninstall", false, "remove launchd supervision")
	return cmd
}

func runInstallDaemon(uninstall bool) error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("install-daemon is macOS-only (launchd); on %s run `bridge serve` under your own supervisor", runtime.GOOS)
	}
	uid := strconv.Itoa(os.Getuid())
	domainTarget := "gui/" + uid
	serviceTarget := domainTarget + "/" + launchdLabel
	plist := launchdPlistPath()

	if uninstall {
		_ = exec.Command("launchctl", "bootout", serviceTarget).Run()
		if err := os.Remove(plist); err != nil && !os.IsNotExist(err) {
			return err
		}
		fmt.Println("bridge: launchd supervision removed. Start manually with `bridge serve` if you like.")
		return nil
	}

	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(plist), 0o755); err != nil {
		return err
	}
	logDir := bridgePath("logs")
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		return err
	}
	outLog := filepath.Join(logDir, "daemon.out.log")
	errLog := filepath.Join(logDir, "daemon.err.log")
	// Capture the installing shell's TMPDIR so the supervised daemon shares the
	// user's tmux socket (see launchdPlist) — the difference between a clean
	// cutover and every contact reading offline.
	if err := os.WriteFile(plist, []byte(launchdPlist(exe, outLog, errLog, os.Getenv("TMPDIR"))), 0o644); err != nil {
		return err
	}

	// Reload cleanly: bootout an old instance (ignore "not loaded"), then
	// bootstrap the fresh plist and kickstart it so it starts immediately
	// rather than only at the next login.
	_ = exec.Command("launchctl", "bootout", serviceTarget).Run()
	if out, err := exec.Command("launchctl", "bootstrap", domainTarget, plist).CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl bootstrap failed: %v: %s", err, out)
	}
	_ = exec.Command("launchctl", "kickstart", "-k", serviceTarget).Run()

	fmt.Printf("bridge: now supervised by launchd (%s).\n", launchdLabel)
	fmt.Printf("  plist:  %s\n", plist)
	fmt.Printf("  logs:   %s\n  redeploy a new build: launchctl kickstart -k %s\n", logDir, serviceTarget)
	return nil
}
