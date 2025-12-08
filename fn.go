package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/structpb"

	"github.com/crossplane/function-external-name-backup-restore/input/v1beta1"
	"github.com/crossplane/function-sdk-go/errors"
	"github.com/crossplane/function-sdk-go/logging"
	fnv1 "github.com/crossplane/function-sdk-go/proto/v1"
	"github.com/crossplane/function-sdk-go/request"
	"github.com/crossplane/function-sdk-go/response"
)

const (
	// StoredExternalNameAnnotation tracks the external name value stored in the store
	StoredExternalNameAnnotation = "fn.crossplane.io/stored-external-name"

	// ExternalNameStoredAnnotation tracks when the external name was stored with timestamp
	ExternalNameStoredAnnotation = "fn.crossplane.io/external-name-stored"

	// ExternalNameDeletedAnnotation tracks when the external name was deleted with timestamp
	ExternalNameDeletedAnnotation = "fn.crossplane.io/external-name-deleted"

	// ExternalNameRestoredAnnotation tracks when the external name was restored with timestamp
	ExternalNameRestoredAnnotation = "fn.crossplane.io/external-name-restored"

	// StoredResourceNameAnnotation tracks the resource name (metadata.name) stored in the store
	StoredResourceNameAnnotation = "fn.crossplane.io/stored-resource-name"

	// ResourceNameStoredAnnotation tracks when the resource name was stored with timestamp
	ResourceNameStoredAnnotation = "fn.crossplane.io/resource-name-stored"

	// ResourceNameRestoredAnnotation tracks when the resource name was restored with timestamp
	ResourceNameRestoredAnnotation = "fn.crossplane.io/resource-name-restored"

	// EnableExternalStoreAnnotation on XR enables external store loading and storing
	EnableExternalStoreAnnotation = "fn.crossplane.io/enable-external-store"

	// PurgeExternalStoreAnnotation on XR purges all stored external names for this composition
	PurgeExternalStoreAnnotation = "fn.crossplane.io/purge-external-store"

	// ClusterIDAnnotation specifies the cluster ID for external name storage
	ClusterIDAnnotation = "fn.crossplane.io/cluster-id"
	// StoreTypeAnnotation specifies the type of external store to use
	StoreTypeAnnotation = "fn.crossplane.io/store-type"
	// DynamoDBTableAnnotation specifies the DynamoDB table name
	DynamoDBTableAnnotation = "fn.crossplane.io/dynamodb-table"
	// DynamoDBRegionAnnotation specifies the DynamoDB region
	DynamoDBRegionAnnotation = "fn.crossplane.io/dynamodb-region"
	// BackupScopeAnnotation specifies which resources to backup
	BackupScopeAnnotation = "fn.crossplane.io/backup-scope"

	// ConfigMapNamespaceAnnotation specifies the namespace for ConfigMap store
	ConfigMapNamespaceAnnotation = "fn.crossplane.io/configmap-namespace"

	// OverrideKindAnnotation allows overriding the XR kind used in composition key lookup
	// This is useful for migrations where the XR kind changes between versions
	OverrideKindAnnotation = "fn.crossplane.io/override-kind"

	// OverrideNamespaceAnnotation allows overriding the namespace used in composition key lookup
	// This is useful for migrations from cluster-scoped to namespaced XRs
	OverrideNamespaceAnnotation = "fn.crossplane.io/override-namespace"

	// RequireRestoreAnnotation when set to "true" will fail the function if no external names
	// can be restored from the store. This prevents accidental resource creation during migrations
	// when override annotations are misconfigured.
	RequireRestoreAnnotation = "fn.crossplane.io/restore-only"

	// BackupScopeOrphaned processes only orphaned resources
	BackupScopeOrphaned = "orphaned"
	// BackupScopeAll processes all resources regardless of policy
	BackupScopeAll = "all"

	// DeletionPolicyDelete represents the "Delete" deletion policy value
	DeletionPolicyDelete = "Delete"
	// DeletionPolicyOrphan represents the "Orphan" deletion policy value
	DeletionPolicyOrphan = "Orphan"
)

// Function returns whatever response you ask it to.
type Function struct {
	fnv1.UnimplementedFunctionRunnerServiceServer

	log logging.Logger
}

// NewFunction creates a new Function
func NewFunction(_ context.Context, log logging.Logger) *Function {
	return &Function{
		log: log,
	}
}

// FunctionConfig holds all configuration for the function
type FunctionConfig struct {
	ClusterID          string
	StoreType          string
	DynamoDBTable      string
	DynamoDBRegion     string
	ConfigMapNamespace string
	BackupScope        string
}

// getConfigFromAnnotations extracts configuration from XR annotations with defaults
func getConfigFromAnnotations(req *fnv1.RunFunctionRequest, log logging.Logger) *FunctionConfig {
	config := &FunctionConfig{
		ClusterID:          "default",
		StoreType:          "awsdynamodb",
		DynamoDBTable:      "external-name-backup",
		DynamoDBRegion:     "us-west-2",
		ConfigMapNamespace: "crossplane-system",
		BackupScope:        BackupScopeOrphaned,
	}

	// Check observed composite first for XR annotations (the source of truth),
	// then fall back to desired composite for each annotation if not found.
	// This is important because previous pipeline steps may create a desired composite
	// without preserving the original XR annotations.
	observedComposite := req.GetObserved().GetComposite().GetResource()
	desiredComposite := req.GetDesired().GetComposite().GetResource()

	// Helper to get annotation from observed first, then desired as fallback
	getConfigAnnotation := func(annotation string) string {
		if observedComposite != nil {
			if val := getAnnotationValue(observedComposite, annotation); val != "" {
				return val
			}
		}
		if desiredComposite != nil {
			if val := getAnnotationValue(desiredComposite, annotation); val != "" {
				return val
			}
		}
		return ""
	}

	if clusterID := getConfigAnnotation(ClusterIDAnnotation); clusterID != "" {
		config.ClusterID = clusterID
	}
	if storeType := getConfigAnnotation(StoreTypeAnnotation); storeType != "" {
		config.StoreType = storeType
	}
	if dynamoDBTable := getConfigAnnotation(DynamoDBTableAnnotation); dynamoDBTable != "" {
		config.DynamoDBTable = dynamoDBTable
	}
	if dynamoDBRegion := getConfigAnnotation(DynamoDBRegionAnnotation); dynamoDBRegion != "" {
		config.DynamoDBRegion = dynamoDBRegion
	}
	if backupScope := getConfigAnnotation(BackupScopeAnnotation); backupScope != "" {
		config.BackupScope = backupScope
	}
	if configMapNamespace := getConfigAnnotation(ConfigMapNamespaceAnnotation); configMapNamespace != "" {
		config.ConfigMapNamespace = configMapNamespace
	}

	log.Info("Configuration loaded from XR annotations",
		"cluster-id", config.ClusterID,
		"store-type", config.StoreType,
		"dynamodb-table", config.DynamoDBTable,
		"dynamodb-region", config.DynamoDBRegion,
		"configmap-namespace", config.ConfigMapNamespace,
		"backup-scope", config.BackupScope)

	return config
}

// getAWSCredentials retrieves AWS credentials from the request (returns nil if not found)
// Supports both JSON format and AWS CLI INI format
func getAWSCredentials(req *fnv1.RunFunctionRequest) (map[string]string, error) {
	var awsCreds map[string]string
	rawCreds := req.GetCredentials()

	if credsData, ok := rawCreds["aws-creds"]; ok {
		credsData := credsData.GetCredentialData().GetData()
		if credsBytes, ok := credsData["credentials"]; ok {
			credsString := string(credsBytes)

			// Try JSON format first (for compatibility with Azure Resource Graph pattern)
			err := json.Unmarshal(credsBytes, &awsCreds)
			if err == nil {
				return awsCreds, nil
			}

			// If JSON parsing fails, try AWS CLI INI format
			awsCreds, err = parseAWSINICredentials(credsString)
			if err != nil {
				return nil, errors.Wrap(err, "cannot parse credentials (tried both JSON and AWS CLI INI formats)")
			}
		}
	}
	// Return nil if credentials not found - this will trigger fallback to default credential chain
	return awsCreds, nil
}

// parseAWSINICredentials parses AWS CLI INI format credentials
//
//nolint:gocyclo // complex credential parsing logic
func parseAWSINICredentials(iniContent string) (map[string]string, error) {
	creds := make(map[string]string)
	lines := strings.Split(iniContent, "\n")

	inDefaultSection := false
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		// Check for section headers
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			inDefaultSection = (line == "[default]")
			continue
		}

		// Only process lines in the [default] section
		if !inDefaultSection {
			continue
		}

		// Parse key=value pairs
		if strings.Contains(line, "=") {
			parts := strings.SplitN(line, "=", 2)
			if len(parts) == 2 {
				key := strings.TrimSpace(parts[0])
				value := strings.TrimSpace(parts[1])

				// Map AWS CLI keys to our expected JSON keys
				switch key {
				case "aws_access_key_id":
					creds["accessKeyId"] = value
				case "aws_secret_access_key":
					creds["secretAccessKey"] = value
				case "aws_session_token":
					creds["sessionToken"] = value
				}
			}
		}
	}

	// Validate that we have the required keys
	if creds["accessKeyId"] == "" || creds["secretAccessKey"] == "" {
		return nil, errors.New("missing required AWS credentials (accessKeyId and secretAccessKey)")
	}

	return creds, nil
}

// shouldEnableExternalStore checks if XR has annotation to enable external store operations
func shouldEnableExternalStore(req *fnv1.RunFunctionRequest, log logging.Logger) bool {
	// Check desired composite first
	if desiredComposite := req.GetDesired().GetComposite().GetResource(); desiredComposite != nil {
		if enable := checkEnableAnnotation(desiredComposite, log, "desired"); enable {
			return true
		}
	}

	// Check observed composite as fallback
	if observedComposite := req.GetObserved().GetComposite().GetResource(); observedComposite != nil {
		if enable := checkEnableAnnotation(observedComposite, log, "observed"); enable {
			return true
		}
	}

	return false
}

// getAnnotationValue gets an annotation value from a composite resource
func getAnnotationValue(composite *structpb.Struct, annotation string) string {
	if fields := composite.GetFields(); fields != nil {
		if metadata := fields["metadata"]; metadata != nil {
			if metadataStruct := metadata.GetStructValue(); metadataStruct != nil {
				if annotations := metadataStruct.GetFields()["annotations"]; annotations != nil {
					if annotationsStruct := annotations.GetStructValue(); annotationsStruct != nil {
						if value := annotationsStruct.GetFields()[annotation]; value != nil {
							return value.GetStringValue()
						}
					}
				}
			}
		}
	}
	return ""
}

// getMetadataName gets the metadata.name from a resource struct
func getMetadataName(resource *structpb.Struct) string {
	if fields := resource.GetFields(); fields != nil {
		if metadata := fields["metadata"]; metadata != nil {
			if metadataStruct := metadata.GetStructValue(); metadataStruct != nil {
				if name := metadataStruct.GetFields()["name"]; name != nil {
					return name.GetStringValue()
				}
			}
		}
	}
	return ""
}

// getMetadataNameFromResource gets metadata.name from a resource, checking both desired and observed as fallback
func getMetadataNameFromResource(req *fnv1.RunFunctionRequest, resourceName string) string {
	// Check desired resource first
	if desiredResource, exists := req.GetDesired().GetResources()[resourceName]; exists {
		if resourceStruct := desiredResource.GetResource(); resourceStruct != nil {
			if name := getMetadataName(resourceStruct); name != "" {
				return name
			}
		}
	}

	// Fallback to observed resource if not found in desired
	if observedResource, exists := req.GetObserved().GetResources()[resourceName]; exists {
		if resourceStruct := observedResource.GetResource(); resourceStruct != nil {
			return getMetadataName(resourceStruct)
		}
	}

	return ""
}

// getAnnotationValueFromResource gets an annotation value from a resource, checking both desired and observed as fallback
func getAnnotationValueFromResource(req *fnv1.RunFunctionRequest, resourceName string, annotation string) string {
	// Check desired resource first
	if desiredResource, exists := req.GetDesired().GetResources()[resourceName]; exists {
		if resourceStruct := desiredResource.GetResource(); resourceStruct != nil {
			if value := getAnnotationValue(resourceStruct, annotation); value != "" {
				return value
			}
		}
	}

	// Fallback to observed resource if not found in desired
	if observedResource, exists := req.GetObserved().GetResources()[resourceName]; exists {
		if resourceStruct := observedResource.GetResource(); resourceStruct != nil {
			return getAnnotationValue(resourceStruct, annotation)
		}
	}

	return ""
}

// checkEnableAnnotation checks for enable annotation in a composite resource
func checkEnableAnnotation(composite *structpb.Struct, log logging.Logger, source string) bool {
	enableValue := getAnnotationValue(composite, EnableExternalStoreAnnotation)
	if enableValue == "true" || enableValue == "yes" || enableValue == "1" {
		log.Info("External store operations enabled by XR annotation",
			"source", source,
			"annotation", EnableExternalStoreAnnotation,
			"value", enableValue)
		return true
	}
	return false
}

// shouldPurgeExternalStore checks if XR has annotation to purge external store data
func shouldPurgeExternalStore(req *fnv1.RunFunctionRequest, log logging.Logger) bool {
	// Check desired composite first
	if desiredComposite := req.GetDesired().GetComposite().GetResource(); desiredComposite != nil {
		if purge := checkPurgeAnnotation(desiredComposite, log, "desired"); purge {
			return true
		}
	}

	// Check observed composite as fallback
	if observedComposite := req.GetObserved().GetComposite().GetResource(); observedComposite != nil {
		if purge := checkPurgeAnnotation(observedComposite, log, "observed"); purge {
			return true
		}
	}

	return false
}

// checkPurgeAnnotation checks for purge annotation in a composite resource
func checkPurgeAnnotation(composite *structpb.Struct, log logging.Logger, source string) bool {
	purgeValue := getAnnotationValue(composite, PurgeExternalStoreAnnotation)
	if purgeValue == "true" || purgeValue == "yes" || purgeValue == "1" {
		log.Info("External store purge requested by XR annotation",
			"source", source,
			"annotation", PurgeExternalStoreAnnotation,
			"value", purgeValue)
		return true
	}
	return false
}

// shouldRequireRestore checks if the require-restore annotation is set to "true"
// When enabled, the function will fail if no external names can be restored from the store
// This prevents accidental resource creation during migrations when override annotations are misconfigured
func shouldRequireRestore(req *fnv1.RunFunctionRequest) bool {
	// Check observed first (source of truth for XR annotations)
	if observedComposite := req.GetObserved().GetComposite().GetResource(); observedComposite != nil {
		if val := getAnnotationValue(observedComposite, RequireRestoreAnnotation); val == "true" {
			return true
		}
	}
	// Check desired as fallback
	if desiredComposite := req.GetDesired().GetComposite().GetResource(); desiredComposite != nil {
		if val := getAnnotationValue(desiredComposite, RequireRestoreAnnotation); val == "true" {
			return true
		}
	}
	return false
}

// mergeObservedAnnotations ensures desired resource has annotation structure and merges observed annotations
//
//nolint:gocyclo // complex annotation merging logic
func (f *Function) mergeObservedAnnotations(req *fnv1.RunFunctionRequest, resourceName string, desiredResource *fnv1.Resource) *structpb.Struct {
	resourceStruct := desiredResource.GetResource()
	if resourceStruct == nil || resourceStruct.GetFields() == nil {
		return nil
	}

	fields := resourceStruct.GetFields()

	// Ensure metadata exists
	if fields["metadata"] == nil {
		fields["metadata"] = &structpb.Value{
			Kind: &structpb.Value_StructValue{
				StructValue: &structpb.Struct{
					Fields: make(map[string]*structpb.Value),
				},
			},
		}
	}

	metadata := fields["metadata"]
	if metadata == nil {
		return nil
	}

	metadataStruct := metadata.GetStructValue()
	if metadataStruct == nil {
		return nil
	}

	metadataFields := metadataStruct.GetFields()

	// Ensure annotations exist
	if metadataFields["annotations"] == nil {
		metadataFields["annotations"] = &structpb.Value{
			Kind: &structpb.Value_StructValue{
				StructValue: &structpb.Struct{
					Fields: make(map[string]*structpb.Value),
				},
			},
		}
	}

	annotationsStruct := metadataFields["annotations"].GetStructValue()
	if annotationsStruct == nil {
		return nil
	}

	if annotationsStruct.Fields == nil {
		annotationsStruct.Fields = make(map[string]*structpb.Value)
	}

	// Merge observed annotations
	if observedResource, exists := req.GetObserved().GetResources()[resourceName]; exists {
		if observedResourceStruct := observedResource.GetResource(); observedResourceStruct != nil {
			if observedFields := observedResourceStruct.GetFields(); observedFields != nil {
				if observedMetadata := observedFields["metadata"]; observedMetadata != nil {
					if observedMetadataStruct := observedMetadata.GetStructValue(); observedMetadataStruct != nil {
						if observedAnnotations := observedMetadataStruct.GetFields()["annotations"]; observedAnnotations != nil {
							if observedAnnotationsStruct := observedAnnotations.GetStructValue(); observedAnnotationsStruct != nil {
								observedFields := observedAnnotationsStruct.GetFields()

								// Copy all observed annotations to desired annotations
								for key, value := range observedFields {
									annotationsStruct.Fields[key] = value
								}
							}
						}
					}
				}
			}
		}
	}

	return annotationsStruct
}

// removeTrackingAnnotationsFromObserved removes tracking annotations from observed resource
// to prevent them from being merged back into desired state after deletion
func (f *Function) removeTrackingAnnotationsFromObserved(req *fnv1.RunFunctionRequest, resourceName string) {
	if observedResource, exists := req.GetObserved().GetResources()[resourceName]; exists {
		if resourceStruct := observedResource.GetResource(); resourceStruct != nil {
			if fields := resourceStruct.GetFields(); fields != nil {
				if metadata := fields["metadata"]; metadata != nil {
					if metadataStruct := metadata.GetStructValue(); metadataStruct != nil {
						if annotations := metadataStruct.GetFields()["annotations"]; annotations != nil {
							if annotationsStruct := annotations.GetStructValue(); annotationsStruct != nil {
								fields := annotationsStruct.GetFields()
								if fields != nil {
									// Remove external name tracking annotations
									delete(fields, StoredExternalNameAnnotation)
									delete(fields, ExternalNameStoredAnnotation)
									// Remove resource name tracking annotations
									delete(fields, StoredResourceNameAnnotation)
									delete(fields, ResourceNameStoredAnnotation)
								}
							}
						}
					}
				}
			}
		}
	}
}

// shouldDeleteFromExternalStoreWithFallback checks deletion criteria in desired resource, falling back to observed
//
//nolint:gocyclo // complex deletion criteria logic
func (f *Function) shouldDeleteFromExternalStoreWithFallback(desiredFields, observedFields map[string]*structpb.Value, resourceName string) bool {
	// Helper function to check spec fields for deletion criteria
	checkDeletionCriteria := func(fields map[string]*structpb.Value) (hasDeletePolicy bool, hasDeleteManagementPolicy bool, hasSpec bool) {
		if spec := fields["spec"]; spec != nil {
			if specStruct := spec.GetStructValue(); specStruct != nil {
				hasSpec = true
				specFields := specStruct.GetFields()

				// Check deletionPolicy is "Delete"
				if deletionPolicy := specFields["deletionPolicy"]; deletionPolicy != nil {
					if deletionPolicy.GetStringValue() == DeletionPolicyDelete {
						hasDeletePolicy = true
					}
				}

				// Check managementPolicies contains "*" or "Delete"
				if managementPolicies := specFields["managementPolicies"]; managementPolicies != nil {
					if listValue := managementPolicies.GetListValue(); listValue != nil {
						for _, item := range listValue.GetValues() {
							policy := item.GetStringValue()
							if policy == "*" || policy == DeletionPolicyDelete {
								hasDeleteManagementPolicy = true
								break
							}
						}
					}
				}
			}
		}
		return
	}

	// Check desired resource first
	hasDeletePolicy, hasDeleteManagementPolicy, hasDesiredSpec := checkDeletionCriteria(desiredFields)

	// Fall back to observed resource if desired doesn't have spec
	if !hasDesiredSpec && len(observedFields) > 0 {
		hasDeletePolicy, hasDeleteManagementPolicy, _ = checkDeletionCriteria(observedFields)
	}

	shouldDelete := hasDeletePolicy && hasDeleteManagementPolicy
	f.log.Info("Checked deletion criteria",
		"resource", resourceName,
		"deletion-policy-delete", hasDeletePolicy,
		"management-policies-delete", hasDeleteManagementPolicy,
		"should-delete", shouldDelete)

	return shouldDelete
}

// shouldProcessResource determines if a resource should be processed based on backup scope
//
//nolint:gocyclo // complex backup scope logic
func (f *Function) shouldProcessResource(fields map[string]*structpb.Value, resourceName string, backupScope string) bool {
	if backupScope == BackupScopeAll {
		// Process all resources regardless of deletion policy
		return true
	}

	if backupScope == BackupScopeOrphaned {
		// Check spec.deletionPolicy and spec.managementPolicies
		if spec := fields["spec"]; spec != nil {
			if specStruct := spec.GetStructValue(); specStruct != nil {
				specFields := specStruct.GetFields()

				// Check deletionPolicy is "Orphan"
				hasOrphanPolicy := false
				if deletionPolicy := specFields["deletionPolicy"]; deletionPolicy != nil {
					if deletionPolicy.GetStringValue() == DeletionPolicyOrphan {
						hasOrphanPolicy = true
					}
				}

				// Check managementPolicies does not contain "*" or "Delete"
				managementPoliciesOk := true
				if managementPolicies := specFields["managementPolicies"]; managementPolicies != nil {
					if listValue := managementPolicies.GetListValue(); listValue != nil {
						for _, item := range listValue.GetValues() {
							policy := item.GetStringValue()
							if policy == "*" || policy == DeletionPolicyDelete {
								managementPoliciesOk = false
								break
							}
						}
					}
				}

				shouldProcess := hasOrphanPolicy || managementPoliciesOk
				f.log.Info("Checked orphan criteria",
					"resource", resourceName,
					"deletion-policy-orphan", hasOrphanPolicy,
					"management-policies-ok", managementPoliciesOk,
					"should-process", shouldProcess)

				return shouldProcess
			}
		}

		f.log.Info("Resource missing spec, skipping in orphaned backup scope", "resource", resourceName)
		return false
	}

	f.log.Info("Unknown backup scope, defaulting to process", "scope", backupScope, "resource", resourceName)
	return true
}

// RunFunction runs the Function.
//
//nolint:gocyclo // main function with complex orchestration logic
func (f *Function) RunFunction(ctx context.Context, req *fnv1.RunFunctionRequest) (*fnv1.RunFunctionResponse, error) {
	f.log.Info("Running function", "tag", req.GetMeta().GetTag())

	rsp := response.To(req, response.DefaultTTL)

	// Check if external store operations should be enabled
	if !shouldEnableExternalStore(req, f.log) {
		f.log.Info("Skipping all external store operations - not enabled by XR annotation")

		// Parse function input (for consistency)
		in := &v1beta1.Input{}
		if err := request.GetInput(req, in); err != nil {
			response.Fatal(rsp, errors.Wrapf(err, "cannot get Function input from %T", req))
			return rsp, nil
		}

		response.Normalf(rsp, "Processed %d desired and %d observed resources (external store disabled)",
			len(req.GetDesired().GetResources()),
			len(req.GetObserved().GetResources()))

		response.ConditionTrue(rsp, "FunctionSuccess", "Success").
			TargetCompositeAndClaim()

		return rsp, nil
	}

	// Get configuration from XR annotations
	config := getConfigFromAnnotations(req, f.log)

	// Get AWS credentials for DynamoDB store (optional - will fallback to default credential chain)
	var awsCreds map[string]string
	var err error
	if config.StoreType == "awsdynamodb" {
		awsCreds, err = getAWSCredentials(req)
		if err != nil {
			response.Fatal(rsp, errors.Wrapf(err, "failed to parse AWS credentials"))
			return rsp, nil
		}
	}

	// Initialize external store based on configuration
	var store ExternalNameStore

	switch config.StoreType {
	case "awsdynamodb":
		store, err = NewDynamoDBStore(ctx, f.log, config.DynamoDBTable, config.DynamoDBRegion, awsCreds)
		if err != nil {
			response.Fatal(rsp, errors.Wrapf(err, "failed to initialize DynamoDB store"))
			return rsp, nil
		}
	case "mock":
		store, err = NewMockStore(ctx, f.log)
		if err != nil {
			response.Fatal(rsp, errors.Wrapf(err, "failed to initialize Mock store"))
			return rsp, nil
		}
	case "k8sconfigmap":
		store, err = NewConfigMapStore(ctx, f.log, config.ConfigMapNamespace)
		if err != nil {
			response.Fatal(rsp, errors.Wrapf(err, "failed to initialize ConfigMap store"))
			return rsp, nil
		}

	default:
		response.Fatal(rsp, errors.Errorf("unsupported external store type: %s (supported types: 'awsdynamodb', 'mock', 'k8sconfigmap')", config.StoreType))
		return rsp, nil
	}

	clusterID := config.ClusterID
	backupScope := config.BackupScope
	f.log.Info("Using configuration for function execution",
		"cluster-id", clusterID,
		"backup-scope", backupScope,
		"store-type", config.StoreType)

	// Extract claim and XR information from composite resource
	var xrAPIVersion, xrKind, xrName, xrNamespace, claimNamespace, claimName string

	// Use observed composite for metadata extraction (it has complete info)
	if observedComposite := req.GetObserved().GetComposite().GetResource(); observedComposite != nil {
		if fields := observedComposite.GetFields(); fields != nil {
			if apiVersion := fields["apiVersion"]; apiVersion != nil {
				xrAPIVersion = apiVersion.GetStringValue()
			}
			if kind := fields["kind"]; kind != nil {
				xrKind = kind.GetStringValue()
			}
			if metadata := fields["metadata"]; metadata != nil {
				if metadataStruct := metadata.GetStructValue(); metadataStruct != nil {
					if name := metadataStruct.GetFields()["name"]; name != nil {
						xrName = name.GetStringValue()
					}
					// Extract XR namespace (for namespaced XRs)
					if ns := metadataStruct.GetFields()["namespace"]; ns != nil {
						xrNamespace = ns.GetStringValue()
					}
					// Extract claim info from labels
					if labels := metadataStruct.GetFields()["labels"]; labels != nil {
						if labelsStruct := labels.GetStructValue(); labelsStruct != nil {
							if claimNs := labelsStruct.GetFields()["crossplane.io/claim-namespace"]; claimNs != nil {
								claimNamespace = claimNs.GetStringValue()
							}
							if claimN := labelsStruct.GetFields()["crossplane.io/claim-name"]; claimN != nil {
								claimName = claimN.GetStringValue()
							}
						}
					}
				}
			}
		}
	}

	// Set defaults if claim info not found
	// For namespaced XRs without claims, use XR namespace as fallback
	if claimNamespace == "" {
		if xrNamespace != "" {
			claimNamespace = xrNamespace
			f.log.Info("Using XR namespace as claim-namespace for namespaced XR without claim",
				"xr-namespace", xrNamespace)
		} else {
			claimNamespace = "none"
		}
	}
	if claimName == "" {
		claimName = "none"
	}

	f.log.Info("Extracted composition information",
		"xr-api-version", xrAPIVersion,
		"xr-kind", xrKind,
		"xr-name", xrName,
		"xr-namespace", xrNamespace,
		"claim-namespace", claimNamespace,
		"claim-name", claimName)

	// Check for kind override annotation (useful for migrations where XR kind changes)
	// Check desired first, then observed as fallback
	kindForKey := xrKind
	var overrideKind string
	if desiredComposite := req.GetDesired().GetComposite().GetResource(); desiredComposite != nil {
		overrideKind = getAnnotationValue(desiredComposite, OverrideKindAnnotation)
	}
	if overrideKind == "" {
		if observedComposite := req.GetObserved().GetComposite().GetResource(); observedComposite != nil {
			overrideKind = getAnnotationValue(observedComposite, OverrideKindAnnotation)
		}
	}
	if overrideKind != "" {
		f.log.Info("Using override-kind for composition key lookup",
			"original-kind", xrKind,
			"override-kind", overrideKind)
		kindForKey = overrideKind
	}

	// Check for namespace override annotation (useful for migrations from cluster-scoped to namespaced)
	namespaceForKey := claimNamespace
	var overrideNamespace string
	if desiredComposite := req.GetDesired().GetComposite().GetResource(); desiredComposite != nil {
		overrideNamespace = getAnnotationValue(desiredComposite, OverrideNamespaceAnnotation)
	}
	if overrideNamespace == "" {
		if observedComposite := req.GetObserved().GetComposite().GetResource(); observedComposite != nil {
			overrideNamespace = getAnnotationValue(observedComposite, OverrideNamespaceAnnotation)
		}
	}
	if overrideNamespace != "" {
		f.log.Info("Using override-namespace for composition key lookup",
			"original-namespace", claimNamespace,
			"override-namespace", overrideNamespace)
		namespaceForKey = overrideNamespace
	}

	// Parse function input (for future extensibility)
	in := &v1beta1.Input{}
	if err := request.GetInput(req, in); err != nil {
		response.Fatal(rsp, errors.Wrapf(err, "cannot get Function input from %T", req))
		return rsp, nil
	}

	// Create composition key: {namespace}/{claimName}/{apiVersionOfXr}/{kindOfXr}/{metadata.name of XR}
	// Note: Uses namespaceForKey and kindForKey which may be overridden by annotations
	compositionKey := fmt.Sprintf("%s/%s/%s/%s/%s", namespaceForKey, claimName, xrAPIVersion, kindForKey, xrName)

	// Compute timestamp once for this operation
	timestamp := time.Now().UTC().Format(time.RFC3339)

	// Check if external store should be purged for this composition
	if shouldPurgeExternalStore(req, f.log) {
		f.log.Info("Purging external store for composition", "composition-key", compositionKey)
		err := store.Purge(ctx, clusterID, compositionKey)
		if err != nil {
			response.Fatal(rsp, errors.Wrapf(err, "failed to purge external store"))
			return rsp, nil
		}
		f.log.Info("Successfully purged external store for composition", "composition-key", compositionKey)

		// Parse function input (for consistency)
		in := &v1beta1.Input{}
		if err := request.GetInput(req, in); err != nil {
			response.Fatal(rsp, errors.Wrapf(err, "cannot get Function input from %T", req))
			return rsp, nil
		}

		response.Normalf(rsp, "Purged external store for composition %q", compositionKey)
		response.ConditionTrue(rsp, "FunctionSuccess", "Success").
			TargetCompositeAndClaim()

		return rsp, nil
	}

	// Load existing resource data from pre-initialized store
	loadedResources, err := store.Load(ctx, clusterID, compositionKey)
	if err != nil {
		response.Fatal(rsp, errors.Wrapf(err, "failed to load resource data from store"))
		return rsp, nil
	}

	// Safety check: if require-restore is set and no data found, fail to prevent accidental creation
	requireRestore := shouldRequireRestore(req)
	if requireRestore && len(loadedResources) == 0 {
		response.Fatal(rsp, errors.Errorf(
			"require-restore is enabled but no resource data found in store for composition key %q. "+
				"Check that override-kind and override-namespace annotations are correct, or remove require-restore annotation.",
			compositionKey))
		return rsp, nil
	}

	// Convert to nested structure for processing
	resourceDataStore := map[string]map[string]ResourceData{
		compositionKey: loadedResources,
	}
	f.log.Info("Loaded resource data from store",
		"composition-key", compositionKey,
		"loaded-count", len(loadedResources),
		"require-restore", requireRestore)

	// Track only NEW resource data that should be stored (not restored ones)
	newResourceData := make(map[string]ResourceData)

	// Pre-calculate shouldProcess for all resources to avoid redundant checks
	resourceShouldProcess := make(map[string]bool)

	// First pass: Check all desired resources for deletion from external store
	// This needs to happen before restoration to prevent restoring resources that should be deleted
	for name, resource := range req.GetDesired().GetResources() {
		resourceStruct := resource.GetResource()
		if resourceStruct != nil && resourceStruct.GetFields() != nil {
			fields := resourceStruct.GetFields()

			// Use pipeline resource name as the stable identifier
			resourceName := name

			// Check if this desired resource should be deleted from external store
			// This check applies to ALL resources, not just those that meet orphan criteria
			// First check desired resource for stored-external-name annotation, then fallback to observed
			shouldDelete := false

			// Check for stored-external-name annotation (desired resource first, then observed as fallback)
			hasStoredAnnotation := getAnnotationValueFromResource(req, resourceName, StoredExternalNameAnnotation) != ""

			// Only check deletion criteria if we found the stored annotation somewhere
			if hasStoredAnnotation {
				// Check deletion policy and management policies, preferring desired over observed
				observedFields := make(map[string]*structpb.Value)
				if observedResource, exists := req.GetObserved().GetResources()[resourceName]; exists {
					if observedStruct := observedResource.GetResource(); observedStruct != nil && observedStruct.GetFields() != nil {
						observedFields = observedStruct.GetFields()
					}
				}
				shouldDelete = f.shouldDeleteFromExternalStoreWithFallback(fields, observedFields, resourceName)
			}

			if shouldDelete {
				resourceKey := resourceName

				f.log.Info("Processing deletion for desired resource",
					"resource", resourceName,
					"resource-key", resourceKey)

				// Delete from store
				err := store.DeleteResource(ctx, clusterID, compositionKey, resourceKey)
				if err != nil {
					f.log.Info("Failed to delete resource from store",
						"resource", resourceName,
						"error", err.Error())
				} else {
					f.log.Info("Deleted resource from store",
						"resource", resourceName,
						"resource-key", resourceKey)

					// Remove from local cache so it doesn't get re-added during save
					if compositionData, exists := resourceDataStore[compositionKey]; exists {
						delete(compositionData, resourceKey)
					}

					// Remove tracking annotations from observed resource to prevent them from being preserved
					f.removeTrackingAnnotationsFromObserved(req, resourceName)
				}

				// Remove tracking annotations and add deletion timestamp to desired resource
				// Ensure metadata exists
				if fields["metadata"] == nil {
					fields["metadata"] = &structpb.Value{
						Kind: &structpb.Value_StructValue{
							StructValue: &structpb.Struct{
								Fields: make(map[string]*structpb.Value),
							},
						},
					}
				}

				if metadata := fields["metadata"]; metadata != nil {
					if metadataStruct := metadata.GetStructValue(); metadataStruct != nil {
						metadataFields := metadataStruct.GetFields()

						// Ensure annotations exist
						if metadataFields["annotations"] == nil {
							metadataFields["annotations"] = &structpb.Value{
								Kind: &structpb.Value_StructValue{
									StructValue: &structpb.Struct{
										Fields: make(map[string]*structpb.Value),
									},
								},
							}
						}

						if annotationsStruct := metadataFields["annotations"].GetStructValue(); annotationsStruct != nil {
							// Ensure the Fields map exists
							fields := annotationsStruct.GetFields()
							if fields == nil {
								annotationsStruct.Fields = make(map[string]*structpb.Value)
								fields = annotationsStruct.GetFields()
							}

							// Remove tracking annotations
							delete(fields, StoredExternalNameAnnotation)
							delete(fields, ExternalNameStoredAnnotation)

							// Add deletion timestamp
							fields[ExternalNameDeletedAnnotation] = &structpb.Value{
								Kind: &structpb.Value_StringValue{
									StringValue: timestamp,
								},
							}

							f.log.Info("Removed tracking annotations and added deletion timestamp",
								"resource", resourceName,
								"timestamp", timestamp)
						}
					}
				}
			}
		}
	}

	// After all deletions, check if the composition is empty and clean it up
	if compositionData, exists := resourceDataStore[compositionKey]; exists && len(compositionData) == 0 {
		f.log.Info("Composition has no resource data left, purging entire composition from store",
			"composition-key", compositionKey)

		err := store.Purge(ctx, clusterID, compositionKey)
		if err != nil {
			f.log.Info("Failed to purge empty composition from store",
				"composition-key", compositionKey,
				"error", err.Error())
		} else {
			f.log.Info("Successfully purged empty composition from store",
				"composition-key", compositionKey)

			// Remove from local cache
			delete(resourceDataStore, compositionKey)
		}
	}

	// Second pass: Iterate through all desired resources from previous pipeline steps for restoration
	for name, resource := range req.GetDesired().GetResources() {
		resourceStruct := resource.GetResource()
		if resourceStruct != nil && resourceStruct.GetFields() != nil {
			fields := resourceStruct.GetFields()

			// Use pipeline resource name as the stable identifier
			resourceName := name

			var apiVersion, kind string
			if av := fields["apiVersion"]; av != nil {
				apiVersion = av.GetStringValue()
			}
			if k := fields["kind"]; k != nil {
				kind = k.GetStringValue()
			}

			f.log.Info("Desired resource",
				"resource-name", resourceName,
				"apiVersion", apiVersion,
				"kind", kind,
			)

			// Check if this resource should be processed for external store operations
			// When requireRestore is true, we always attempt restore but never backup
			shouldProcess := f.shouldProcessResource(fields, resourceName, backupScope)
			if !requireRestore {
				resourceShouldProcess[resourceName] = shouldProcess // Only cache for backup when not in restore mode
			}
			// When requireRestore is false, skip if backup scope doesn't allow processing
			// When requireRestore is true, always continue to attempt restore
			if !shouldProcess && !requireRestore {
				f.log.Info("Skipping external store operations for desired resource due to backup scope", "resource", resourceName, "scope", backupScope)
				continue
			}

			// Check if the resource already has an external-name annotation (desired first, then observed as fallback)
			existingExternalName := getAnnotationValueFromResource(req, resourceName, "crossplane.io/external-name")
			hasExistingExternalName := existingExternalName != ""

			// Check if the resource already has a metadata.name
			existingResourceName := getMetadataNameFromResource(req, resourceName)
			hasExistingResourceName := existingResourceName != ""

			// Create key for store lookup using pipeline resource name
			resourceKey := resourceName

			// Check if we have data for this resource in our store
			if compositionData, compositionExists := resourceDataStore[compositionKey]; compositionExists {
				if storedData, resourceExists := compositionData[resourceKey]; resourceExists {
					// Ensure metadata exists before any restoration
					if fields["metadata"] == nil {
						fields["metadata"] = &structpb.Value{
							Kind: &structpb.Value_StructValue{
								StructValue: &structpb.Struct{
									Fields: make(map[string]*structpb.Value),
								},
							},
						}
					}

					if metadata := fields["metadata"]; metadata != nil {
						if metadataStruct := metadata.GetStructValue(); metadataStruct != nil {
							metadataFields := metadataStruct.GetFields()

							// Restore resource name (metadata.name) if not already set
							if !hasExistingResourceName && storedData.ResourceName != "" {
								f.log.Info("Restoring resource name from store",
									"resource", resourceName,
									"resource-name", storedData.ResourceName,
									"timestamp", timestamp,
								)

								// Set metadata.name
								metadataFields["name"] = &structpb.Value{
									Kind: &structpb.Value_StringValue{
										StringValue: storedData.ResourceName,
									},
								}

								// Ensure annotations exist for tracking
								if metadataFields["annotations"] == nil {
									metadataFields["annotations"] = &structpb.Value{
										Kind: &structpb.Value_StructValue{
											StructValue: &structpb.Struct{
												Fields: make(map[string]*structpb.Value),
											},
										},
									}
								}

								if annotationsStruct := metadataFields["annotations"].GetStructValue(); annotationsStruct != nil {
									if annotationsStruct.Fields == nil {
										annotationsStruct.Fields = make(map[string]*structpb.Value)
									}

									// Add tracking annotations for resource name
									annotationsStruct.Fields[StoredResourceNameAnnotation] = &structpb.Value{
										Kind: &structpb.Value_StringValue{
											StringValue: storedData.ResourceName,
										},
									}
									annotationsStruct.Fields[ResourceNameRestoredAnnotation] = &structpb.Value{
										Kind: &structpb.Value_StringValue{
											StringValue: timestamp,
										},
									}
								}
							}

							// Restore external name if not already set
							if !hasExistingExternalName && storedData.ExternalName != "" {
								f.log.Info("Restoring external-name from store",
									"resource", resourceName,
									"external-name", storedData.ExternalName,
									"timestamp", timestamp,
								)

								// Ensure annotations exist
								if metadataFields["annotations"] == nil {
									metadataFields["annotations"] = &structpb.Value{
										Kind: &structpb.Value_StructValue{
											StructValue: &structpb.Struct{
												Fields: make(map[string]*structpb.Value),
											},
										},
									}
								}

								if annotationsStruct := metadataFields["annotations"].GetStructValue(); annotationsStruct != nil {
									if annotationsStruct.Fields == nil {
										annotationsStruct.Fields = make(map[string]*structpb.Value)
									}

									// Set the external-name annotation
									annotationsStruct.Fields["crossplane.io/external-name"] = &structpb.Value{
										Kind: &structpb.Value_StringValue{
											StringValue: storedData.ExternalName,
										},
									}

									// Add tracking annotation
									annotationsStruct.Fields[StoredExternalNameAnnotation] = &structpb.Value{
										Kind: &structpb.Value_StringValue{
											StringValue: storedData.ExternalName,
										},
									}

									// Add restoration timestamp annotation
									annotationsStruct.Fields[ExternalNameRestoredAnnotation] = &structpb.Value{
										Kind: &structpb.Value_StringValue{
											StringValue: timestamp,
										},
									}
								}
							}
						}
					}
				} else {
					f.log.Info("No data found in store for resource", "resource", resourceName, "composition-key", compositionKey, "resource-key", resourceKey)
					if requireRestore {
						response.Fatal(rsp, errors.Errorf(
							"require-restore is enabled but no data found in store for resource %q (composition key: %q). "+
								"All resources must have data in the store when require-restore is set.",
							resourceName, compositionKey))
						return rsp, nil
					}
				}
			} else {
				f.log.Info("No composition found in store", "composition-key", compositionKey)
				if requireRestore {
					response.Fatal(rsp, errors.Errorf(
						"require-restore is enabled but no composition data found in store for key %q. "+
							"Check that override-kind and override-namespace annotations are correct.",
						compositionKey))
					return rsp, nil
				}
			}
		}
	}

	// Iterate through all observed resources from previous pipeline steps
	f.log.Info("Checking observed resources", "count", len(req.GetObserved().GetResources()))
	for name, resource := range req.GetObserved().GetResources() {
		resourceStruct := resource.GetResource()
		if resourceStruct != nil && resourceStruct.GetFields() != nil {
			fields := resourceStruct.GetFields()

			// Use pipeline resource name as the stable identifier
			resourceName := name

			var apiVersion, kind string
			if av := fields["apiVersion"]; av != nil {
				apiVersion = av.GetStringValue()
			}
			if k := fields["kind"]; k != nil {
				kind = k.GetStringValue()
			}

			f.log.Info("Observed resource",
				"resource-name", resourceName,
				"apiVersion", apiVersion,
				"kind", kind,
			)

			// Check if this resource should be processed for external store operations
			shouldProcessForStore, exists := resourceShouldProcess[resourceName]
			if !exists {
				shouldProcessForStore = f.shouldProcessResource(fields, resourceName, backupScope)
				resourceShouldProcess[resourceName] = shouldProcessForStore
			}

			// Get values to potentially store
			composite := &structpb.Struct{Fields: fields}
			externalNameValue := getAnnotationValue(composite, "crossplane.io/external-name")
			resourceNameValue := getMetadataName(resourceStruct)

			// Check if we need to store anything
			storedExternalName := getAnnotationValue(composite, StoredExternalNameAnnotation)
			storedResourceName := getAnnotationValue(composite, StoredResourceNameAnnotation)

			// External name backup respects backup scope (only for managed resources with deletion policies)
			shouldStoreExternalName := shouldProcessForStore && externalNameValue != "" && storedExternalName != externalNameValue

			// Resource name (metadata.name) backup is independent of backup scope
			// because XRs and other non-managed resources don't have deletion policies
			shouldStoreResourceName := resourceNameValue != "" && storedResourceName != resourceNameValue

			if shouldStoreExternalName || shouldStoreResourceName {
				// Create resource key for this observed resource
				observedResourceKey := resourceName

				// Build ResourceData with values to store
				data := ResourceData{}
				if shouldStoreExternalName {
					data.ExternalName = externalNameValue
					f.log.Info("Will store external name",
						"resource", resourceName,
						"external-name", externalNameValue)
				}
				if shouldStoreResourceName {
					data.ResourceName = resourceNameValue
					f.log.Info("Will store resource name",
						"resource", resourceName,
						"resource-name", resourceNameValue)
				}

				newResourceData[observedResourceKey] = data

				f.log.Info("Marked resource data for storage",
					"resource", resourceName,
					"external-name", externalNameValue,
					"resource-name", resourceNameValue,
					"composition-key", compositionKey,
					"resource-key", observedResourceKey,
				)
			} else if !shouldProcessForStore && externalNameValue != "" {
				f.log.Info("Skipping external name store - resource not eligible in current backup scope",
					"resource", resourceName,
					"scope", backupScope,
				)
			}
		}
	}

	// Save any NEW resource data back to the store
	// Skip backup entirely when requireRestore is true to prevent overwriting stored data
	if requireRestore {
		f.log.Info("Skipping backup operations - require-restore mode is enabled")
	} else if len(newResourceData) > 0 {
		// Merge new resource data with existing ones
		allResourceData := make(map[string]ResourceData)

		// Start with existing data
		if existingData, exists := resourceDataStore[compositionKey]; exists {
			for k, v := range existingData {
				allResourceData[k] = v
			}
		}

		// Merge new resource data (update existing entries or add new ones)
		for k, newData := range newResourceData {
			existing := allResourceData[k]
			if newData.ExternalName != "" {
				existing.ExternalName = newData.ExternalName
			}
			if newData.ResourceName != "" {
				existing.ResourceName = newData.ResourceName
			}
			allResourceData[k] = existing
		}

		err := store.Save(ctx, clusterID, compositionKey, allResourceData)
		if err != nil {
			response.Fatal(rsp, errors.Wrapf(err, "failed to save resource data to store"))
			return rsp, nil
		}
		f.log.Info("Saved updated resource data to store", "composition-key", compositionKey, "new-count", len(newResourceData), "total-count", len(allResourceData))

		// Add tracking annotations to desired resources for what was successfully stored
		for name, resource := range req.GetDesired().GetResources() {
			resourceStruct := resource.GetResource()
			if resourceStruct != nil && resourceStruct.GetFields() != nil {
				fields := resourceStruct.GetFields()

				// Use pipeline resource name as the stable identifier
				resourceName := name

				// Check if this resource was stored in this operation
				resourceKey := resourceName
				storedData, wasStored := newResourceData[resourceKey]
				if !wasStored {
					continue
				}

				// Check if resource should be processed for external name tracking
				shouldProcess, exists := resourceShouldProcess[resourceName]
				shouldAddExternalNameTracking := exists && shouldProcess && storedData.ExternalName != ""
				// Resource name tracking is always allowed (independent of backup scope)
				shouldAddResourceNameTracking := storedData.ResourceName != ""

				if !shouldAddExternalNameTracking && !shouldAddResourceNameTracking {
					continue
				}

				// Ensure metadata exists
				if fields["metadata"] == nil {
					fields["metadata"] = &structpb.Value{
						Kind: &structpb.Value_StructValue{
							StructValue: &structpb.Struct{
								Fields: make(map[string]*structpb.Value),
							},
						},
					}
				}

				if metadata := fields["metadata"]; metadata != nil {
					if metadataStruct := metadata.GetStructValue(); metadataStruct != nil {
						metadataFields := metadataStruct.GetFields()

						// Ensure annotations exist
						if metadataFields["annotations"] == nil {
							metadataFields["annotations"] = &structpb.Value{
								Kind: &structpb.Value_StructValue{
									StructValue: &structpb.Struct{
										Fields: make(map[string]*structpb.Value),
									},
								},
							}
						}

						if annotationsStruct := metadataFields["annotations"].GetStructValue(); annotationsStruct != nil {
							if annotationsStruct.Fields == nil {
								annotationsStruct.Fields = make(map[string]*structpb.Value)
							}

							// Add tracking annotations for external name if stored (respects backup scope)
							if shouldAddExternalNameTracking {
								annotationsStruct.Fields[StoredExternalNameAnnotation] = &structpb.Value{
									Kind: &structpb.Value_StringValue{
										StringValue: storedData.ExternalName,
									},
								}
								annotationsStruct.Fields[ExternalNameStoredAnnotation] = &structpb.Value{
									Kind: &structpb.Value_StringValue{
										StringValue: timestamp,
									},
								}
							}

							// Add tracking annotations for resource name if stored (independent of backup scope)
							if shouldAddResourceNameTracking {
								annotationsStruct.Fields[StoredResourceNameAnnotation] = &structpb.Value{
									Kind: &structpb.Value_StringValue{
										StringValue: storedData.ResourceName,
									},
								}
								annotationsStruct.Fields[ResourceNameStoredAnnotation] = &structpb.Value{
									Kind: &structpb.Value_StringValue{
										StringValue: timestamp,
									},
								}
							}

							f.log.Info("Added tracking annotations after successful store",
								"resource", resourceName,
								"stored-external-name", storedData.ExternalName,
								"stored-resource-name", storedData.ResourceName,
								"timestamp", timestamp,
							)
						}
					}
				}
			}
		}
	}

	// Final step: merge observed annotations for resources that should be tracked
	for name, resource := range req.GetDesired().GetResources() {
		// Always merge observed annotations to preserve existing function annotations
		f.mergeObservedAnnotations(req, name, resource)
	}

	response.Normalf(rsp, "Processed %d desired and %d observed resources",
		len(req.GetDesired().GetResources()),
		len(req.GetObserved().GetResources()))

	// You can set a custom status condition on the claim. This allows you to
	// communicate with the user. See the link below for status condition
	// guidance.
	// https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md#typical-status-properties
	response.ConditionTrue(rsp, "FunctionSuccess", "Success").
		TargetCompositeAndClaim()

	return rsp, nil
}
