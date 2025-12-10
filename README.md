# Crossplane External Name Backup & Restore Function

A Crossplane composition function designed for **GitOps disaster recovery scenarios** where you need to restore your entire Crossplane infrastructure from Git, but some cloud resources have external names that aren't known upfront.

## The GitOps Gap This Function Fills

In a typical GitOps setup, your Crossplane compositions and claims are stored in Git. If you lose your Kubernetes cluster, you can restore everything from Git. However, there's a critical gap:

**Cloud resources with auto-generated names** cannot be restored from Git alone because their external names aren't predetermined.

**Common Examples:**
- **AWS Networking**: VPCs (`vpc-0123456789abcdef0`), subnets (`subnet-0abc123def456789`), security groups (`sg-0987654321fedcba`)
- **Storage**: S3 buckets with random suffixes, EBS volumes
- **Databases**: RDS instances with generated identifiers
- **Load Balancers**: ELBs with auto-generated names

## XR Name Backup (Nested Compositions)

This function also supports backing up **Composite Resource (XR) names** for nested compositions. When XRs are composed as part of other compositions, their `metadata.name` is generated deterministically from the parent UID:

```
XR name = SHA256(parentUID + compositionResourceName)[:12]
```

**The Problem**: If the parent UID changes (e.g., during migration or recreation), all child XR names change, breaking references to existing resources.

**The Solution**: This function backs up `metadata.name` for all resources (independent of operation mode), allowing XRs to retain their original names even when parent UIDs change.

**Key difference from external name backup:**
- **External name backup**: Respects backup scope (orphaned vs all)
- **Resource name backup**: Always active for all resources (XRs don't have deletion policies)

## Overview

**Primary Use Case**: Full GitOps infrastructure backup and restore for resources with unpredictable external names.

When you restore your GitOps configuration from Git:
1. Crossplane recreates the managed resources 
2. Resources with `deletionPolicy: Orphan` still exist in your cloud provider
3. **Without this function**: Crossplane generates new external names → creates duplicate cloud resources
4. **With this function**: External names are restored from backup → Crossplane adopts existing cloud resources

This function solves the GitOps gap by:
- **Backing up** external names and resource names from observed resources to DynamoDB during normal operations
- **Restoring** external names and resource names to desired resources during GitOps recovery
- **Focusing on orphaned resources** for external names (the ones that survive cluster disasters)
- **Backing up all resource names** regardless of operation mode (for XRs in nested compositions)
- **Providing audit trails** for compliance and troubleshooting

## Features

- 🔄 **Automatic Backup & Restore**: Seamlessly handles external name and resource name persistence
- 🎯 **Operation Modes**: Choose between processing only orphaned resources or all resources for external names
- 📦 **XR Name Backup**: Always backs up `metadata.name` for all resources (independent of operation mode)
- 📊 **Multiple Storage Backends**: AWS DynamoDB or Kubernetes ConfigMaps
- 🏷️ **Annotation-Based Control**: Fine-grained control through XR annotations
- ⚡ **Performance Optimized**: Tracking annotations prevent unnecessary writes
- 🔧 **Flexible Configuration**: Environment variables and ConfigMap support
- 🧹 **Purge Capabilities**: Clean up stored data when needed

## Quick Start

### 1. Deploy the Function

```bash
# Install the function
kubectl apply -f package/crossplane.yaml
```

### 2. Configure Storage Backend

Choose between DynamoDB or Kubernetes ConfigMaps as your storage backend.

#### Option A: DynamoDB (Default)

Create a DynamoDB table with the following schema:

```yaml
# DynamoDB Table Schema
TableName: external-name-backup
KeySchema:
  - AttributeName: cluster_id
    KeyType: HASH
  - AttributeName: composition_key
    KeyType: RANGE
AttributeDefinitions:
  - AttributeName: cluster_id
    AttributeType: S
  - AttributeName: composition_key
    AttributeType: S
```

#### Option B: Kubernetes ConfigMaps

No external infrastructure required. The function stores data in ConfigMaps within your cluster.

**ConfigMap naming convention:** `external-name-backup-{cluster-id}`

**Data structure:**
- Each ConfigMap contains data for one cluster
- Composition keys are base64-encoded as data keys
- Resource data is stored as JSON

**Required RBAC permissions:**

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: function-external-name-backup-restore-configmap
rules:
  - apiGroups: [""]
    resources: ["configmaps"]
    verbs: ["get", "create", "update", "delete"]
  - apiGroups: [""]
    resources: ["namespaces"]
    verbs: ["get"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: function-external-name-backup-restore-configmap
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: function-external-name-backup-restore-configmap
subjects:
  - kind: ServiceAccount
    name: <service-account-name>  # See note below
    namespace: crossplane-system
```

> **Note:** The function's ServiceAccount name has an auto-generated suffix (e.g., `function-external-name-backup-restore-a1b2c3d4e5f6`). Retrieve it with:
> ```bash
> kubectl get serviceaccount -n crossplane-system -l pkg.crossplane.io/function=function-external-name-backup-restore -o name
> ```

**Usage in composition:**

```yaml
apiVersion: apiextensions.crossplane.io/v1
kind: Composition
metadata:
  name: my-composition
spec:
  mode: Pipeline
  pipeline:
  - step: create-resources
    functionRef:
      name: function-patch-and-transform
  - step: external-name-backup
    functionRef:
      name: function-external-name-backup-restore
    credentials:
      - name: store-creds
        source: Secret
        secretRef:
          namespace: crossplane-system
          name: configmap-store-creds  # Can be a dummy secret
          key: credentials
```

> **Note:** The `credentials` block is required due to a Crossplane limitation - if a function uses credentials in any composition, the credentials block must be present in all compositions using that function. Create a dummy secret if no actual credentials are needed (e.g., with ConfigMap store).

**XR annotations for ConfigMap store:**

```yaml
apiVersion: example.com/v1alpha1
kind: MyXR
metadata:
  annotations:
    fn.crossplane.io/enable-external-store: "true"
    fn.crossplane.io/store-type: "k8sconfigmap"
    fn.crossplane.io/cluster-id: "my-cluster"
    fn.crossplane.io/configmap-namespace: "crossplane-system"  # optional, default
```

### 3. Configure AWS Credentials (DynamoDB only)

Create a secret with your AWS credentials:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: aws-account-creds
  namespace: crossplane-system
type: Opaque
data:
  credentials: <base64-encoded-json-credentials>
```

The credentials can be provided in two formats:

**JSON Format (recommended):**
```json
{
  "accessKeyId": "AKIAIOSFODNN7EXAMPLE", 
  "secretAccessKey": "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
  "sessionToken": "optional-for-temporary-credentials"
}
```

**AWS CLI INI Format:**
```ini
[default]
aws_access_key_id = AKIAIOSFODNN7EXAMPLE
aws_secret_access_key = wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY
aws_session_token = optional-for-temporary-credentials
```

### 4. Use in Compositions

```yaml
apiVersion: apiextensions.crossplane.io/v1
kind: Composition  
metadata:
  name: my-composition
spec:
  mode: Pipeline
  pipeline:
  - step: create-resources
    functionRef:
      name: function-patch-and-transform
    # ... your resource creation logic
  - step: external-name-backup
    functionRef:
      name: function-external-name-backup-restore
    credentials:
      - name: aws-creds
        source: Secret
        secretRef:
          namespace: crossplane-system
          name: aws-account-creds
          key: credentials
```

## Configuration

Configuration is provided through XR annotations. All configuration is specified on your Composite Resource (XR) using annotations:

### Required Annotations

| Annotation | Example | Description |
|------------|---------|-------------|
| `fn.crossplane.io/enable-external-store` | `"true"` | Enable external store operations |
| `fn.crossplane.io/cluster-id` | `"my-cluster"` | Unique identifier for this cluster |
| `fn.crossplane.io/store-type` | `"awsdynamodb"` | External store type (`awsdynamodb`, `k8sconfigmap`, or `mock`) |
| `fn.crossplane.io/dynamodb-table` | `"external-name-backup"` | DynamoDB table name (only for `awsdynamodb`) |
| `fn.crossplane.io/dynamodb-region` | `"us-west-2"` | AWS region for DynamoDB (only for `awsdynamodb`) |
| `fn.crossplane.io/configmap-namespace` | `"crossplane-system"` | Namespace for ConfigMap store (only for `k8sconfigmap`, default: `crossplane-system`) |
| `fn.crossplane.io/backup-scope` | `"orphaned"` | Backup scope (`orphaned` or `all`) |

### Optional Annotations

| Annotation | Example | Description |
|------------|---------|-------------|
| `fn.crossplane.io/override-kind` | `"XNetwork"` | Override XR kind in composition key lookup (for migrations) |
| `fn.crossplane.io/override-namespace` | `"none"` | Override namespace in composition key lookup (for migrations from cluster-scoped to namespaced XRs) |
| `fn.crossplane.io/restore-only` | `"true"` | Enable restore-only mode: always restore from store regardless of operation mode, skip backup, fail if any resource is missing from store |
| `fn.crossplane.io/purge-external-store` | `"true"` | Delete all stored external names for this composition |

### AWS Credentials

AWS credentials are provided via Crossplane's credential management system. The function supports:
- Static credentials (Access Key ID + Secret Access Key)  
- Temporary credentials (with Session Token)
- Falls back to default AWS credential chain if no credentials provided

## Operation Modes

### Only Orphaned Mode (Default - Recommended)

**This is the intended mode of operation for production environments.** This mode is designed for critical production resources that are usually not intended to be deleted, where preserving external names is crucial for maintaining infrastructure consistency.

Processes only resources that meet orphaned criteria:
- `deletionPolicy: Orphan`, OR  
- `managementPolicies` that don't contain `"*"` or `"Delete"`

```yaml
# This resource WILL be processed
spec:
  deletionPolicy: Orphan
  managementPolicies: ["*"]  # Orphan policy takes precedence

# This resource WILL be processed  
spec:
  deletionPolicy: Delete
  managementPolicies: ["Create", "Update", "Observe"]  # No Delete policy

# This resource will NOT be processed
spec:
  deletionPolicy: Delete  
  managementPolicies: ["*"]  # Contains Delete via wildcard
```

#### Important: Store Cleanup After Resource Deletion

If a resource protected by this function gets accidentally deleted, you **must purge the store** when creating a new composite or claim that would result in the same composition key.

**Composition keys are formed as:** `{namespace}/{claim-name}/{xr-apiVersion}/{xr-kind}/{xr-name}`

The namespace component is determined as follows:
- **For Composites created by Claims**: Uses the claim's namespace from labels
- **For v2 Namespaced XRs** (without claims): Uses the XR's `metadata.namespace`
- **For v1 Cluster-scoped XRs** (without claims): Uses `"none"`

Examples:

**Claim-based Composite:**
- Claim: `my-database` in namespace `production`
- XR: `my-database-xyz` of type `aws.platform.upbound.io/v1alpha1/XRDS`
- **Key:** `production/my-database/aws.platform.upbound.io/v1alpha1/XRDS/my-database-xyz`

**v2 Namespaced XR (without claim):**
- XR: `standalone-db` in namespace `team-a` of type `aws.platform.upbound.io/v1alpha1/XRDS`
- **Key:** `team-a/none/aws.platform.upbound.io/v1alpha1/XRDS/standalone-db`

**v1 Cluster-scoped XR (without claim):**
- XR: `standalone-db` of type `aws.platform.upbound.io/v1alpha1/XRDS` (cluster-scoped, no namespace)
- **Key:** `none/none/aws.platform.upbound.io/v1alpha1/XRDS/standalone-db`

If you need to recreate a deleted composite, purge the store first:

```yaml
apiVersion: aws.platform.upbound.io/v1alpha1
kind: XRDS
metadata:
  name: my-database-xyz
  annotations:
    fn.crossplane.io/purge-external-store: "true"
# Apply this first, then remove the annotation for normal operation
```

### All Resources Mode (Experimental)

**⚠️ Experimental Feature**: This mode processes all resources regardless of deletion policy. While we don't restrict users from using this mode, it's primarily for testing and experimental use cases.

```yaml
# Set via environment variable
BACKUP_SCOPE=all

# Or via ConfigMap
data:
  backup-scope: "all"
```

**Important Considerations for All Resources Mode:**
- May require store cleanup similar to orphaned scope if resources are deleted
- Increased DynamoDB usage and costs due to processing more resources
- Tracking annotations help minimize unnecessary writes, but overhead is still higher
- Recommended primarily for testing or specific experimental use cases

## XR Annotations

Control function behavior through annotations on your XR:

### Enable External Store Operations

By default, external store operations are disabled. Enable them with:

```yaml
apiVersion: example.com/v1alpha1
kind: MyXR
metadata:
  annotations:
    fn.crossplane.io/enable-external-store: "true"
# Function will perform backup/restore operations for this XR
```

### Purge Stored Data

```yaml
apiVersion: example.com/v1alpha1
kind: MyXR
metadata:
  annotations:
    fn.crossplane.io/purge-external-store: "true"
# Function will delete all stored external names for this composition
```

### Migration from v1 Cluster-Scoped to v2 Namespaced XRs

When migrating from v1 cluster-scoped XRs to v2 namespaced XRs, the composition key format changes. Use override annotations to look up external names stored under the old format:

```yaml
apiVersion: aws.platform.upbound.io/v1alpha1
kind: Network  # New v2 kind (namespaced)
metadata:
  name: my-network
  namespace: team-a  # v2 namespaced XR
  annotations:
    fn.crossplane.io/enable-external-store: "true"
    fn.crossplane.io/override-kind: "XNetwork"      # Look up keys stored under v1 kind
    fn.crossplane.io/override-namespace: "none"      # Look up keys stored under v1 cluster-scoped format
    fn.crossplane.io/restore-only: "true"             # Safety: fail if no data found
```

**Why these annotations are needed:**

| Scenario | Key Format | Annotations |
|----------|-----------|-------------|
| v1 cluster-scoped export | `none/none/.../XNetwork/my-network` | (original backup) |
| v2 namespaced import | `team-a/none/.../Network/my-network` | (default, wrong key) |
| v2 with overrides | `none/none/.../XNetwork/my-network` | `override-kind`, `override-namespace` |

**The `require-restore` mode:**

When set to `"true"`, the function operates in **restore-only mode** with the following behavior:

1. **Bypass operation mode for restore**: External names and resource names are restored from the store regardless of whether resources meet orphan criteria. This is essential for migrations where the new XR may not have matching `deletionPolicy` or `managementPolicies` set yet.

2. **Skip backup operations**: The function will NOT write any new data to the store, preventing accidental overwrites of existing backup data.

3. **Fail if any resource is missing**: The function will fail with a fatal error if:
   - No data exists in the store for the composition key
   - Any desired resource doesn't have corresponding data in the store

This prevents accidental creation of duplicate cloud resources when:
- Override annotations are misconfigured
- The store doesn't contain data for the expected key
- Migration is attempted before backup data exists
- The composition has more resources than were previously backed up

Example error messages:
```
require-restore is enabled but no resource data found in store for composition key "none/none/aws.platform.upbound.io/v1alpha1/XNetwork/my-network".
Check that override-kind and override-namespace annotations are correct, or remove require-restore annotation.
```

```
require-restore is enabled but no data found in store for resource "vpc" (composition key: "none/none/aws.platform.upbound.io/v1alpha1/XNetwork/my-network").
All resources must have data in the store when require-restore is set.
```

### Override Kind for Migrations

When migrating between composition versions where only the XR kind changes (e.g., `XNetwork` to `Network`), use the `override-kind` annotation to look up external names stored under the old kind:

```yaml
apiVersion: aws.platform.upbound.io/v1alpha1
kind: Network  # New v2 kind
metadata:
  name: my-network
  annotations:
    fn.crossplane.io/enable-external-store: "true"
    fn.crossplane.io/override-kind: "XNetwork"  # Look up keys stored under v1 kind
```

This is useful for migrations where:
- The XRD kind changes between versions (e.g., `XNetwork` → `Network`)
- You want to restore external names backed up from the previous version
- The composition key format includes the kind: `{namespace}/{claim-name}/{apiVersion}/{kind}/{name}`

## Deletion Behavior

When resources are deleted from the external store (e.g., when switching from `deletionPolicy: Orphan` to `deletionPolicy: Delete`), the function:

1. **Removes the external name** from the DynamoDB store
2. **Removes tracking annotations** (`fn.crossplane.io/stored-external-name`, `fn.crossplane.io/external-name-stored`)
3. **Adds deletion timestamp** annotation: `fn.crossplane.io/external-name-deleted` with the timestamp when the deletion occurred

This deletion annotation helps track which resources were processed for deletion and provides an audit trail of when external names were removed from the store.

## How It Works

### Data Storage Structure

Resource data is stored in DynamoDB with this structure:

```
Primary Key: cluster_id + composition_key
composition_key format: {claim-namespace}/{claim-name}/{xr-apiVersion}/{xr-kind}/{xr-name}

Data:
{
  "cluster_id": "my-cluster",
  "composition_key": "default/my-claim/example.com/v1alpha1/MyXR/my-xr",
  "resources": {
    "my-bucket": {
      "externalName": "actual-bucket-name-12345",
      "resourceName": "my-bucket-abc123"
    },
    "my-vpc": {
      "externalName": "vpc-0123456789abcdef0",
      "resourceName": "my-vpc-def456"
    },
    "nested-xr": {
      "resourceName": "nested-xr-xyz789"
    }
  }
}
```

**Note:** The `resourceName` field stores the `metadata.name` of the composed resource. Resources may have only `externalName`, only `resourceName`, or both.

### Function Flow

1. **Load Phase**: Load existing resource data from DynamoDB
2. **Deletion Check**: Check desired resources for deletion criteria (policy change from Orphan to Delete)
3. **Desired Resources Processing**:
   - Check each resource against operation mode criteria (for external names)
   - Restore external names from store if available and not already set
   - Restore resource names (`metadata.name`) from store if not already set
   - Add tracking annotations
4. **Observed Resources Processing**:
   - Collect external names from resources (respects operation mode)
   - Collect resource names from all resources (independent of operation mode)
   - Use tracking annotations to optimize writes
5. **Store Phase**: Save collected resource data back to DynamoDB

### Function Annotations

The function uses several annotations to track state and optimize performance:

**External Name Tracking Annotations** (added during storage):
```yaml
metadata:
  annotations:
    fn.crossplane.io/stored-external-name: "actual-external-name"
    fn.crossplane.io/external-name-stored: "2024-08-06T12:00:00Z"
```

**Resource Name Tracking Annotations** (added during storage):
```yaml
metadata:
  annotations:
    fn.crossplane.io/stored-resource-name: "my-resource-abc123"
    fn.crossplane.io/resource-name-stored: "2024-08-06T12:00:00Z"
```

**Restoration Annotations** (added during restore):
```yaml
metadata:
  annotations:
    fn.crossplane.io/external-name-restored: "2024-08-06T12:15:00Z"
    fn.crossplane.io/resource-name-restored: "2024-08-06T12:15:00Z"
```

**Deletion Annotation** (added during deletion):
```yaml
metadata:
  annotations:
    fn.crossplane.io/external-name-deleted: "2024-08-06T12:30:00Z"
```

These annotations serve multiple purposes:
- **Performance Optimization**: Tracking annotations prevent unnecessary DynamoDB writes when values haven't changed
- **Audit Trail**: Timestamps provide visibility into when operations occurred
- **State Management**: Help the function understand what has been processed

## Examples

See the [`example/`](./example/) directory for complete examples:

- [`composition.yaml`](./example/composition.yaml) - Test composition with S3 buckets
- [`xr.yaml`](./example/xr.yaml) - Basic XR example
- [`xr-skip-store.yaml`](./example/xr-skip-store.yaml) - XR with skip annotation
- [`configmap.yaml`](./example/configmap.yaml) - ConfigMap configuration
- [`functions.yaml`](./example/functions.yaml) - Function definitions for local development

## Local Development

### Prerequisites

- Go 1.23+
- AWS credentials configured
- DynamoDB table created

### Running Locally

```bash
# Set environment variables
export EXTERNAL_STORE_TYPE=awsdynamodb
export DYNAMODB_TABLE_NAME=external-name-backup
export DYNAMODB_REGION=us-west-2

# Build and run
go build .
./function-external-name-backup-restore --insecure --debug

# Test with crossplane beta render
xp render example/xr.yaml example/composition.yaml example/functions.yaml
```

### Testing Different Operation Modes

```bash
# Test orphaned scope (default)
BACKUP_SCOPE=orphaned xp render example/xr.yaml example/composition.yaml example/functions.yaml

# Test all scope
BACKUP_SCOPE=all xp render example/xr.yaml example/composition.yaml example/functions.yaml
```

## AWS Permissions

The function requires the following AWS IAM permissions:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": [
        "dynamodb:GetItem",
        "dynamodb:PutItem", 
        "dynamodb:DeleteItem",
        "dynamodb:DescribeTable"
      ],
      "Resource": "arn:aws:dynamodb:*:*:table/external-name-backup"
    }
  ]
}
```

## Troubleshooting

### Common Issues

1. **Function not connecting to DynamoDB**
   - Verify AWS credentials are configured
   - Check DynamoDB table exists and is accessible
   - Verify IAM permissions

2. **External names not being restored**
   - Check operation mode matches your resource configuration
   - Verify data exists in DynamoDB for your composition key
   - Check function logs for processing details

3. **Performance issues**
   - Ensure tracking annotations are working to prevent unnecessary writes
   - Consider using `orphaned` scope to reduce processing overhead
   - Monitor DynamoDB read/write capacity

### Debugging

Enable debug logging:

```bash
go run . --insecure --debug
```

Check function logs in Kubernetes:

```bash
kubectl logs -f deployment/function-external-name-backup-restore -n crossplane-system
```

## Contributing

1. Fork the repository
2. Create a feature branch
3. Make your changes
4. Add tests if applicable
5. Submit a pull request

## License

This project is licensed under the Apache License 2.0 - see the [LICENSE](LICENSE) file for details.