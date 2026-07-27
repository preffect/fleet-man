package cli

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetclient"
	"github.com/BenjaminBenetti/fleet-man/internal/protoconv"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
)

func newUpCmd() *cobra.Command {
	var repoFlag string
	var backendFlag string
	var branchFlag string
	var pathFlag string

	cmd := &cobra.Command{
		Use:   "up <name>",
		Short: "Spawn a new instance",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]

			backendType, err := fleet.ParseBackendType(backendFlag)
			if err != nil {
				return err
			}

			var target *fleet.Target
			req := &fleetgrpc.CreateInstanceRequest{
				Backend: protoconv.BackendToProto(backendType),
				Verbose: true,
			}

			if pathFlag != "" {
				// Local-folder instance: bind-mount an existing folder in place
				// instead of cloning a remote. The path is resolved on the DAEMON
				// host, so it must be absolute (we can only expand a relative path
				// against the CLIENT's cwd, which is only the same machine for a
				// local daemon).
				if repoFlag != "" {
					return fmt.Errorf("--path and --repo are mutually exclusive")
				}
				if branchFlag != "" {
					return fmt.Errorf("--branch cannot be combined with --path (a local folder is used in place, on its current checkout)")
				}
				absPath := pathFlag
				if !filepath.IsAbs(absPath) {
					if fleetclient.IsRemote() {
						return fmt.Errorf("--path must be an absolute path on the remote daemon host: %q", pathFlag)
					}
					absPath, err = filepath.Abs(absPath)
					if err != nil {
						return fmt.Errorf("resolve --path: %w", err)
					}
				}
				target, err = fleet.ResolvePath(name, absPath)
				if err != nil {
					return err
				}
				req.SourcePath = &absPath
				fmt.Printf("Creating %s/%s from folder %s (in place)...\n", target.Fleet, target.Instance, absPath)
			} else {
				target, err = fleet.Resolve(name, repoFlag)
				if err != nil {
					return err
				}

				// Resolve the remote URL: explicit --repo wins; otherwise reuse the
				// fleet's recorded remote (read via the server) and finally fall back
				// to the cwd's git remote for a brand-new fleet. The server pre-creates
				// the StatusCreating record and provisions — no client-side state write
				// (the #63 fix; concurrent `fleet up` now serialize through the server).
				remoteURL := repoFlag
				if remoteURL == "" {
					if st, serr := fetchFleetState(cmd.Context()); serr == nil {
						if f := st.GetFleets()[target.Fleet]; f != nil {
							remoteURL = f.GetRemote()
						}
					}
					if remoteURL == "" {
						remoteURL, err = fleet.RemoteURLFromCwd()
						if err != nil {
							return fmt.Errorf("could not determine repo URL: %w", err)
						}
					}
				}
				if remoteURL != "" {
					req.Remote = &remoteURL
				}
				if branchFlag != "" {
					req.Branch = &branchFlag
				}
				fmt.Printf("Creating %s/%s (backend: %s)...\n", target.Fleet, target.Instance, backendType)
			}

			req.Fleet = target.Fleet
			req.Instance = target.Instance

			if err := runInstanceJob(cmd.Context(), func(ctx context.Context, svc fleetgrpc.FleetServiceClient) (grpc.ServerStreamingClient[fleetgrpc.JobEvent], error) {
				return svc.CreateInstance(ctx, req)
			}); err != nil {
				return err
			}

			if st, serr := fetchFleetState(cmd.Context()); serr == nil {
				if f := st.GetFleets()[target.Fleet]; f != nil {
					for _, inst := range f.GetInstances() {
						if inst.GetName() == target.Instance {
							cid := inst.GetContainerId()
							fmt.Printf("Instance %s/%s is running (container: %s)\n", target.Fleet, target.Instance, cid[:min(12, len(cid))])
						}
					}
				}
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&repoFlag, "repo", "", "Git remote URL to clone from")
	cmd.Flags().StringVar(&backendFlag, "backend", "devcontainer", "Backend type: devcontainer, coder, or codespaces")
	cmd.Flags().StringVar(&branchFlag, "branch", "", "Git branch to check out (defaults to the repository's default branch)")
	cmd.Flags().StringVar(&pathFlag, "path", "", "Absolute path to an existing folder (on the daemon host) to use as the project root, bind-mounted in place instead of cloning a remote. Mutually exclusive with --repo/--branch")
	return cmd
}
