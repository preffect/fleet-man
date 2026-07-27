package tui

import "testing"

// TestAvailableBackendTypesRemoteIgnoresLocalTools verifies that when the TUI is
// pointed at a remote daemon, backend availability is NOT gated on the client's
// local tool probe (the tools live on the daemon). A laptop with no CLIs must
// still be offered every backend for a remote fleet — otherwise a local-folder
// fleet (which requires devcontainer) is impossible to create from a laptop that
// lacks the devcontainer CLI.
func TestAvailableBackendTypesRemoteIgnoresLocalTools(t *testing.T) {
	fp := newFleetPage()
	m := &model{toolStatus: nil} // nothing detected on the client host

	// Local daemon: with no tools detected, nothing is offered (every backend
	// needs a CLI).
	t.Setenv("FLEET_GATEWAY", "")
	t.Setenv("FLEET_SERVER", "")
	t.Setenv("FLEET_SOCKET", "")
	t.Setenv("FLEET_SSH", "")
	if got := fp.availableBackendTypes(m); len(got) != 0 {
		t.Fatalf("local + no tools: want no backends, got %v", got)
	}

	// Remote daemon: every backend is offered regardless of local tools.
	t.Setenv("FLEET_SERVER", "10.0.0.5:50051")
	got := fp.availableBackendTypes(m)
	if len(got) != len(allBackendTypes) {
		t.Fatalf("remote: want all %d backends, got %v", len(allBackendTypes), got)
	}
	if got[0] != allBackendTypes[0] { // devcontainer first → sensible default
		t.Fatalf("remote: first backend = %v, want %v", got[0], allBackendTypes[0])
	}
}
