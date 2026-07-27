package tui

import (
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// TestSaveAddFleetLocalFolder verifies that an ABSOLUTE path in the new-fleet
// dialog registers a local-folder fleet (empty remote, SourcePath set) directly
// — no clone-inspect — and persists it via the RPC seam.
func TestSaveAddFleetLocalFolder(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	fp := newFleetPage()
	m := &model{
		st:           &state.State{Fleets: map[string]*fleet.Fleet{}},
		fleetPage:    fp,
		sessionStore: NewSessionStore(),
	}

	var gotName, gotRemote, gotSource string
	orig := createFleetRemote
	createFleetRemote = func(name, remote, sourcePath string) error {
		gotName, gotRemote, gotSource = name, remote, sourcePath
		return nil
	}
	defer func() { createFleetRemote = orig }()

	fp.mode = viewAddFleet
	fp.textInput.SetValue("/home/dev/my-project")
	_ = fp.saveAddFleet(m)

	// Registered in the local model as a local-folder fleet.
	f := m.st.Fleets["my-project"]
	if f == nil {
		t.Fatal("local-folder fleet was not registered")
	}
	if f.Remote != "" || f.SourcePath != "/home/dev/my-project" {
		t.Fatalf("fleet remote=%q source=%q, want remote empty + source /home/dev/my-project", f.Remote, f.SourcePath)
	}

	// Persisted via the RPC seam with the folder path and no remote.
	if gotName != "my-project" || gotRemote != "" || gotSource != "/home/dev/my-project" {
		t.Fatalf("createFleetRemote(name=%q remote=%q source=%q)", gotName, gotRemote, gotSource)
	}

	// No inspection: a local folder is registered immediately (back to normal).
	if fp.mode != viewNormal {
		t.Fatalf("mode = %v, want viewNormal (local folder is not inspected)", fp.mode)
	}
}

// TestSaveAddFleetRemoteStillInspects verifies a non-absolute input is still
// treated as a git URL and enters the inspecting state (not registered yet).
func TestSaveAddFleetRemoteStillInspects(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	fp := newFleetPage()
	m := &model{
		st:           &state.State{Fleets: map[string]*fleet.Fleet{}},
		fleetPage:    fp,
		sessionStore: NewSessionStore(),
	}

	fp.mode = viewAddFleet
	fp.textInput.SetValue("git@github.com:org/my-project.git")
	_ = fp.saveAddFleet(m)

	if _, ok := m.st.Fleets["my-project"]; ok {
		t.Fatal("git fleet must not be registered before inspection completes")
	}
	if fp.mode != viewAddFleetInspecting {
		t.Fatalf("mode = %v, want viewAddFleetInspecting", fp.mode)
	}
}
