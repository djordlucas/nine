package main

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

// rule is a single security check: if re matches the command, it is blocked.
type rule struct {
	re     *regexp.Regexp
	reason string
}

func re(pattern string) *regexp.Regexp { return regexp.MustCompile(pattern) }

// blocked is the list of command patterns Nine refuses to run by default.
// Set NINE_SHELL_UNSAFE=1 to bypass all checks.
var blocked = []rule{
	// ── deletion ─────────────────────────────────────────────────────────────
	{re(`(?i)\brm\b[^;|&\n]*-[a-zA-Z]*[rR]`), "recursive deletion (rm -r / rm -rf)"},
	{re(`(?i)\brm\b[^;|&\n]*--recursive`), "recursive deletion (rm --recursive)"},
	{re(`(?i)\bshred\b`), "secure file deletion (shred)"},

	// ── raw disk / filesystem ────────────────────────────────────────────────
	{re(`(?i)\bdd\b[^;|&\n]*\bof=/dev/`), "raw device write (dd of=/dev/)"},
	{re(`(?i)\bmkfs\b`), "filesystem formatting (mkfs)"},
	{re(`(?i)\bfdisk\b`), "disk partitioning (fdisk)"},
	{re(`(?i)\bparted\b`), "disk partitioning (parted)"},
	{re(`(?i)\bdiskutil\s+(erase|partition|applymap)`), "destructive disk operation (diskutil)"},

	// ── system control ───────────────────────────────────────────────────────
	{re(`(?i)\b(shutdown|reboot|halt|poweroff)\b`), "system shutdown/reboot"},
	{re(`(?i)\binit\s+[06]\b`), "system runlevel change (init 0/6)"},
	{re(`(?i)\bsystemctl\s+(stop|disable|mask)\b`), "service stop/disable (systemctl)"},

	// ── privilege escalation ─────────────────────────────────────────────────
	{re(`(?i)\bsudo\b`), "privilege escalation (sudo)"},
	{re(`(?i)\bsu\s`), "privilege escalation (su)"},

	// ── arbitrary code execution via pipe ────────────────────────────────────
	{re(`\|\s*(bash|sh|zsh|fish|ksh|csh|tcsh)\b`), "piping into a shell interpreter"},
	{re(`(?i)\bbash\s*<\s*\(`), "process substitution into bash"},

	// ── destructive git operations ───────────────────────────────────────────
	{re(`(?i)\bgit\b[^;|&\n]*(push\b[^;|&\n]*(--force|-f\b)|--force[^;|&\n]*push)`), "force git push"},
	{re(`(?i)\bgit\s+reset\b[^;|&\n]*--hard`), "hard git reset (destructive)"},
	{re(`(?i)\bgit\s+clean\b[^;|&\n]*-[a-zA-Z]*f`), "git clean -f (removes untracked files)"},

	// ── writing to critical system paths ─────────────────────────────────────
	{re(`>\s*/etc/(passwd|shadow|sudoers|crontab|hosts|fstab)`), "overwriting a critical system file"},
	{re(`>\s*/dev/[a-zA-Z]`), "writing directly to a device node"},
}

// checkCommand returns an error if cmd matches any blocked pattern and the
// NINE_SHELL_UNSAFE environment variable is not set to "1".
func checkCommand(cmd string) error {
	if os.Getenv("NINE_SHELL_UNSAFE") == "1" {
		return nil
	}
	// Normalise whitespace so patterns don't need to handle tabs etc.
	cmd = strings.Join(strings.Fields(cmd), " ")
	for _, r := range blocked {
		if r.re.MatchString(cmd) {
			return fmt.Errorf(
				"blocked: %s\ncommand: %s\nset NINE_SHELL_UNSAFE=1 to override",
				r.reason, cmd,
			)
		}
	}
	return nil
}
