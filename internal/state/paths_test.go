package state

import (
	"path/filepath"
	"testing"
)

// TestIsManagedWorkspace is the safety invariant behind the destroy() gate:
// only paths INSIDE the managed workspaces tree may be os.RemoveAll'd, so an
// in-place "local folder" workspace (the user's real project) is never deleted.
func TestIsManagedWorkspace(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	ws := WorkspacesDir()

	cases := []struct {
		name string
		path string
		want bool
	}{
		{"empty is not managed", "", false},
		{"workspaces root itself is not managed", ws, false},
		{"a real per-instance workspace is managed", filepath.Join(ws, "myfleet", "agent-1", "myfleet"), true},
		{"a fleet subdir is managed", filepath.Join(ws, "myfleet"), true},
		{"an external in-place folder is not managed", filepath.Join(home, "projects", "myapp"), false},
		{"an unrelated absolute path is not managed", "/etc", false},
		{"a path escaping the tree via .. is not managed", filepath.Join(ws, "..", "not-workspaces"), false},
	}
	for _, c := range cases {
		if got := IsManagedWorkspace(c.path); got != c.want {
			t.Errorf("%s: IsManagedWorkspace(%q) = %v, want %v", c.name, c.path, got, c.want)
		}
	}
}
