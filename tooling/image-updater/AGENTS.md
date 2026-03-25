Load all of CLAUDE.md for context

## Image Bump Procedure

The image-updater tool updates container image digests, SHAs, and version
fields in `config/config.yaml`. Run it from the `tooling/image-updater/`
directory.

### Available Components

Components and groups are defined in `tooling/image-updater/config.yaml`.
Refer to that file for the authoritative list of component and group names.

ACM-related components (those whose names start with `acm-`) require extra
post-bump steps described below.

### Branch Strategy

All bump work MUST happen on a dedicated branch, never on main. Create a branch
before making any changes. Name the branch after the component(s) being updated
or excluded:

- Single component: `bump-<component>` (e.g. `bump-hypershift`)
- Multiple components: `bump-<component1>-<component2>` (e.g. `bump-maestro-hypershift`)
- All except some: `bump-all-except-<excluded>` (e.g. `bump-all-except-arohcpfrontend`)
- All components: `bump-all`

```bash
git checkout -b bump-<name> main
```

### Bump a Single Component

```bash
cd tooling/image-updater
./image-updater update --config config.yaml --components <name> --output-format markdown
```

Example: `--components hypershift`

### Bump All Components

```bash
cd tooling/image-updater
./image-updater update --config config.yaml --output-format markdown
```

### Bump All Except Some Components

Use `--exclude-components` to skip specific components:

```bash
cd tooling/image-updater
./image-updater update --config config.yaml --exclude-components arohcpfrontend,arohcpbackend --output-format markdown
```

### Bump by Group

```bash
cd tooling/image-updater
./image-updater update --config config.yaml --groups hypershift-stack --output-format markdown
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

### Commit and PR Strategy

All bump changes (image digest update, rendered configs, fixtures) go into a
single commit. The commit message is used directly as the PR title and body,
so format it carefully:

- **Title**: `chore: bump <component-name(s)> image(s)`
- **Body**: The markdown table output from the `image-updater update` command

The `--output-format markdown` flag produces a table like:

```
| Name | Old Digest | New Digest | Tag | Date | Status |
| --- | --- | --- | --- | --- | --- |
| hypershift | 5c4ce9ac3f41… | a190b2bd63a0… | latest | 2026-03-25 08:24 | updated |
```

Include this table in the commit body so the PR inherits it automatically.
Reviewers can then see what changed at a glance.

### Force Update

Add `--force` to update even when digests already match. This is useful for
regenerating version tag comments but should not be used by default as it
produces unnecessary changes when nothing has actually been updated.

### Dry Run

To preview changes without writing:

```bash
./image-updater update --config config.yaml --components hypershift --dry-run --output-format markdown
```

### Troubleshooting

- Some images require Azure auth (`useAuth: true`). Make sure `az` is logged in.
- Some images require KeyVault-stored pull secrets. The user must have access to the referenced KeyVault.
- Use `-v 2` for debug output if an update fails silently.
