Load CLAUDE.md for context

## Image Bump Procedure

The image-updater tool updates container image digests in `config/config.yaml`.
Run it from the `tooling/image-updater/` directory.

### Available Components

Components are defined in `tooling/image-updater/config.yaml`. Current list:

maestro, maestro-agent-sidecar, hypershift, pko-package, pko-manager,
pko-remote-phase-manager, clusters-service, aro-hcp-exporter, arohcpfrontend,
arohcpbackend, backplaneAPI, kubeEvents, acm-operator, acm-mce,
aksCommandRuntime, acrPull, arobit-forwarder, arobit-mdsd, secretSyncController,
secretSyncProvider, imageSync, prometheus-operator, prometheus,
prometheus-config-reloader, kube-state-metrics, admin-api, sessiongate,
velero-server, velero-azure-plugin, velero-hypershift-plugin,
kube-webhook-certgen, istio-istioctl

Groups: hypershift-stack, pko, cs, aro-hcp-exporter, aro-rp, aro-deps,
obs-agents, platform-utils, prom-stack, velero, istio

ACM components (require extra steps): acm-operator, acm-mce

### Bump a Single Component

```bash
cd tooling/image-updater
./image-updater update --config config.yaml --force --components <name> --output-format markdown
```

Example: `--components hypershift`

### Bump All Components

```bash
cd tooling/image-updater
./image-updater update --config config.yaml --force --output-format markdown
```

### Bump All Except Some Components

Use `--exclude-components` to skip specific components:

```bash
cd tooling/image-updater
./image-updater update --config config.yaml --force --exclude-components arohcpfrontend,arohcpbackend --output-format markdown
```

### Bump by Group

```bash
cd tooling/image-updater
./image-updater update --config config.yaml --force --groups hypershift-stack --output-format markdown
```

### Post-Bump Steps

After running the updater, you MUST regenerate configs and digests.

#### Standard components (non-ACM)

```bash
make -C config materialize
```

Run this from the repo root (`/path/to/ARO-HCP`).

#### ACM components (acm-operator, acm-mce)

If any `acm-*` component was updated, run ALL of the following from the repo root:

```bash
make -C config materialize
make -C acm helm-charts
make update-helm-fixtures
make yamlfmt
```

### Commit Strategy

Split changes into exactly two commits:

1. **First commit**: The image digest update in `config/config.yaml`
   - Only contains the changes made by `image-updater update`
   - Commit message: `bump <component-name(s)> image(s)`

2. **Second commit**: The rendered config and digest changes
   - Contains all changes produced by the post-bump `make` commands
   - Commit message: `render config after <component-name(s)> bump`

### Dry Run

To preview changes without writing:

```bash
./image-updater update --config config.yaml --force --components hypershift --dry-run --output-format markdown
```

### Troubleshooting

- Some images require Azure auth (`useAuth: true`). Make sure `az` is logged in.
- Some images require KeyVault-stored pull secrets. The user must have access to the referenced KeyVault.
- Use `-v 2` for debug output if an update fails silently.
