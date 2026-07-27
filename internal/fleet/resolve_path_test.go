package fleet

import "testing"

func TestFleetNameFromPath(t *testing.T) {
	cases := map[string]string{
		"/home/dev/my-project":  "my-project",
		"/home/dev/my-project/": "my-project",
		"/srv/app":              "app",
		"relative/dir":          "dir",
		"":                      "",
		"/":                     "",
		".":                     "",
	}
	for in, want := range cases {
		if got := FleetNameFromPath(in); got != want {
			t.Errorf("FleetNameFromPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestResolvePath(t *testing.T) {
	// Basename derives the fleet; instance is the bare name.
	tgt, err := ResolvePath("agent-1", "/home/dev/my-project")
	if err != nil {
		t.Fatalf("ResolvePath: %v", err)
	}
	if tgt.Fleet != "my-project" || tgt.Instance != "agent-1" {
		t.Fatalf("got %+v, want fleet=my-project instance=agent-1", tgt)
	}

	// Explicit fleet/instance wins over the path basename.
	tgt, err = ResolvePath("team/agent-2", "/home/dev/my-project")
	if err != nil {
		t.Fatalf("ResolvePath explicit: %v", err)
	}
	if tgt.Fleet != "team" || tgt.Instance != "agent-2" {
		t.Fatalf("got %+v, want fleet=team instance=agent-2", tgt)
	}

	// A path with no derivable basename and a bare name is an error.
	if _, err := ResolvePath("agent-1", "/"); err == nil {
		t.Fatal("ResolvePath with underivable path should error")
	}
}
