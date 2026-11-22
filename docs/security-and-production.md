# Security and production considerations

The current setup is designed for local development and learning, not as a
hardened multi-user service. The key limitations are specific to how this
project currently creates and exposes workspaces:

- **Pod isolation:** Workspaces run as Kubernetes Pods, but they do not yet
  provide a hardened tenant boundary or enforce per-tenant network and resource policies.
- **Storage lifecycle:** PVCs preserve workspace files, but snapshots, backup,
  restore, encryption, and quota enforcement are not implemented.
- **Editor access:** The sample code-server configuration uses `--auth none`.
  It is safe only while every route remains behind Pomerium and its owner
  policy. Do not expose the container port directly or bypass that proxy.
- **Local identity and certificates:** The included Dex accounts and
  self-signed certificates are for development. Use a managed identity
  provider, protected secret storage, and appropriately issued certificates
  outside a local cluster.
- **Workspace data:** Workspace files are mounted from a per-workspace PVC at
  `/workspace`. PVC retention and deletion are configurable, but snapshots,
  backup, restore, encryption, and quota enforcement are not implemented.
- **Local-only routing:** `*.localhost.pomerium.io` is configured for local
  development. The project does not publish these URLs or configure a public
  tunnel.

Before considering a multi-user or production deployment, establish and test
at least the following controls:

1. Replace the remaining local defaults with hardened workload isolation,
   dedicated tenant boundaries, and per-workspace network and resource policies.
2. Apply least-privilege Kubernetes RBAC, workload security policies, and
   resource quotas/limits.
3. Enforce authentication and authorization on every route; remove
   unauthenticated access paths and validate owner checks.
4. Use managed secrets with rotation, trusted TLS, and explicit network
   policies.
5. Define persistent storage, backup, and recovery behavior, including what
   happens when a workspace is deleted.
6. Add image provenance/scanning, audit logs, monitoring, and tested upgrade
   and incident-response procedures.

These are design and validation requirements, not settings that can be made
production-safe merely by changing the local sample manifests.
