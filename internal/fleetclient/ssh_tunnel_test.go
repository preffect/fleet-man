package fleetclient

import (
	"slices"
	"strings"
	"testing"
)

func TestParseSSHURL(t *testing.T) {
	cases := []struct {
		raw     string
		want    sshSpec
		wantErr bool
	}{
		{raw: "ssh://user@host", want: sshSpec{user: "user", host: "host"}},
		{raw: "ssh://host", want: sshSpec{host: "host"}},
		{raw: "ssh://user@host:2222", want: sshSpec{user: "user", host: "host", port: "2222"}},
		{raw: "ssh://user@host/home/dev/.fleet/fleet.sock", want: sshSpec{user: "user", host: "host", remoteSocket: "/home/dev/.fleet/fleet.sock"}},
		{raw: "ssh://user@host:22/var/run/fleet.sock", want: sshSpec{user: "user", host: "host", port: "22", remoteSocket: "/var/run/fleet.sock"}},
		{raw: "ssh://host/", want: sshSpec{host: "host"}}, // bare slash → no explicit path
		{raw: "https://host/id", wantErr: true},           // wrong scheme
		{raw: "ssh://", wantErr: true},                    // no host
		{raw: "ssh:///only/path", wantErr: true},          // no host
	}
	for _, c := range cases {
		got, err := parseSSHURL(c.raw)
		if c.wantErr {
			if err == nil {
				t.Errorf("%q: want error, got %+v", c.raw, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: unexpected error %v", c.raw, err)
			continue
		}
		if got != c.want {
			t.Errorf("%q: got %+v, want %+v", c.raw, got, c.want)
		}
	}
}

func TestSSHSpecTargetAndHash(t *testing.T) {
	if got := (sshSpec{user: "u", host: "h"}).target(); got != "u@h" {
		t.Errorf("target with user = %q, want u@h", got)
	}
	if got := (sshSpec{host: "h"}).target(); got != "h" {
		t.Errorf("target without user = %q, want h", got)
	}
	// The hash is stable and distinguishes specs that differ in any field.
	a := sshSpec{user: "u", host: "h", port: "22"}
	if a.hash() != a.hash() {
		t.Error("hash must be stable")
	}
	if a.hash() == (sshSpec{user: "u", host: "h", port: "2222"}).hash() {
		t.Error("hash must differ when the port differs")
	}
	if a.hash() == (sshSpec{user: "u2", host: "h", port: "22"}).hash() {
		t.Error("hash must differ when the user differs")
	}
}

// TestSSHArgsShape verifies the ssh command lines carry the multiplexing +
// keepalive options and the right positional args, so a regression in flag
// construction is caught without a live remote.
func TestSSHArgsShape(t *testing.T) {
	s := sshSpec{user: "dev", host: "box", port: "2222"}
	ctl := "/tmp/x.ctl"

	must := func(name string, args []string, wants ...string) {
		joined := strings.Join(args, " ")
		for _, w := range wants {
			if !strings.Contains(joined, w) {
				t.Errorf("%s args %v missing %q", name, args, w)
			}
		}
	}

	resolve := sshResolveArgs(s, ctl)
	must("resolve", resolve, "ControlMaster=auto", "ControlPath="+ctl, "ControlPersist=300", "-p", "2222", "dev@box", defaultRemoteSocketCmd)

	fwd := sshForwardArgs(s, ctl, "/tmp/local.sock", "/remote/fleet.sock")
	must("forward", fwd, "-f", "-N", "ExitOnForwardFailure=yes", "StreamLocalBindUnlink=yes", "-L", "/tmp/local.sock:/remote/fleet.sock", "dev@box")
	// -f/-N precede the target so ssh treats the remainder as connection setup, not a remote command.
	if i, j := slices.Index(fwd, "-L"), slices.Index(fwd, "dev@box"); i < 0 || j < 0 || i > j {
		t.Errorf("forward args malformed (want -L before target): %v", fwd)
	}

	exit := sshExitArgs(s, ctl)
	must("exit", exit, "-O", "exit", "ControlPath="+ctl, "dev@box")

	// A spec without a port must not emit a -p flag.
	if slices.Contains(sshForwardArgs(sshSpec{host: "box"}, ctl, "/l", "/r"), "-p") {
		t.Error("no port should not produce a -p flag")
	}
}

// TestSelectEndpointSSHPrecedence verifies FLEET_SOCKET (an explicit path) wins
// over FLEET_SSH, and that FLEET_SSH counts as remote.
func TestSelectEndpointSSHPrecedence(t *testing.T) {
	t.Setenv("FLEET_GATEWAY", "")
	t.Setenv("FLEET_SERVER", "")
	t.Setenv("FLEET_SSH", "ssh://user@box")

	t.Setenv("FLEET_SOCKET", "/tmp/explicit.sock")
	ep, err := selectEndpoint()
	if err != nil {
		t.Fatalf("selectEndpoint: %v", err)
	}
	if se, ok := ep.(socketEndpoint); !ok || se.socket != "/tmp/explicit.sock" {
		t.Fatalf("FLEET_SOCKET should win over FLEET_SSH, got %T %+v", ep, ep)
	}

	t.Setenv("FLEET_SOCKET", "")
	if !IsRemote() {
		t.Fatal("FLEET_SSH set: IsRemote() should be true")
	}
	if IsGateway() {
		t.Fatal("FLEET_SSH is not a gateway")
	}
}
