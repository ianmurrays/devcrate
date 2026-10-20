# devcrate

A local CLI that runs dev environments on cloud VMs, stops them when idle, and connects to them over SSH.

## Language

**Environment**:
One dev workspace: a VM with a persistent disk, optionally running a devcontainer, identified by a name. A repo can have several Environments, one per worktree.
_Avoid_: Workspace, machine, box

**Provider**:
An external executable that creates, stops and deletes Environments on one cloud, behind a fixed set of verbs.
_Avoid_: Driver, backend, plugin

**Agent**:
The small devcrate program running on an Environment's VM that detects idleness and triggers Self-stop.
_Avoid_: Daemon, sidecar

**Stop**:
Ending compute billing for an Environment while its persistent disk survives.
_Avoid_: Shutdown, power off, pause

**Delete**:
Removing an Environment and everything it owns, including its persistent disk.
_Avoid_: Destroy, terminate

**Ready**:
An Environment whose VM is running and whose setup has finished, so it can be connected to.
_Avoid_: Up, healthy

**Idle**:
An Environment with no open SSH connection, no setup running and no active Keep-alive.
_Avoid_: Inactive, unused

**Keep-alive**:
A time-limited hold that stops an Environment from counting as Idle, for work that runs with nobody connected.
_Avoid_: Lock, pin

**Self-stop**:
A Stop initiated by the Agent from inside the Environment, using a credential pushed to the VM.
_Avoid_: Auto-shutdown

**Base image**:
A Provider's disk image of an Environment's root disk with setup already applied, which a repo's new VMs boot from so setup has less to do.
_Avoid_: Snapshot, image, template

**Rebuild**:
Recreating an Environment's devcontainer from its configuration, discarding the old container but keeping cached image layers.
_Avoid_: Reset, recreate
