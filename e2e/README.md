# function-external-name-backup-restore end-to-end tests

This directory is a small Upbound project used only to run **real** end-to-end
tests for `function-external-name-backup-restore`: `up test run tests/* --e2e`
installs it on a throwaway control plane, together with the function built from
the pull request, and checks a full backup and restore. It is never published to
production. No cloud account is needed: the test uses the ConfigMap store.

## Layout

```
e2e/
├── upbound.yaml                 # test configuration; depends on the function under test (pinned by CI)
├── apis/xbackups/               # XRD + Composition: a ConfigMap rendered by function-go-templating, then the function under test
├── functions/.gitkeep           # must exist for `up`
└── tests/
    └── e2etest-backup-restore/
        └── test.yaml            # E2ETest: namespaces + RBAC, two XRs, waits for both to be Ready
```

Dependency-installed functions are named `<org>-<repo>`, so the Composition
references `solutions-function-external-name-backup-restore` and
`crossplane-contrib-function-go-templating`.

## What the test proves

Two XRs with the same name in different namespaces:

- **`e2e-source/e2e-backup`** composes a ConfigMap with
  `crossplane.io/external-name: e2e-original-external-name`. The function backs
  up its external name and resource name to the store ConfigMap
  `crossplane-system/external-name-backup-e2e`.
- **`e2e-restore/e2e-backup`** composes the same ConfigMap without an external
  name. With `override-namespace: e2e-source` it reads the source XR's backup,
  and `restore-only: "true"` makes the function fail until every composed
  resource has one.

The Composition marks the ConfigMap Ready only when its observed external name
equals `spec.expectedExternalName`. So the restore XR becomes Ready only after the
backup was written, read back, and the external name restored onto its resource.
Until the source XR's backup exists, the restore XR shows the fatal
`require-restore is enabled but no resource data found in store` and retries.

## Setup in the test (`initResources`)

- Namespaces `e2e-source` and `e2e-restore`.
- A ClusterRole for the ConfigMap store (ConfigMaps, namespaces), bound to all
  ServiceAccounts in `crossplane-system`, because the function's ServiceAccount
  name has a generated suffix.
- A ClusterRole aggregated to Crossplane so it can compose ConfigMaps, which it
  can't by default. Its verbs are explicit: the test user can only grant verbs it
  holds itself, so `*` is rejected.

## Two repos, one function

The function dev build is pushed to `solutions/<repo>` and the wrapper project to
**`solutions/<repo>-e2e-wrapper`**. They must be separate: Crossplane's package
lock keys dependency-graph nodes by source repo, so a project and a function
sharing one repo collide (`node ... already exists`). The `<repo>-e2e-wrapper`
repo is granted to the e2e robot in `solutions-gitops-prod`.

The control plane name, `<project>-uptest-<test>`, is cut to 63 characters here
(`...-uptest-e2e-ba`). That doesn't matter for this test, which uses no OIDC
subject.

## What runs in CI

On a pull request labelled `run-e2e-tests`, `.github/workflows/e2e.yaml` (synced
from `upbound/sa-up`):

1. builds the function package from the PR head and pushes it to `solutions/<repo>`
   at a dev tag,
2. pins that tag in `upbound.yaml` (`@FUNCTION_VERSION@`),
3. switches to the e2e Spaces control plane group, and
4. runs `up test run tests/* --e2e`, pushing the wrapper project to
   `solutions/<repo>-e2e-wrapper`.

A separate job then posts the `e2e` commit status that branch protection requires.
