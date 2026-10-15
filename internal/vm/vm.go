// Package vm names the paths on an Environment's VM that both devcrate and
// the Agent use.
package vm

const (
	// VolumeDir is where the Provider mounts the persistent volume.
	VolumeDir = "/var/lib/devcrate"
	// AgentError holds the Agent's last Self-stop error, on the volume, for
	// the next up or ssh to print.
	AgentError = VolumeDir + "/agent-error"
	// SelfStopEnv holds the Provider's self_stop_env (tmpfs, root-only).
	SelfStopEnv = "/run/devcrate/self-stop.env"
	// AgentSock is the Agent's SSH-agent relay; setup bind-mounts its
	// directory into the devcontainer.
	AgentSock = "/run/devcrate/ssh-agent/agent.sock"
	// CredsOwner is the devcontainer remoteUser's uid:gid, written by setup.
	CredsOwner = "/run/devcrate/creds-owner"
)
