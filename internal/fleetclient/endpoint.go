// Package fleetclient is the client side of the fleet client/server split: it
// dials the fleet server, auto-spawns a local one when needed, runs the version
// handshake, and hands callers a fleetgrpc.FleetServiceClient.
//
// BOUNDARY: this package may import only fleetgrpc + fleetpaths + version (and
// grpc). It must NEVER import the server-only internals (internal/state,
// internal/backend, internal/create, internal/control, internal/flog, or
// internal/server). That rule is what keeps a remote client possible and is
// enforced by the depguard rule in .golangci.yml.
package fleetclient

import (
	"os"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/fleetpaths"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// The endpoint-selection environment variables. This package owns their
// semantics (selectEndpoint / IsRemote / IsGateway), so the names are defined
// here once; other packages that read or set them (the TUI's armada switch,
// admiralmcp's local-only guard) refer to these constants.
const (
	// EnvGateway points the client through a fleet gateway:
	// FLEET_GATEWAY=https://gw:50051/<id>.
	EnvGateway = "FLEET_GATEWAY"
	// EnvServer points the client at a plain remote TCP daemon:
	// FLEET_SERVER=host:port.
	EnvServer = "FLEET_SERVER"
	// EnvSSH points the client at a remote daemon over SSH, given as
	// ssh://[user@]host[:port][/abs/remote/socket]. fleet forwards the remote
	// daemon's unix socket to a local path (via the system `ssh` binary and an
	// SSH ControlMaster) and drives it like FLEET_SOCKET — but sets the tunnel up
	// for you. SSH provides auth (key / password / agent) and confidentiality; no
	// bearer token is used. It is the convenience form of FLEET_SOCKET.
	EnvSSH = "FLEET_SSH"
	// EnvSocket points the client at a unix socket at an ARBITRARY path — the
	// secure "develop over SSH" path: forward the remote daemon's socket to the
	// laptop (`ssh -L /local.sock:~/.fleet/fleet.sock user@host`) and set
	// FLEET_SOCKET=/local.sock. SSH authenticates the transport, so no bearer
	// token is needed (the daemon's socket server is auth-less by design, gated
	// by the far-side 0600/same-user perms). Unlike the default local socket it
	// is NOT auto-spawned — the daemon lives on the far end of the forward.
	EnvSocket = "FLEET_SOCKET"
	// EnvToken carries the bearer token for a gateway/remote daemon; falls back
	// to ~/.fleet/mcp.token (see BearerToken).
	EnvToken = "FLEET_TOKEN"
)

// Endpoint abstracts WHERE the server is and HOW we reach it, so commands never
// see a socket path or address. Locality is a property of the endpoint:
// auto-spawn is only valid for a local endpoint (you can't fork-exec a process
// on someone else's machine).
type Endpoint interface {
	// Target is the gRPC dial target (scheme-qualified).
	Target() string
	// IsLocal reports whether this endpoint is the local unix socket (and thus
	// whether auto-spawn / version-restart are permitted).
	IsLocal() bool
	String() string
	// DialOptions are the grpc.DialOptions for this endpoint (transport creds, and
	// for a gateway endpoint the CONNECT dialer + per-RPC token).
	DialOptions() []grpc.DialOption
}

// insecureCreds is the transport for unix-socket / plain-TCP endpoints (and the
// inner h2c of a gateway tunnel, whose outer TLS the dialer establishes).
func insecureCreds() []grpc.DialOption {
	return []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}
}

// localEndpoint is the per-user unix socket; auto-spawnable.
type localEndpoint struct{ socket string }

func (e localEndpoint) Target() string                 { return "unix://" + e.socket }
func (e localEndpoint) IsLocal() bool                  { return true }
func (e localEndpoint) String() string                 { return "unix:" + e.socket }
func (e localEndpoint) DialOptions() []grpc.DialOption { return insecureCreds() }

// remoteEndpoint is a plain TCP target (e.g. a remote TUI). Not auto-spawnable.
type remoteEndpoint struct{ addr string }

func (e remoteEndpoint) Target() string                 { return "dns:///" + e.addr }
func (e remoteEndpoint) IsLocal() bool                  { return false }
func (e remoteEndpoint) String() string                 { return e.addr }
func (e remoteEndpoint) DialOptions() []grpc.DialOption { return insecureCreds() }

// socketEndpoint is a unix socket at an arbitrary path (FLEET_SOCKET) — typically
// a remote daemon's socket forwarded to the laptop over SSH. Like remoteEndpoint
// it is NOT local: no auto-spawn (you can't fork the daemon across the forward)
// and a version mismatch is a hard error. The transport is insecure creds (the
// SSH forward, not gRPC, provides confidentiality), and no bearer token is sent
// because the daemon's socket server is auth-less.
type socketEndpoint struct{ socket string }

func (e socketEndpoint) Target() string                 { return "unix://" + e.socket }
func (e socketEndpoint) IsLocal() bool                  { return false }
func (e socketEndpoint) String() string                 { return "socket:" + e.socket }
func (e socketEndpoint) DialOptions() []grpc.DialOption { return insecureCreds() }

// selectEndpoint picks the transport, in precedence order:
//   - FLEET_GATEWAY=https://gw:50051/<id> (or http://… behind a TLS-terminating
//     proxy) → through a fleet gateway (the bearer token from FLEET_TOKEN, or
//     ~/.fleet/mcp.token for a same-host user).
//   - FLEET_SERVER=host:port → a plain remote TCP target.
//   - FLEET_SOCKET=/path → a unix socket at an arbitrary path (an SSH-forwarded
//     remote daemon socket).
//   - FLEET_SSH=ssh://[user@]host[:port][/socket] → fleet sets up the SSH forward
//     and targets the resulting local socket.
//   - otherwise → the local auto-spawned unix socket.
//
// It errors when FLEET_GATEWAY / FLEET_SSH is set but malformed (or the SSH
// tunnel can't be established). Remote endpoints (gateway/server/socket/ssh) are
// not auto-spawned and a version mismatch is a hard error.
func selectEndpoint() (Endpoint, error) {
	if gw := os.Getenv(EnvGateway); gw != "" {
		return newGatewayEndpoint(gw, gatewayToken())
	}
	if addr := os.Getenv(EnvServer); addr != "" {
		return remoteEndpoint{addr: addr}, nil
	}
	if sock := strings.TrimSpace(os.Getenv(EnvSocket)); sock != "" {
		return socketEndpoint{socket: sock}, nil
	}
	if raw := strings.TrimSpace(os.Getenv(EnvSSH)); raw != "" {
		spec, err := parseSSHURL(raw)
		if err != nil {
			return nil, err
		}
		localSocket, err := ensureSSHTunnel(spec)
		if err != nil {
			return nil, err
		}
		return socketEndpoint{socket: localSocket}, nil
	}
	return localEndpoint{socket: fleetpaths.SocketPath()}, nil
}

// IsRemote reports whether the client is pointed at a remote daemon
// (FLEET_GATEWAY, FLEET_SERVER, FLEET_SOCKET, or FLEET_SSH set) rather than the
// local auto-spawned socket. Client-host concerns — dependency checks, the
// local-exec TTY carve-out — only apply when this is false. FLEET_SOCKET/FLEET_SSH
// count as remote: though the transport is a unix socket, the daemon is on the
// far end of an SSH forward, so auto-spawn and client-host assumptions must not apply.
func IsRemote() bool {
	return os.Getenv(EnvGateway) != "" || os.Getenv(EnvServer) != "" ||
		os.Getenv(EnvSocket) != "" || os.Getenv(EnvSSH) != ""
}

// IsGateway reports whether the client reaches the daemon THROUGH a fleet
// gateway (FLEET_GATEWAY set), as opposed to a direct remote TCP target
// (FLEET_SERVER) or the local socket. Only a gateway connection has a gateway
// version to show in the control chain.
func IsGateway() bool {
	return os.Getenv(EnvGateway) != ""
}

// gatewayToken resolves the bearer token for a gateway endpoint: FLEET_TOKEN, or
// the on-disk MCP token (so a user on the same host as their daemon need only
// supply the URL).
func gatewayToken() string {
	if t := os.Getenv(EnvToken); t != "" {
		return strings.TrimSpace(t)
	}
	if data, err := os.ReadFile(fleetpaths.McpTokenPath()); err == nil {
		return strings.TrimSpace(string(data))
	}
	return ""
}

// BearerToken resolves the bearer token used to authenticate to a fleet daemon:
// FLEET_TOKEN if set, else the on-disk MCP token (~/.fleet/mcp.token). It is the
// same secret the MCP HTTP server and the remote-fleet gRPC surface accept, so
// callers (e.g. the Settings page) can hand it out for copy-paste setup of
// remote clients. Returns "" when neither source yields a token.
func BearerToken() string {
	return gatewayToken()
}
