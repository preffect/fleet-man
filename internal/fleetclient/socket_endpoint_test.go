package fleetclient

import "testing"

// TestSelectEndpointSocket verifies FLEET_SOCKET selects a non-local
// socketEndpoint that dials the given unix path (trimmed) with no token.
func TestSelectEndpointSocket(t *testing.T) {
	t.Setenv("FLEET_GATEWAY", "")
	t.Setenv("FLEET_SERVER", "")
	t.Setenv("FLEET_SOCKET", "  /tmp/fleet-remote.sock  ")

	ep, err := selectEndpoint()
	if err != nil {
		t.Fatalf("selectEndpoint: %v", err)
	}
	se, ok := ep.(socketEndpoint)
	if !ok {
		t.Fatalf("FLEET_SOCKET should select a socketEndpoint, got %T", ep)
	}
	if se.socket != "/tmp/fleet-remote.sock" {
		t.Fatalf("socket = %q, want trimmed /tmp/fleet-remote.sock", se.socket)
	}
	if ep.IsLocal() {
		t.Fatal("socket endpoint must not be local (no auto-spawn — the daemon is on the far end of the forward)")
	}
	if got, want := ep.Target(), "unix:///tmp/fleet-remote.sock"; got != want {
		t.Fatalf("Target() = %q, want %q", got, want)
	}
	if got, want := ep.String(), "socket:/tmp/fleet-remote.sock"; got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}

// TestSelectEndpointGatewayAndServerBeatSocket verifies precedence:
// FLEET_GATEWAY > FLEET_SERVER > FLEET_SOCKET.
func TestSelectEndpointGatewayAndServerBeatSocket(t *testing.T) {
	t.Setenv("FLEET_SOCKET", "/tmp/fleet-remote.sock")

	// FLEET_SERVER beats FLEET_SOCKET.
	t.Setenv("FLEET_GATEWAY", "")
	t.Setenv("FLEET_SERVER", "1.2.3.4:9000")
	ep, err := selectEndpoint()
	if err != nil {
		t.Fatalf("selectEndpoint: %v", err)
	}
	if _, ok := ep.(remoteEndpoint); !ok {
		t.Fatalf("FLEET_SERVER should win over FLEET_SOCKET, got %T", ep)
	}

	// FLEET_GATEWAY beats both.
	t.Setenv("FLEET_GATEWAY", "https://gw.example.com:50051/abc123")
	t.Setenv("FLEET_TOKEN", "tok")
	ep, err = selectEndpoint()
	if err != nil {
		t.Fatalf("selectEndpoint: %v", err)
	}
	if _, ok := ep.(gatewayEndpoint); !ok {
		t.Fatalf("FLEET_GATEWAY should win over FLEET_SOCKET, got %T", ep)
	}
}

// TestIsRemoteSocket verifies FLEET_SOCKET counts as a remote endpoint so the
// client-host carve-outs (dep checks, the local-exec TTY path) are skipped.
func TestIsRemoteSocket(t *testing.T) {
	t.Setenv("FLEET_GATEWAY", "")
	t.Setenv("FLEET_SERVER", "")

	t.Setenv("FLEET_SOCKET", "")
	if IsRemote() {
		t.Fatal("no remote env set: IsRemote() should be false")
	}
	t.Setenv("FLEET_SOCKET", "/tmp/fleet-remote.sock")
	if !IsRemote() {
		t.Fatal("FLEET_SOCKET set: IsRemote() should be true")
	}
	if IsGateway() {
		t.Fatal("FLEET_SOCKET is not a gateway: IsGateway() should be false")
	}
}
