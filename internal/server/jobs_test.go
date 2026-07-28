package server

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

// newTestServer stands up the service on an in-process bufconn with the hub
// running, returning a connected client and a cleanup func.
func newTestServer(t *testing.T) (*service, fleetgrpc.FleetServiceClient, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	svc := newService()
	go svc.hub.run(ctx)

	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer()
	fleetgrpc.RegisterFleetServiceServer(gs, svc)
	go func() { _ = gs.Serve(lis) }()

	conn, err := grpc.NewClient(
		"passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	cleanup := func() {
		_ = conn.Close()
		gs.Stop()
		cancel()
	}
	return svc, fleetgrpc.NewFleetServiceClient(conn), cleanup
}

// drainJob reads a job stream to its JobDone, returning the ordered events.
func drainJob(t *testing.T, stream grpc.ServerStreamingClient[fleetgrpc.JobEvent]) []*fleetgrpc.JobEvent {
	t.Helper()
	var evs []*fleetgrpc.JobEvent
	for {
		ev, err := stream.Recv()
		if err == io.EOF {
			return evs
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		evs = append(evs, ev)
		if ev.GetDone() != nil {
			return evs
		}
	}
}

func TestCreateInstanceJobStartedThenDone(t *testing.T) {
	isolateFleetDir(t)
	if err := state.Save(&state.State{Fleets: map[string]*fleet.Fleet{
		"alpha": {Name: "alpha", Remote: "git@x:a.git"},
	}}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	orig := jobRunCreate
	jobRunCreate = func(fleetName, instanceName, remote, branch string, verbose bool, b fleet.BackendType, sourcePath string) error {
		if remote != "git@x:a.git" {
			t.Errorf("remote not resolved from fleet record: %q", remote)
		}
		return state.Update(func(st *state.State) error {
			if f, ok := st.Fleets[fleetName]; ok {
				if inst, err := f.GetInstance(instanceName); err == nil {
					inst.Status = fleet.StatusRunning
					inst.ContainerID = "fake-cid"
				}
			}
			return nil
		})
	}
	defer func() { jobRunCreate = orig }()

	_, client, cleanup := newTestServer(t)
	defer cleanup()

	stream, err := client.CreateInstance(context.Background(), &fleetgrpc.CreateInstanceRequest{
		Fleet: "alpha", Instance: "i1", Backend: fleetgrpc.BackendType_BACKEND_TYPE_DEVCONTAINER,
	})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	evs := drainJob(t, stream)

	if len(evs) < 2 {
		t.Fatalf("want >=2 events, got %d", len(evs))
	}
	if evs[0].GetStarted() == nil {
		t.Fatalf("first event must be JobStarted, got %T", evs[0].GetEvent())
	}
	last := evs[len(evs)-1].GetDone()
	if last == nil || !last.GetSuccess() {
		t.Fatalf("last event must be successful JobDone: %v", evs[len(evs)-1])
	}
	if last.GetInstance().GetStatus() != fleetgrpc.InstanceStatus_INSTANCE_STATUS_RUNNING {
		t.Fatalf("JobDone instance not RUNNING: %v", last.GetInstance())
	}

	st, _ := state.Load()
	inst, err := st.Fleets["alpha"].GetInstance("i1")
	if err != nil || inst.Status != fleet.StatusRunning || inst.ContainerID != "fake-cid" {
		t.Fatalf("persisted record wrong: %+v err=%v", inst, err)
	}
}

// TestCreateInstanceLocalFolder verifies a --path (local folder) create: no
// remote is required, the workspace IS the in-place folder, source is persisted
// on both fleet and instance, and a second instance in the same local-folder
// fleet is rejected (one-instance-per-folder rule).
func TestCreateInstanceLocalFolder(t *testing.T) {
	isolateFleetDir(t)

	var gotSource, gotRemote string
	orig := jobRunCreate
	jobRunCreate = func(fleetName, instanceName, remote, branch string, verbose bool, b fleet.BackendType, sourcePath string) error {
		gotRemote, gotSource = remote, sourcePath
		return state.Update(func(st *state.State) error {
			if f, ok := st.Fleets[fleetName]; ok {
				if inst, err := f.GetInstance(instanceName); err == nil {
					inst.Status = fleet.StatusRunning
					inst.ContainerID = "cid"
				}
			}
			return nil
		})
	}
	defer func() { jobRunCreate = orig }()

	_, client, cleanup := newTestServer(t)
	defer cleanup()

	src := "/home/dev/my-project"
	stream, err := client.CreateInstance(context.Background(), &fleetgrpc.CreateInstanceRequest{
		Fleet: "my-project", Instance: "a1",
		Backend:    fleetgrpc.BackendType_BACKEND_TYPE_DEVCONTAINER,
		SourcePath: &src,
	})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	evs := drainJob(t, stream)
	if last := evs[len(evs)-1].GetDone(); last == nil || !last.GetSuccess() {
		t.Fatalf("want successful JobDone: %v", evs[len(evs)-1])
	}

	if gotSource != src {
		t.Errorf("jobRunCreate sourcePath = %q, want %q", gotSource, src)
	}
	if gotRemote != "" {
		t.Errorf("jobRunCreate remote = %q, want empty for a local folder", gotRemote)
	}

	st, _ := state.Load()
	f := st.Fleets["my-project"]
	if f == nil {
		t.Fatal("local-folder fleet not created (no remote provided)")
	}
	if f.Remote != "" || f.SourcePath != src {
		t.Errorf("fleet remote=%q source=%q, want remote empty + source %q", f.Remote, f.SourcePath, src)
	}
	inst, err := f.GetInstance("a1")
	if err != nil {
		t.Fatal(err)
	}
	if inst.WorkspaceDir != src {
		t.Errorf("instance WorkspaceDir = %q, want the in-place folder %q", inst.WorkspaceDir, src)
	}
	if inst.SourcePath != src {
		t.Errorf("instance SourcePath = %q, want %q", inst.SourcePath, src)
	}

	// One-instance-per-local-folder rule: a second instance is rejected.
	stream2, err := client.CreateInstance(context.Background(), &fleetgrpc.CreateInstanceRequest{
		Fleet: "my-project", Instance: "a2",
		Backend:    fleetgrpc.BackendType_BACKEND_TYPE_DEVCONTAINER,
		SourcePath: &src,
	})
	if err == nil {
		_, err = stream2.Recv()
	}
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("second local-folder instance: got %v, want FailedPrecondition", err)
	}
}

// TestCreateInstanceInheritsFleetSourcePath verifies that adding an instance to
// an EXISTING local-folder fleet (registered with a SourcePath but no instances,
// e.g. via the TUI "new fleet from folder") inherits the folder even though the
// request carries no source_path — the in-place-workspace path the TUI 'a' flow
// and `fleet up <name>` both rely on.
func TestCreateInstanceInheritsFleetSourcePath(t *testing.T) {
	isolateFleetDir(t)
	if err := state.Save(&state.State{Fleets: map[string]*fleet.Fleet{
		"my-project": {Name: "my-project", SourcePath: "/home/dev/my-project"},
	}}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var gotSource, gotRemote string
	orig := jobRunCreate
	jobRunCreate = func(fleetName, instanceName, remote, branch string, verbose bool, b fleet.BackendType, sourcePath string) error {
		gotRemote, gotSource = remote, sourcePath
		return nil
	}
	defer func() { jobRunCreate = orig }()

	_, client, cleanup := newTestServer(t)
	defer cleanup()

	// No SourcePath in the request — it must be inherited from the fleet record.
	stream, err := client.CreateInstance(context.Background(), &fleetgrpc.CreateInstanceRequest{
		Fleet: "my-project", Instance: "dev",
		Backend: fleetgrpc.BackendType_BACKEND_TYPE_DEVCONTAINER,
	})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	if last := drainJob(t, stream); last[len(last)-1].GetDone() == nil || !last[len(last)-1].GetDone().GetSuccess() {
		t.Fatalf("want successful JobDone: %v", last[len(last)-1])
	}

	if gotSource != "/home/dev/my-project" {
		t.Errorf("inherited sourcePath = %q, want /home/dev/my-project", gotSource)
	}
	if gotRemote != "" {
		t.Errorf("remote = %q, want empty for an inherited local folder", gotRemote)
	}

	st, _ := state.Load()
	inst, err := st.Fleets["my-project"].GetInstance("dev")
	if err != nil {
		t.Fatal(err)
	}
	if inst.WorkspaceDir != "/home/dev/my-project" || inst.SourcePath != "/home/dev/my-project" {
		t.Errorf("instance workspace=%q source=%q, want the in-place folder", inst.WorkspaceDir, inst.SourcePath)
	}
}

// TestCreateInstanceLocalFolderNormalizesPath guards the trailing-slash bug: a
// source_path like "/x/proj/" must be cleaned before it's stored/used, else the
// devcontainer CLI's normalized "/x/proj" label desyncs pruneStaleContainers and
// stale containers get reused forever.
func TestCreateInstanceLocalFolderNormalizesPath(t *testing.T) {
	isolateFleetDir(t)

	var gotSource string
	orig := jobRunCreate
	jobRunCreate = func(fleetName, instanceName, remote, branch string, verbose bool, b fleet.BackendType, sourcePath string) error {
		gotSource = sourcePath
		return nil
	}
	defer func() { jobRunCreate = orig }()

	_, client, cleanup := newTestServer(t)
	defer cleanup()

	slashed := "/home/dev/my-project/" // trailing slash + should be cleaned
	stream, err := client.CreateInstance(context.Background(), &fleetgrpc.CreateInstanceRequest{
		Fleet: "my-project", Instance: "a1",
		Backend:    fleetgrpc.BackendType_BACKEND_TYPE_DEVCONTAINER,
		SourcePath: &slashed,
	})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	if last := drainJob(t, stream); last[len(last)-1].GetDone() == nil {
		t.Fatalf("no JobDone: %v", last)
	}

	const clean = "/home/dev/my-project"
	if gotSource != clean {
		t.Errorf("jobRunCreate sourcePath = %q, want cleaned %q", gotSource, clean)
	}
	st, _ := state.Load()
	f := st.Fleets["my-project"]
	if f.SourcePath != clean {
		t.Errorf("fleet SourcePath = %q, want cleaned %q", f.SourcePath, clean)
	}
	inst, _ := f.GetInstance("a1")
	if inst.SourcePath != clean || inst.WorkspaceDir != clean {
		t.Errorf("instance source=%q workspace=%q, want cleaned %q", inst.SourcePath, inst.WorkspaceDir, clean)
	}
}

func TestCreateInstanceRejectsDuplicate(t *testing.T) {
	isolateFleetDir(t)
	if err := state.Save(&state.State{Fleets: map[string]*fleet.Fleet{
		"alpha": {Name: "alpha", Remote: "git@x:a.git", Instances: []*fleet.Instance{{Name: "i1"}}},
	}}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, client, cleanup := newTestServer(t)
	defer cleanup()

	stream, err := client.CreateInstance(context.Background(), &fleetgrpc.CreateInstanceRequest{Fleet: "alpha", Instance: "i1"})
	if err != nil {
		t.Fatalf("CreateInstance call: %v", err)
	}
	_, err = stream.Recv()
	if status.Code(err) != codes.AlreadyExists {
		t.Fatalf("want AlreadyExists, got %v", err)
	}
}

func TestCreateInstanceRejectsSpaceInName(t *testing.T) {
	isolateFleetDir(t)
	if err := state.Save(&state.State{Fleets: map[string]*fleet.Fleet{
		"alpha": {Name: "alpha", Remote: "git@x:a.git"},
	}}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, client, cleanup := newTestServer(t)
	defer cleanup()

	stream, err := client.CreateInstance(context.Background(), &fleetgrpc.CreateInstanceRequest{Fleet: "alpha", Instance: "my agent"})
	if err != nil {
		t.Fatalf("CreateInstance call: %v", err)
	}
	_, err = stream.Recv()
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("want InvalidArgument, got %v", err)
	}
}

func TestCloneInstanceRejectsSpaceInName(t *testing.T) {
	isolateFleetDir(t)
	if err := state.Save(&state.State{Fleets: map[string]*fleet.Fleet{
		"alpha": {Name: "alpha", Remote: "git@x:a.git", Instances: []*fleet.Instance{{Name: "i1"}}},
	}}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, client, cleanup := newTestServer(t)
	defer cleanup()

	stream, err := client.CloneInstance(context.Background(), &fleetgrpc.CloneInstanceRequest{Fleet: "alpha", SourceInstance: "i1", NewInstance: "i 2"})
	if err != nil {
		t.Fatalf("CloneInstance call: %v", err)
	}
	_, err = stream.Recv()
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("want InvalidArgument, got %v", err)
	}

	// The optional new display name follows the same rule.
	stream, err = client.CloneInstance(context.Background(), &fleetgrpc.CloneInstanceRequest{Fleet: "alpha", SourceInstance: "i1", NewInstance: "i2", NewDisplayName: ptr("i 2")})
	if err != nil {
		t.Fatalf("CloneInstance call: %v", err)
	}
	_, err = stream.Recv()
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("want InvalidArgument, got %v", err)
	}
}

func TestStartStopJobs(t *testing.T) {
	isolateFleetDir(t)
	if err := state.Save(&state.State{Fleets: map[string]*fleet.Fleet{
		"alpha": {Name: "alpha", Instances: []*fleet.Instance{{Name: "i1", Status: fleet.StatusStopped, ContainerID: "c1"}}},
	}}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	origStart, origStop := jobRunStart, jobRunStop
	jobRunStart = func(f, i string) error {
		return state.Update(func(st *state.State) error {
			inst, _ := st.Fleets[f].GetInstance(i)
			inst.Status = fleet.StatusRunning
			return nil
		})
	}
	jobRunStop = func(f, i string) error {
		return state.Update(func(st *state.State) error {
			inst, _ := st.Fleets[f].GetInstance(i)
			inst.Status = fleet.StatusStopped
			return nil
		})
	}
	defer func() { jobRunStart, jobRunStop = origStart, origStop }()

	_, client, cleanup := newTestServer(t)
	defer cleanup()

	startStream, err := client.StartInstance(context.Background(), &fleetgrpc.StartInstanceRequest{Fleet: "alpha", Instance: "i1"})
	if err != nil {
		t.Fatalf("StartInstance: %v", err)
	}
	evs := drainJob(t, startStream)
	if d := evs[len(evs)-1].GetDone(); d == nil || !d.GetSuccess() {
		t.Fatalf("start job not done-success: %v", evs)
	}
	if st, _ := state.Load(); st.Fleets["alpha"].Instances[0].Status != fleet.StatusRunning {
		t.Fatalf("instance not running after start")
	}

	stopStream, err := client.StopInstance(context.Background(), &fleetgrpc.StopInstanceRequest{Fleet: "alpha", Instance: "i1"})
	if err != nil {
		t.Fatalf("StopInstance: %v", err)
	}
	drainJob(t, stopStream)
	if st, _ := state.Load(); st.Fleets["alpha"].Instances[0].Status != fleet.StatusStopped {
		t.Fatalf("instance not stopped after stop")
	}
}

func TestRebuildInstanceJob(t *testing.T) {
	isolateFleetDir(t)
	if err := state.Save(&state.State{Fleets: map[string]*fleet.Fleet{
		"alpha": {Name: "alpha", Instances: []*fleet.Instance{{Name: "i1", Status: fleet.StatusRunning, ContainerID: "c1"}}},
	}}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	orig := jobRunRebuild
	// Simulate create.RunRebuild's success path: a new container id, running.
	jobRunRebuild = func(f, i string) error {
		return state.Update(func(st *state.State) error {
			inst, _ := st.Fleets[f].GetInstance(i)
			inst.ContainerID = "c2"
			inst.Status = fleet.StatusRunning
			return nil
		})
	}
	defer func() { jobRunRebuild = orig }()

	_, client, cleanup := newTestServer(t)
	defer cleanup()

	stream, err := client.RebuildInstance(context.Background(), &fleetgrpc.RebuildInstanceRequest{Fleet: "alpha", Instance: "i1"})
	if err != nil {
		t.Fatalf("RebuildInstance: %v", err)
	}
	evs := drainJob(t, stream)
	if k := evs[0].GetStarted().GetKind(); k != fleetgrpc.JobKind_JOB_KIND_REBUILD_INSTANCE {
		t.Fatalf("first event kind = %v, want REBUILD_INSTANCE", k)
	}
	if d := evs[len(evs)-1].GetDone(); d == nil || !d.GetSuccess() {
		t.Fatalf("rebuild not done-success: %v", evs)
	}
	st, _ := state.Load()
	inst, _ := st.Fleets["alpha"].GetInstance("i1")
	if inst.Status != fleet.StatusRunning || inst.ContainerID != "c2" {
		t.Fatalf("instance not running with new container after rebuild: %+v", inst)
	}
}

func TestRebuildInstanceRejectsUnsupportedBackend(t *testing.T) {
	isolateFleetDir(t)
	if err := state.Save(&state.State{Fleets: map[string]*fleet.Fleet{
		"alpha": {Name: "alpha", Instances: []*fleet.Instance{
			{Name: "i1", Status: fleet.StatusRunning, ContainerID: "c1", Backend: fleet.BackendCoder},
		}},
	}}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, client, cleanup := newTestServer(t)
	defer cleanup()

	stream, err := client.RebuildInstance(context.Background(), &fleetgrpc.RebuildInstanceRequest{Fleet: "alpha", Instance: "i1"})
	if err != nil {
		t.Fatalf("RebuildInstance call: %v", err)
	}
	if _, err = stream.Recv(); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("want FailedPrecondition for coder backend, got %v", err)
	}
}

func TestRebuildInstanceRejectsTransitional(t *testing.T) {
	isolateFleetDir(t)
	if err := state.Save(&state.State{Fleets: map[string]*fleet.Fleet{
		"alpha": {Name: "alpha", Instances: []*fleet.Instance{{Name: "i1", Status: fleet.StatusCreating}}},
	}}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, client, cleanup := newTestServer(t)
	defer cleanup()

	stream, err := client.RebuildInstance(context.Background(), &fleetgrpc.RebuildInstanceRequest{Fleet: "alpha", Instance: "i1"})
	if err != nil {
		t.Fatalf("RebuildInstance call: %v", err)
	}
	if _, err = stream.Recv(); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("want FailedPrecondition for mid-transition instance, got %v", err)
	}
}

func TestRebuildInstanceRejectsMissing(t *testing.T) {
	isolateFleetDir(t)
	if err := state.Save(&state.State{Fleets: map[string]*fleet.Fleet{
		"alpha": {Name: "alpha"},
	}}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, client, cleanup := newTestServer(t)
	defer cleanup()

	stream, err := client.RebuildInstance(context.Background(), &fleetgrpc.RebuildInstanceRequest{Fleet: "alpha", Instance: "nope"})
	if err != nil {
		t.Fatalf("RebuildInstance call: %v", err)
	}
	if _, err = stream.Recv(); status.Code(err) != codes.NotFound {
		t.Fatalf("want NotFound for missing instance, got %v", err)
	}
}

func TestDestroyInstanceJobRemovesRecord(t *testing.T) {
	isolateFleetDir(t)
	wsDir := t.TempDir()
	if err := state.Save(&state.State{Fleets: map[string]*fleet.Fleet{
		"alpha": {Name: "alpha", Instances: []*fleet.Instance{{Name: "i1", WorkspaceDir: wsDir}}},
	}}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, client, cleanup := newTestServer(t)
	defer cleanup()

	stream, err := client.DestroyInstance(context.Background(), &fleetgrpc.DestroyInstanceRequest{Fleet: "alpha", Instance: ptr("i1")})
	if err != nil {
		t.Fatalf("DestroyInstance: %v", err)
	}
	evs := drainJob(t, stream)
	if d := evs[len(evs)-1].GetDone(); d == nil || !d.GetSuccess() {
		t.Fatalf("destroy not done-success: %v", evs)
	}
	st, _ := state.Load()
	if _, err := st.Fleets["alpha"].GetInstance("i1"); err == nil {
		t.Fatalf("instance record not removed")
	}
}

// TestJobManagerFinishedRetention asserts a finished job stays resolvable by id
// (for async pollers) until FIFO eviction pushes it out.
func TestJobManagerFinishedRetention(t *testing.T) {
	m := newJobManager()

	first := m.start(fleetgrpc.JobKind_JOB_KIND_CREATE_INSTANCE, "f", "i0", time.Now())
	firstID := first.summary.GetJobId()
	if m.get(firstID) != first {
		t.Fatalf("active job not resolvable by id")
	}
	m.finish(firstID)
	if m.get(firstID) != first {
		t.Fatalf("finished job should stay resolvable by id")
	}
	if len(m.summaries()) != 0 {
		t.Fatalf("finished job must not be advertised as active")
	}

	// Filling the retention window evicts the oldest finished job.
	for range finishedJobRetention {
		j := m.start(fleetgrpc.JobKind_JOB_KIND_CREATE_INSTANCE, "f", "i", time.Now())
		m.finish(j.summary.GetJobId())
	}
	if m.get(firstID) != nil {
		t.Fatalf("oldest finished job should be evicted after %d newer ones", finishedJobRetention)
	}
}

func TestActiveJobsSurfacedInGetState(t *testing.T) {
	isolateFleetDir(t)
	if err := state.Save(&state.State{Fleets: map[string]*fleet.Fleet{
		"alpha": {Name: "alpha", Instances: []*fleet.Instance{{Name: "i1", Status: fleet.StatusStopped, ContainerID: "c1"}}},
	}}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	release := make(chan struct{})
	orig := jobRunStart
	jobRunStart = func(f, i string) error {
		<-release // hold the job in-flight until the test releases it
		return nil
	}
	defer func() { jobRunStart = orig }()

	_, client, cleanup := newTestServer(t)
	defer cleanup()

	stream, err := client.StartInstance(context.Background(), &fleetgrpc.StartInstanceRequest{Fleet: "alpha", Instance: "i1"})
	if err != nil {
		t.Fatalf("StartInstance: %v", err)
	}
	// First event (JobStarted) confirms the job is registered + in-flight.
	if ev, err := stream.Recv(); err != nil || ev.GetStarted() == nil {
		t.Fatalf("want JobStarted, got ev=%v err=%v", ev, err)
	}

	// GetState must advertise the in-flight job so a non-launching watcher can
	// learn its identity.
	var found bool
	for i := 0; i < 50 && !found; i++ {
		reply, err := client.GetState(context.Background(), &fleetgrpc.GetStateRequest{})
		if err != nil {
			t.Fatalf("GetState: %v", err)
		}
		for _, js := range reply.GetActiveJobs() {
			if js.GetFleet() == "alpha" && js.GetInstance() == "i1" && js.GetKind() == fleetgrpc.JobKind_JOB_KIND_START_INSTANCE {
				found = true
			}
		}
		if !found {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if !found {
		t.Fatalf("in-flight job not surfaced in GetState.active_jobs")
	}

	close(release)
	drainJob(t, stream)

	// After completion the job is no longer active.
	reply, _ := client.GetState(context.Background(), &fleetgrpc.GetStateRequest{})
	if len(reply.GetActiveJobs()) != 0 {
		t.Fatalf("completed job still active: %v", reply.GetActiveJobs())
	}
}
