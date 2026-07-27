package fleetclient

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/fleetpaths"
)

// ssh_tunnel.go implements the FLEET_SSH convenience endpoint: given an
// ssh://[user@]host[:port][/abs/remote/socket] URL, it forwards the remote
// daemon's unix socket to a local path and hands selectEndpoint a socketEndpoint
// pointing at it — so the whole client (TUI, browser proxy, exec) drives the
// remote daemon over an SSH tunnel with no gateway and no bearer token.
//
// It shells out to the system `ssh` binary ON PURPOSE: that inherits every auth
// method the user already has — a key (with ~/.ssh/config IdentityFile or the
// default keys), an interactive password/passphrase prompt (ssh reads it from
// /dev/tty), and the ssh-agent (SSH_AUTH_SOCK) — plus known_hosts and ProxyJump.
// Reimplementing auth in x/crypto/ssh would support fewer of these.
//
// An SSH ControlMaster (a persistent multiplexed connection) means the user
// authenticates ONCE: the first invocation prompts (if a password/passphrase is
// needed), later `fleet` commands reuse the master over its control socket with
// no re-prompt. The forwarded socket lives as long as the master (ControlPersist
// reaps it after an idle window).

// sshSpec is a parsed FLEET_SSH URL.
type sshSpec struct {
	user         string // "" → ssh picks (config / current user)
	host         string
	port         string // "" → ssh default (22)
	remoteSocket string // "" → auto-resolved to the remote's ~/.fleet/fleet.sock
}

// parseSSHURL parses ssh://[user@]host[:port][/abs/remote/socket]. The path,
// when present, is the ABSOLUTE remote socket path (sshd does not expand ~ for
// StreamLocal forwards, so a relative/~ path cannot be forwarded — we resolve
// the default path on the remote instead when the path is omitted).
func parseSSHURL(raw string) (sshSpec, error) {
	var s sshSpec
	u, err := url.Parse(raw)
	if err != nil {
		return s, fmt.Errorf("FLEET_SSH: invalid URL %q: %w", raw, err)
	}
	if u.Scheme != "ssh" {
		return s, fmt.Errorf("FLEET_SSH must be an ssh:// URL, got %q", raw)
	}
	s.host = u.Hostname()
	if s.host == "" {
		return s, fmt.Errorf("FLEET_SSH: missing host in %q", raw)
	}
	s.port = u.Port()
	if u.User != nil {
		s.user = u.User.Username()
	}
	if u.Path != "" && u.Path != "/" {
		s.remoteSocket = u.Path
	}
	return s, nil
}

// target is the [user@]host argument passed to ssh.
func (s sshSpec) target() string {
	if s.user != "" {
		return s.user + "@" + s.host
	}
	return s.host
}

// hash is a stable short id for this spec, used to name the per-target local
// socket and control-socket files so repeated invocations reuse one tunnel.
func (s sshSpec) hash() string {
	sum := sha256.Sum256([]byte(strings.Join([]string{s.user, s.host, s.port, s.remoteSocket}, "|")))
	return hex.EncodeToString(sum[:])[:8]
}

// defaultRemoteSocketCmd resolves the daemon's socket path on the remote when
// the URL omits it. Run as the remote command; $HOME is expanded by the REMOTE
// shell (it is passed to ssh as a single literal arg, never expanded locally).
const defaultRemoteSocketCmd = `printf %s "$HOME/.fleet/fleet.sock"`

// sshCommonOpts are the -o options shared by every ssh invocation for a spec:
// the ControlMaster (so one authenticated connection is multiplexed and
// persisted), keepalives (so a long-lived TUI tunnel isn't dropped), and the
// stale-socket unbind (so a leftover local socket file doesn't block the bind).
func sshCommonOpts(s sshSpec, ctlPath string) []string {
	opts := []string{
		"-o", "ControlMaster=auto",
		"-o", "ControlPath=" + ctlPath,
		"-o", "ControlPersist=300",
		"-o", "StreamLocalBindUnlink=yes",
		"-o", "ServerAliveInterval=30",
		"-o", "ServerAliveCountMax=3",
		"-o", "ConnectTimeout=15",
	}
	if s.port != "" {
		opts = append(opts, "-p", s.port)
	}
	return opts
}

// sshResolveArgs runs the remote path-resolver over (and, on first use,
// establishing) the ControlMaster. It authenticates when no master exists yet.
func sshResolveArgs(s sshSpec, ctlPath string) []string {
	args := sshCommonOpts(s, ctlPath)
	return append(args, s.target(), defaultRemoteSocketCmd)
}

// sshForwardArgs establishes the -L unix-socket forward in the background
// (-f -N). If a master already exists (from the resolve step) it multiplexes
// with no re-auth; otherwise ControlMaster=auto makes this the master and it
// authenticates here. -f returns only once auth + the forward have succeeded;
// ExitOnForwardFailure makes a failed forward a non-zero exit rather than a
// silently-connected session with no tunnel.
func sshForwardArgs(s sshSpec, ctlPath, localSocket, remoteSocket string) []string {
	args := []string{"-f", "-N", "-o", "ExitOnForwardFailure=yes"}
	args = append(args, sshCommonOpts(s, ctlPath)...)
	args = append(args, "-L", localSocket+":"+remoteSocket, s.target())
	return args
}

// sshExitArgs tears the master (and its forward) down.
func sshExitArgs(s sshSpec, ctlPath string) []string {
	return []string{"-O", "exit", "-o", "ControlPath=" + ctlPath, s.target()}
}

var sshTunnelMu sync.Mutex

// ensureSSHTunnel makes sure a local unix socket forwarded to the remote daemon
// is up for spec, and returns its path. It is idempotent and cheap on the steady
// state: if the forwarded socket already accepts connections (a master from this
// or a prior invocation is still alive), it returns immediately without spawning
// ssh. Concurrent callers are serialized so two Dials can't race two masters.
func ensureSSHTunnel(s sshSpec) (string, error) {
	sshTunnelMu.Lock()
	defer sshTunnelMu.Unlock()

	dir := filepath.Join(fleetpaths.Dir(), "ssh")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("FLEET_SSH: create tunnel dir: %w", err)
	}
	h := s.hash()
	localSocket := filepath.Join(dir, h+".sock")
	ctlPath := filepath.Join(dir, h+".ctl")

	// Fast path: the forward is already up (reused across `fleet` invocations
	// while ControlPersist keeps the master alive).
	if socketAccepts(localSocket, 300*time.Millisecond) {
		return localSocket, nil
	}

	remote := s.remoteSocket
	if remote == "" {
		// Authenticate + establish the master + learn the remote socket path in
		// one shot. ssh prompts on /dev/tty if a password/passphrase is needed,
		// so capturing stdout here does not suppress the prompt.
		cmd := exec.Command("ssh", sshResolveArgs(s, ctlPath)...)
		cmd.Stderr = os.Stderr
		out, err := cmd.Output()
		if err != nil {
			return "", fmt.Errorf("FLEET_SSH: connect to %s: %w", s.target(), err)
		}
		remote = strings.TrimSpace(string(out))
		if remote == "" {
			return "", fmt.Errorf("FLEET_SSH: could not resolve the remote daemon socket on %s (is fleetd installed there?)", s.target())
		}
	}

	// Bring up the forward (auth here too if the resolve step was skipped
	// because the path was given explicitly). Inherit stdio so any prompt is
	// visible and answerable.
	fwd := exec.Command("ssh", sshForwardArgs(s, ctlPath, localSocket, remote)...)
	fwd.Stdin, fwd.Stdout, fwd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := fwd.Run(); err != nil {
		return "", fmt.Errorf("FLEET_SSH: forward %s from %s: %w", remote, s.target(), err)
	}

	// -f returns after the forward is set up, but give the socket a brief moment
	// to start accepting (it is created as the forward is wired).
	if !waitSocketAccepts(localSocket, 6*time.Second) {
		return "", fmt.Errorf("FLEET_SSH: forwarded socket %s never came up", localSocket)
	}
	return localSocket, nil
}

// socketAccepts reports whether a unix socket at path accepts a connection
// within timeout.
func socketAccepts(path string, timeout time.Duration) bool {
	c, err := net.DialTimeout("unix", path, timeout)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// waitSocketAccepts polls socketAccepts until it succeeds or the deadline passes.
func waitSocketAccepts(path string, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for {
		if socketAccepts(path, 200*time.Millisecond) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
}
