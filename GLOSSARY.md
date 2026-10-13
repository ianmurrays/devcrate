# devcrate

A local CLI that runs dev environments on cloud VMs, stops them when idle, and connects to them over SSH.

## Language

**Environment**:
One dev workspace: a VM with a persistent disk, optionally running a devcontainer, identified by a name.
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

**Self-stop**:
A Stop initiated by the Agent from inside the Environment, using a credential pushed to the VM.
_Avoid_: Auto-shutdown
