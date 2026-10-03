# lwd

The boring, deterministic application platform for OBH/QH.

> Given an application, environment, release and target host, make the
> declared application exist, remain healthy, and be recoverable.

LWD 2.0 is under construction on this branch. The contract is
[docs/lwd2/DESIGN.md](docs/lwd2/DESIGN.md). The v1 multi-node engine is
preserved at tag `v0.1-legacy`.

Layers: Ansible manages hosts, LWD manages workloads and application
resources, agentd (later) operates LWD through its API.
