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

## Overview

**Primary Use Case**: Full GitOps infrastructure backup and restore for resources with unpredictable external names.

When you restore your GitOps configuration from Git:
1. Crossplane recreates the managed resources 
2. Resources with `deletionPolicy: Orphan` still exist in your cloud provider
3. **Without this function**: Crossplane generates new external names → creates duplicate cloud resources
4. **With this function**: External names are restored from backup → Crossplane adopts existing cloud resources

This function solves the GitOps gap by:
- **Backing up** external names from observed resources to DynamoDB during normal operations
- **Restoring** external names to desired resources during GitOps recovery
- **Focusing on orphaned resources** (the ones that survive cluster disasters)
- **Providing audit trails** for compliance and troubleshooting

## Features

- 🔄 **Automatic Backup & Restore**: Seamlessly handles external name persistence
- 🎯 **Operation Modes**: Choose between processing only orphaned resources or all resources
- 📊 **AWS DynamoDB Storage**: Reliable, scalable external storage backend
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

### 2. Configure DynamoDB

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

### 3. Configure AWS Credentials

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
| `fn.crossplane.io/store-type` | `"awsdynamodb"` | External store type (`awsdynamodb` or `mock`) |
| `fn.crossplane.io/dynamodb-table` | `"external-name-backup"` | DynamoDB table name |
| `fn.crossplane.io/dynamodb-region` | `"us-west-2"` | AWS region for DynamoDB |
| `fn.crossplane.io/operation-mode` | `"only-orphaned"` | Operation mode (`only-orphaned` or `all-resources`) |

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

**Composition keys are formed as:** `{claim-namespace}/{claim-name}/{xr-apiVersion}/{xr-kind}/{xr-name}`

The key components are determined as follows:
- **For Composites created by Claims**: Uses the claim's namespace and name from labels
- **For Direct Composites** (not created by claims): Uses `"none"` for both claim-namespace and claim-name

Examples:

**Claim-based Composite:**
- Claim: `my-database` in namespace `production` 
- XR: `my-database-xyz` of type `aws.platform.upbound.io/v1alpha1/XRDS`
- **Key:** `production/my-database/aws.platform.upbound.io/v1alpha1/XRDS/my-database-xyz`

**Direct Composite:**
- XR: `standalone-db` of type `aws.platform.upbound.io/v1alpha1/XRDS` (no claim)
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
OPERATION_MODE=all-resources

# Or via ConfigMap
data:
  operation-mode: "all-resources"
```

**Important Considerations for All Resources Mode:**
- May require store cleanup similar to only-orphaned mode if resources are deleted
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

## Deletion Behavior

When resources are deleted from the external store (e.g., when switching from `deletionPolicy: Orphan` to `deletionPolicy: Delete`), the function:

1. **Removes the external name** from the DynamoDB store
2. **Removes tracking annotations** (`fn.crossplane.io/stored-external-name`, `fn.crossplane.io/external-name-stored`)
3. **Adds deletion timestamp** annotation: `fn.crossplane.io/external-name-deleted` with the timestamp when the deletion occurred

This deletion annotation helps track which resources were processed for deletion and provides an audit trail of when external names were removed from the store.

## How It Works

### Data Storage Structure

External names are stored in DynamoDB with this structure:

```
Primary Key: cluster_id + composition_key
composition_key format: {claim-namespace}/{claim-name}/{xr-apiVersion}/{xr-kind}/{xr-name}

Data:
{
  "cluster_id": "my-cluster",
  "composition_key": "default/my-claim/example.com/v1alpha1/MyXR/my-xr",
  "external_names": {
    "s3.aws.upbound.io/v1beta1/Bucket/my-bucket": "actual-bucket-name-12345",
    "ec2.aws.upbound.io/v1beta1/VPC/my-vpc": "vpc-0123456789abcdef0"
  }
}
```

### Function Flow

1. **Load Phase**: Load existing external names from DynamoDB
2. **Desired Resources Processing**: 
   - Check each resource against operation mode criteria
   - Restore external names from store if available
   - Add tracking annotations
3. **Observed Resources Processing**:
   - Check each resource against operation mode criteria  
   - Collect external names from resources for storage
   - Use tracking annotations to optimize writes
4. **Store Phase**: Save collected external names back to DynamoDB

### Function Annotations

The function uses several annotations to track state and optimize performance:

**Tracking Annotations** (added during storage):
```yaml
metadata:
  annotations:
    fn.crossplane.io/stored-external-name: "actual-external-name"
    fn.crossplane.io/external-name-stored: "2024-08-06T12:00:00Z"
```

**Deletion Annotation** (added during deletion):
```yaml
metadata:
  annotations:
    fn.crossplane.io/external-name-deleted: "2024-08-06T12:30:00Z"
```

These annotations serve multiple purposes:
- **Performance Optimization**: Tracking annotations prevent unnecessary DynamoDB writes when external names haven't changed
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
# Test only-orphaned mode (default)
OPERATION_MODE=only-orphaned xp render example/xr.yaml example/composition.yaml example/functions.yaml

# Test all-resources mode  
OPERATION_MODE=all-resources xp render example/xr.yaml example/composition.yaml example/functions.yaml
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
   - Consider using `only-orphaned` mode to reduce processing overhead
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