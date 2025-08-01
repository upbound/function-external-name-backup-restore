package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/crossplane/function-sdk-go/errors"
	"github.com/crossplane/function-sdk-go/logging"
	fnv1 "github.com/crossplane/function-sdk-go/proto/v1"
	"github.com/crossplane/function-sdk-go/request"
	"github.com/crossplane/function-sdk-go/response"
	"github.com/crossplane/function-external-name-backup-restore/input/v1beta1"
	"google.golang.org/protobuf/types/known/structpb"
)

const (
	// StoredExternalNameAnnotation tracks the external name value stored in the external store
	StoredExternalNameAnnotation = "fn.crossplane.io/stored-external-name"

	// ExternalNameStoredAnnotation tracks when the external name was stored with timestamp
	ExternalNameStoredAnnotation = "fn.crossplane.io/external-name-stored"

	// ExternalNameDeletedAnnotation tracks when the external name was deleted with timestamp
	ExternalNameDeletedAnnotation = "fn.crossplane.io/external-name-deleted"

	// ExternalNameRestoredAnnotation tracks when the external name was restored with timestamp
	ExternalNameRestoredAnnotation = "fn.crossplane.io/external-name-restored"

	// EnableExternalStoreAnnotation on XR enables external store loading and storing
	EnableExternalStoreAnnotation = "fn.crossplane.io/enable-external-store"

	// PurgeExternalStoreAnnotation on XR purges all stored external names for this composition
	PurgeExternalStoreAnnotation = "fn.crossplane.io/purge-external-store"

	// Configuration annotations on XR
	ClusterIDAnnotation      = "fn.crossplane.io/cluster-id"
	StoreTypeAnnotation      = "fn.crossplane.io/store-type"
	DynamoDBTableAnnotation  = "fn.crossplane.io/dynamodb-table"
	DynamoDBRegionAnnotation = "fn.crossplane.io/dynamodb-region"
	OperationModeAnnotation  = "fn.crossplane.io/operation-mode"

	// Operation modes
	OperationModeOnlyOrphaned  = "only-orphaned"
	OperationModeAllResources  = "all-resources"
)

// Function returns whatever response you ask it to.
type Function struct {
	fnv1.UnimplementedFunctionRunnerServiceServer

	log logging.Logger
}


// NewFunction creates a new Function
func NewFunction(ctx context.Context, log logging.Logger) (*Function, error) {
	return &Function{
		log: log,
	}, nil
}

// FunctionConfig holds all configuration for the function
type FunctionConfig struct {
	ClusterID       string
	StoreType       string
	DynamoDBTable   string
	DynamoDBRegion  string
	OperationMode   string
}

// getConfigFromAnnotations extracts configuration from XR annotations with defaults
func getConfigFromAnnotations(req *fnv1.RunFunctionRequest, log logging.Logger) *FunctionConfig {
	config := &FunctionConfig{
		ClusterID:       "default",
		StoreType:       "awsdynamodb",
		DynamoDBTable:   "external-name-backup",
		DynamoDBRegion:  "us-west-2",
		OperationMode:   OperationModeOnlyOrphaned,
	}

	// Helper function to get annotation value from composite resource
	getAnnotationValue := func(composite *structpb.Struct, annotation string) string {
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

	// Check desired composite first, then observed composite as fallback
	var composite *structpb.Struct
	if desiredComposite := req.GetDesired().GetComposite().GetResource(); desiredComposite != nil {
		composite = desiredComposite
	} else if observedComposite := req.GetObserved().GetComposite().GetResource(); observedComposite != nil {
		composite = observedComposite
	}

	if composite != nil {
		if clusterID := getAnnotationValue(composite, ClusterIDAnnotation); clusterID != "" {
			config.ClusterID = clusterID
		}
		if storeType := getAnnotationValue(composite, StoreTypeAnnotation); storeType != "" {
			config.StoreType = storeType
		}
		if dynamoDBTable := getAnnotationValue(composite, DynamoDBTableAnnotation); dynamoDBTable != "" {
			config.DynamoDBTable = dynamoDBTable
		}
		if dynamoDBRegion := getAnnotationValue(composite, DynamoDBRegionAnnotation); dynamoDBRegion != "" {
			config.DynamoDBRegion = dynamoDBRegion
		}
		if operationMode := getAnnotationValue(composite, OperationModeAnnotation); operationMode != "" {
			config.OperationMode = operationMode
		}
	}

	log.Info("Configuration loaded from XR annotations",
		"cluster-id", config.ClusterID,
		"store-type", config.StoreType,
		"dynamodb-table", config.DynamoDBTable,
		"dynamodb-region", config.DynamoDBRegion,
		"operation-mode", config.OperationMode)

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

// checkEnableAnnotation checks for enable annotation in a composite resource
func checkEnableAnnotation(composite *structpb.Struct, log logging.Logger, source string) bool {
	if fields := composite.GetFields(); fields != nil {
		if metadata := fields["metadata"]; metadata != nil {
			if metadataStruct := metadata.GetStructValue(); metadataStruct != nil {
				if annotations := metadataStruct.GetFields()["annotations"]; annotations != nil {
					if annotationsStruct := annotations.GetStructValue(); annotationsStruct != nil {
						if enableAnnotation := annotationsStruct.GetFields()[EnableExternalStoreAnnotation]; enableAnnotation != nil {
							enableValue := enableAnnotation.GetStringValue()
							if enableValue == "true" || enableValue == "yes" || enableValue == "1" {
								log.Info("External store operations enabled by XR annotation",
									"source", source,
									"annotation", EnableExternalStoreAnnotation,
									"value", enableValue)
								return true
							}
						}
					}
				}
			}
		}
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
	if fields := composite.GetFields(); fields != nil {
		if metadata := fields["metadata"]; metadata != nil {
			if metadataStruct := metadata.GetStructValue(); metadataStruct != nil {
				if annotations := metadataStruct.GetFields()["annotations"]; annotations != nil {
					if annotationsStruct := annotations.GetStructValue(); annotationsStruct != nil {
						if purgeAnnotation := annotationsStruct.GetFields()[PurgeExternalStoreAnnotation]; purgeAnnotation != nil {
							purgeValue := purgeAnnotation.GetStringValue()
							if purgeValue == "true" || purgeValue == "yes" || purgeValue == "1" {
								log.Info("External store purge requested by XR annotation",
									"source", source,
									"annotation", PurgeExternalStoreAnnotation,
									"value", purgeValue)
								return true
							}
						}
					}
				}
			}
		}
	}
	return false
}

// getCompositionResourceName extracts the composition resource name from annotations
func getCompositionResourceName(fields map[string]*structpb.Value) string {
	if metadata := fields["metadata"]; metadata != nil {
		if metadataStruct := metadata.GetStructValue(); metadataStruct != nil {
			if annotations := metadataStruct.GetFields()["annotations"]; annotations != nil {
				if annotationsStruct := annotations.GetStructValue(); annotationsStruct != nil {
					if compositionResourceName := annotationsStruct.GetFields()["crossplane.io/composition-resource-name"]; compositionResourceName != nil {
						return compositionResourceName.GetStringValue()
					}
				}
			}
		}
	}
	return ""
}

// mergeObservedAnnotations ensures desired resource has annotation structure and merges observed annotations
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
								if annotationsStruct.Fields != nil {
									// Remove tracking annotations
									if _, exists := annotationsStruct.Fields[StoredExternalNameAnnotation]; exists {
										delete(annotationsStruct.Fields, StoredExternalNameAnnotation)
									}
									if _, exists := annotationsStruct.Fields[ExternalNameStoredAnnotation]; exists {
										delete(annotationsStruct.Fields, ExternalNameStoredAnnotation)
									}
								}
							}
						}
					}
				}
			}
		}
	}
}

// hasStoredExternalNameAnnotation checks if a resource already has the stored-external-name annotation
// either in observed or desired resources, returning true if the annotation exists with the same value
func (f *Function) hasStoredExternalNameAnnotation(req *fnv1.RunFunctionRequest, resourceName, externalNameValue string, observedAnnotations *structpb.Struct) bool {
	// First check observed resource
	if storedAnnotation := observedAnnotations.GetFields()[StoredExternalNameAnnotation]; storedAnnotation != nil {
		storedExternalName := storedAnnotation.GetStringValue()
		if storedExternalName == externalNameValue {
			f.log.Info("Found stored-external-name annotation in observed resource with same value",
				"resource", resourceName,
				"external-name", externalNameValue)
			return true
		}
	}

	// Check desired resource for the annotation (in case we just restored it)
	if desiredResource, exists := req.GetDesired().GetResources()[resourceName]; exists {
		if desiredResourceStruct := desiredResource.GetResource(); desiredResourceStruct != nil && desiredResourceStruct.GetFields() != nil {
			desiredFields := desiredResourceStruct.GetFields()
			if desiredMetadata := desiredFields["metadata"]; desiredMetadata != nil {
				if desiredMetadataStruct := desiredMetadata.GetStructValue(); desiredMetadataStruct != nil {
					if desiredAnnotations := desiredMetadataStruct.GetFields()["annotations"]; desiredAnnotations != nil {
						if desiredAnnotationsStruct := desiredAnnotations.GetStructValue(); desiredAnnotationsStruct != nil {
							if desiredStoredAnnotation := desiredAnnotationsStruct.GetFields()[StoredExternalNameAnnotation]; desiredStoredAnnotation != nil {
								storedExternalName := desiredStoredAnnotation.GetStringValue()
								if storedExternalName == externalNameValue {
									f.log.Info("Found stored-external-name annotation in desired resource with same value",
										"resource", resourceName,
										"external-name", externalNameValue)
									return true
								}
							}
						}
					}
				}
			}
		}
	}

	return false
}

// shouldDeleteFromExternalStoreWithFallback checks deletion criteria in desired resource, falling back to observed
func (f *Function) shouldDeleteFromExternalStoreWithFallback(desiredFields, observedFields map[string]*structpb.Value, resourceName string) bool {
	// Helper function to check spec fields for deletion criteria
	checkDeletionCriteria := func(fields map[string]*structpb.Value) (hasDeletePolicy bool, hasDeleteManagementPolicy bool, hasSpec bool) {
		if spec := fields["spec"]; spec != nil {
			if specStruct := spec.GetStructValue(); specStruct != nil {
				hasSpec = true
				specFields := specStruct.GetFields()

				// Check deletionPolicy is "Delete"
				if deletionPolicy := specFields["deletionPolicy"]; deletionPolicy != nil {
					if deletionPolicy.GetStringValue() == "Delete" {
						hasDeletePolicy = true
					}
				}

				// Check managementPolicies contains "*" or "Delete"
				if managementPolicies := specFields["managementPolicies"]; managementPolicies != nil {
					if listValue := managementPolicies.GetListValue(); listValue != nil {
						for _, item := range listValue.GetValues() {
							policy := item.GetStringValue()
							if policy == "*" || policy == "Delete" {
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

// shouldDeleteFromExternalStore checks if a resource with stored external name should be deleted
// from the external store due to deletionPolicy being "Delete" and managementPolicies containing "*" or "Delete"
func (f *Function) shouldDeleteFromExternalStore(fields map[string]*structpb.Value, resourceName string) bool {
	// First check if resource has the stored-external-name annotation
	hasStoredAnnotation := false
	if metadata := fields["metadata"]; metadata != nil {
		if metadataStruct := metadata.GetStructValue(); metadataStruct != nil {
			if annotations := metadataStruct.GetFields()["annotations"]; annotations != nil {
				if annotationsStruct := annotations.GetStructValue(); annotationsStruct != nil {
					if storedAnnotation := annotationsStruct.GetFields()[StoredExternalNameAnnotation]; storedAnnotation != nil {
						hasStoredAnnotation = true
					}
				}
			}
		}
	}

	if !hasStoredAnnotation {
		f.log.Info("Resource does not have stored-external-name annotation, skipping deletion check",
			"resource", resourceName)
		return false
	}

	if spec := fields["spec"]; spec != nil {
		if specStruct := spec.GetStructValue(); specStruct != nil {
			specFields := specStruct.GetFields()

			// Check deletionPolicy is "Delete"
			hasDeletePolicy := false
			if deletionPolicy := specFields["deletionPolicy"]; deletionPolicy != nil {
				if deletionPolicy.GetStringValue() == "Delete" {
					hasDeletePolicy = true
				}
			}

			// Check managementPolicies contains "*" or "Delete"
			hasDeleteManagementPolicy := false
			if managementPolicies := specFields["managementPolicies"]; managementPolicies != nil {
				if listValue := managementPolicies.GetListValue(); listValue != nil {
					for _, item := range listValue.GetValues() {
						policy := item.GetStringValue()
						if policy == "*" || policy == "Delete" {
							hasDeleteManagementPolicy = true
							break
						}
					}
				}
			}

			shouldDelete := hasDeletePolicy && hasDeleteManagementPolicy
			f.log.Info("Checked deletion criteria",
				"resource", resourceName,
				"has-stored-annotation", hasStoredAnnotation,
				"deletion-policy-delete", hasDeletePolicy,
				"management-policies-delete", hasDeleteManagementPolicy,
				"should-delete", shouldDelete)

			return shouldDelete
		}
	}

	return false
}

// shouldProcessResource determines if a resource should be processed based on operation mode
func (f *Function) shouldProcessResource(fields map[string]*structpb.Value, resourceName string, operationMode string) bool {
	if operationMode == OperationModeAllResources {
		// Process all resources regardless of deletion policy
		return true
	}

	if operationMode == OperationModeOnlyOrphaned {
		// Check spec.deletionPolicy and spec.managementPolicies
		if spec := fields["spec"]; spec != nil {
			if specStruct := spec.GetStructValue(); specStruct != nil {
				specFields := specStruct.GetFields()

				// Check deletionPolicy is "Orphan"
				hasOrphanPolicy := false
				if deletionPolicy := specFields["deletionPolicy"]; deletionPolicy != nil {
					if deletionPolicy.GetStringValue() == "Orphan" {
						hasOrphanPolicy = true
					}
				}

				// Check managementPolicies does not contain "*" or "Delete"
				managementPoliciesOk := true
				if managementPolicies := specFields["managementPolicies"]; managementPolicies != nil {
					if listValue := managementPolicies.GetListValue(); listValue != nil {
						for _, item := range listValue.GetValues() {
							policy := item.GetStringValue()
							if policy == "*" || policy == "Delete" {
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

		f.log.Info("Resource missing spec, skipping in only-orphaned mode", "resource", resourceName)
		return false
	}

	f.log.Info("Unknown operation mode, defaulting to process", "mode", operationMode, "resource", resourceName)
	return true
}

// RunFunction runs the Function.
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

	default:
		response.Fatal(rsp, errors.Errorf("unsupported external store type: %s (supported types: 'awsdynamodb', 'mock')", config.StoreType))
		return rsp, nil
	}

	clusterID := config.ClusterID
	operationMode := config.OperationMode
	f.log.Info("Using configuration for function execution", 
		"cluster-id", clusterID,
		"operation-mode", operationMode,
		"store-type", config.StoreType)

	// Extract claim and XR information from composite resource
	var xrApiVersion, xrKind, xrName, claimNamespace, claimName string


	// Use observed composite for metadata extraction (it has complete info)
	if observedComposite := req.GetObserved().GetComposite().GetResource(); observedComposite != nil {
		if fields := observedComposite.GetFields(); fields != nil {
			if apiVersion := fields["apiVersion"]; apiVersion != nil {
				xrApiVersion = apiVersion.GetStringValue()
			}
			if kind := fields["kind"]; kind != nil {
				xrKind = kind.GetStringValue()
			}
			if metadata := fields["metadata"]; metadata != nil {
				if metadataStruct := metadata.GetStructValue(); metadataStruct != nil {
					if name := metadataStruct.GetFields()["name"]; name != nil {
						xrName = name.GetStringValue()
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
	if claimNamespace == "" {
		claimNamespace = "none"
	}
	if claimName == "" {
		claimName = "none"
	}

	f.log.Info("Extracted composition information",
		"xr-api-version", xrApiVersion,
		"xr-kind", xrKind,
		"xr-name", xrName,
		"claim-namespace", claimNamespace,
		"claim-name", claimName)

	// Parse function input (for future extensibility)
	in := &v1beta1.Input{}
	if err := request.GetInput(req, in); err != nil {
		response.Fatal(rsp, errors.Wrapf(err, "cannot get Function input from %T", req))
		return rsp, nil
	}

	// Create composition key: {claimNamespace}/{claimName}/{apiVersionOfXr}/{kindOfXr}/{metadata.name of XR}
	compositionKey := fmt.Sprintf("%s/%s/%s/%s/%s", claimNamespace, claimName, xrApiVersion, xrKind, xrName)

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

	// Load existing external names from pre-initialized store
	loadedResources, err := store.Load(ctx, clusterID, compositionKey)
	if err != nil {
		response.Fatal(rsp, errors.Wrapf(err, "failed to load external names from store"))
		return rsp, nil
	}

	// Convert to nested structure for processing
	externalNameStore := map[string]map[string]string{
		compositionKey: loadedResources,
	}
	f.log.Info("Loaded external names from store", "loaded-count", len(loadedResources))

	// Track only NEW external names that should be stored (not restored ones)
	newExternalNames := make(map[string]string)

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

			var apiVersion, kind string
			if av := fields["apiVersion"]; av != nil {
				apiVersion = av.GetStringValue()
			}
			if k := fields["kind"]; k != nil {
				kind = k.GetStringValue()
			}

			// Check if this desired resource should be deleted from external store
			// This check applies to ALL resources, not just those that meet orphan criteria
			// First check desired resource for stored-external-name annotation, then fallback to observed
			shouldDelete := false

			// Check desired resource first for stored-external-name annotation
			hasStoredAnnotation := false
			if metadata := fields["metadata"]; metadata != nil {
				if metadataStruct := metadata.GetStructValue(); metadataStruct != nil {
					if annotations := metadataStruct.GetFields()["annotations"]; annotations != nil {
						if annotationsStruct := annotations.GetStructValue(); annotationsStruct != nil {
							if annotationsStruct.GetFields()[StoredExternalNameAnnotation] != nil {
								hasStoredAnnotation = true
							}
						}
					}
				}
			}

			// Fallback to observed resource if not found in desired
			if !hasStoredAnnotation {
				if observedResource, exists := req.GetObserved().GetResources()[resourceName]; exists {
					if observedStruct := observedResource.GetResource(); observedStruct != nil && observedStruct.GetFields() != nil {
						observedFields := observedStruct.GetFields()
						if metadata := observedFields["metadata"]; metadata != nil {
							if metadataStruct := metadata.GetStructValue(); metadataStruct != nil {
								if annotations := metadataStruct.GetFields()["annotations"]; annotations != nil {
									if annotationsStruct := annotations.GetStructValue(); annotationsStruct != nil {
										if annotationsStruct.GetFields()[StoredExternalNameAnnotation] != nil {
											hasStoredAnnotation = true
										}
									}
								}
							}
						}
					}
				}
			}

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
				resourceKey := fmt.Sprintf("%s/%s/%s", apiVersion, kind, resourceName)

				f.log.Info("Processing deletion for desired resource",
					"resource", resourceName,
					"resource-key", resourceKey)

				// Delete from external store
				err := store.DeleteResource(ctx, clusterID, compositionKey, resourceKey)
				if err != nil {
					f.log.Info("Failed to delete resource from external store",
						"resource", resourceName,
						"error", err.Error())
				} else {
					f.log.Info("Deleted resource from external store",
						"resource", resourceName,
						"resource-key", resourceKey)

					// Remove from local cache so it doesn't get re-added during save
					if compositionData, exists := externalNameStore[compositionKey]; exists {
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
							if annotationsStruct.Fields == nil {
								annotationsStruct.Fields = make(map[string]*structpb.Value)
							}

							// Remove tracking annotations
							delete(annotationsStruct.Fields, StoredExternalNameAnnotation)
							delete(annotationsStruct.Fields, ExternalNameStoredAnnotation)

							// Add deletion timestamp
							annotationsStruct.Fields[ExternalNameDeletedAnnotation] = &structpb.Value{
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
	if compositionData, exists := externalNameStore[compositionKey]; exists && len(compositionData) == 0 {
		f.log.Info("Composition has no external names left, purging entire composition from store",
			"composition-key", compositionKey)
		
		err := store.Purge(ctx, clusterID, compositionKey)
		if err != nil {
			f.log.Info("Failed to purge empty composition from external store",
				"composition-key", compositionKey,
				"error", err.Error())
		} else {
			f.log.Info("Successfully purged empty composition from external store",
				"composition-key", compositionKey)
			
			// Remove from local cache
			delete(externalNameStore, compositionKey)
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
			shouldProcess := f.shouldProcessResource(fields, resourceName, operationMode)
			resourceShouldProcess[resourceName] = shouldProcess // Cache the result
			if !shouldProcess {
				f.log.Info("Skipping external store operations for desired resource due to operation mode", "resource", resourceName, "mode", operationMode)
				continue // For desired resources, we can skip entirely since we're only restoring
			}

			// Check if the desired resource already has an external-name annotation
			hasExistingExternalName := false
			if metadata := fields["metadata"]; metadata != nil {
				if metadataStruct := metadata.GetStructValue(); metadataStruct != nil {
					if annotations := metadataStruct.GetFields()["annotations"]; annotations != nil {
						if annotationsStruct := annotations.GetStructValue(); annotationsStruct != nil {
							if existingExternalName := annotationsStruct.GetFields()["crossplane.io/external-name"]; existingExternalName != nil {
								hasExistingExternalName = true
								f.log.Info("Desired resource already has external-name, skipping restoration",
									"resource", name,
									"existing-external-name", existingExternalName.GetStringValue(),
								)
							}
						}
					}
				}
			}

			// Check if the corresponding observed resource has an external-name annotation
			if !hasExistingExternalName {
				if observedResource, exists := req.GetObserved().GetResources()[resourceName]; exists {
					if observedResourceStruct := observedResource.GetResource(); observedResourceStruct != nil && observedResourceStruct.GetFields() != nil {
						observedFields := observedResourceStruct.GetFields()
						if metadata := observedFields["metadata"]; metadata != nil {
							if metadataStruct := metadata.GetStructValue(); metadataStruct != nil {
								if annotations := metadataStruct.GetFields()["annotations"]; annotations != nil {
									if annotationsStruct := annotations.GetStructValue(); annotationsStruct != nil {
										if observedExternalName := annotationsStruct.GetFields()["crossplane.io/external-name"]; observedExternalName != nil {
											hasExistingExternalName = true
											f.log.Info("Observed resource has external-name, skipping restoration",
												"resource", resourceName,
												"observed-external-name", observedExternalName.GetStringValue(),
											)
										}
									}
								}
							}
						}
					}
				}
			}

			// Only restore if no existing external-name and we have one in our store
			if !hasExistingExternalName {
				// Create key for external name store lookup using pipeline resource name
				resourceKey := fmt.Sprintf("%s/%s/%s", apiVersion, kind, resourceName)

				// Check if we have an external name for this resource in our store
				if compositionData, compositionExists := externalNameStore[compositionKey]; compositionExists {
					if externalName, resourceExists := compositionData[resourceKey]; resourceExists {
						f.log.Info("Restoring external-name from store with timestamp",
							"resource", resourceName,
							"external-name", externalName,
							"timestamp", timestamp,
						)

						// Add/update the external-name annotation in the desired resource
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
									if annotationsStruct.Fields == nil {
										annotationsStruct.Fields = make(map[string]*structpb.Value)
									}

									// Set the external-name annotation
									annotationsStruct.Fields["crossplane.io/external-name"] = &structpb.Value{
										Kind: &structpb.Value_StringValue{
											StringValue: externalName,
										},
									}

									// Add tracking annotation
									annotationsStruct.Fields[StoredExternalNameAnnotation] = &structpb.Value{
										Kind: &structpb.Value_StringValue{
											StringValue: externalName,
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
					} else {
						f.log.Info("No external-name found in store for resource", "resource", resourceName, "composition-key", compositionKey, "resource-key", resourceKey)
					}
				} else {
					f.log.Info("No composition found in store", "composition-key", compositionKey)
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
				shouldProcessForStore = f.shouldProcessResource(fields, resourceName, operationMode)
				resourceShouldProcess[resourceName] = shouldProcessForStore
			}

			// Check for external-name annotation in metadata
			if metadata := fields["metadata"]; metadata != nil {
				if metadataStruct := metadata.GetStructValue(); metadataStruct != nil {
					if annotations := metadataStruct.GetFields()["annotations"]; annotations != nil {
						if annotationsStruct := annotations.GetStructValue(); annotationsStruct != nil {
							if externalName := annotationsStruct.GetFields()["crossplane.io/external-name"]; externalName != nil {
								externalNameValue := externalName.GetStringValue()

								// Only store if resource should be processed for the external store
								if shouldProcessForStore {
									// Check if resource already has stored-external-name annotation to avoid unnecessary writes
									hasAnnotation := f.hasStoredExternalNameAnnotation(req, resourceName, externalNameValue, annotationsStruct)
									shouldStore := !hasAnnotation
									if !shouldStore {
										f.log.Info("Skipping store operation - resource already has stored-external-name annotation with same value",
											"resource", resourceName,
											"external-name", externalNameValue)
									} else {
										f.log.Info("Will store external name - no existing annotation found",
											"resource", resourceName,
											"external-name", externalNameValue)
									}

								if shouldStore {
									// Create resource key for this observed resource
									observedResourceKey := fmt.Sprintf("%s/%s/%s", apiVersion, kind, resourceName)

									// Store the external name for saving to external store later
									newExternalNames[observedResourceKey] = externalNameValue

									f.log.Info("Marked external-name for storage",
										"resource", resourceName,
										"external-name", externalNameValue,
										"composition-key", compositionKey,
										"resource-key", observedResourceKey,
									)
								}
								} else {
									f.log.Info("Skipping store operation - resource not eligible in current operation mode",
										"resource", resourceName,
										"external-name", externalNameValue,
										"mode", operationMode,
									)
								}
							}
						}
					}
				}
			}
		}
	}

	// Save any NEW external names back to the store
	if len(newExternalNames) > 0 {
		// Merge new external names with existing ones
		allExternalNames := make(map[string]string)

		// Start with existing data
		if existingData, exists := externalNameStore[compositionKey]; exists {
			for k, v := range existingData {
				allExternalNames[k] = v
			}
		}

		// Add new external names
		for k, v := range newExternalNames {
			allExternalNames[k] = v
		}

		err := store.Save(ctx, clusterID, compositionKey, allExternalNames)
		if err != nil {
			response.Fatal(rsp, errors.Wrapf(err, "failed to save external names to store"))
			return rsp, nil
		}
		f.log.Info("Saved updated external names to store", "composition-key", compositionKey, "new-count", len(newExternalNames), "total-count", len(allExternalNames))

		// Add tracking annotations to desired resources for what was successfully stored
		for name, resource := range req.GetDesired().GetResources() {
			resourceStruct := resource.GetResource()
			if resourceStruct != nil && resourceStruct.GetFields() != nil {
				fields := resourceStruct.GetFields()

				// Use pipeline resource name as the stable identifier
				resourceName := name

				// Only add tracking annotations for resources that should be processed
				if shouldProcess, exists := resourceShouldProcess[resourceName]; !exists || !shouldProcess {
					continue
				}

				var apiVersion, kind string
				if av := fields["apiVersion"]; av != nil {
					apiVersion = av.GetStringValue()
				}
				if k := fields["kind"]; k != nil {
					kind = k.GetStringValue()
				}

				// Check if this resource was stored in this operation (only for NEW external names)
				resourceKey := fmt.Sprintf("%s/%s/%s", apiVersion, kind, resourceName)
				if storedValue, wasStored := newExternalNames[resourceKey]; wasStored {
					// Add tracking annotation to the desired resource

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
								if annotationsStruct.Fields == nil {
									annotationsStruct.Fields = make(map[string]*structpb.Value)
								}

								// Add tracking annotation
								annotationsStruct.Fields[StoredExternalNameAnnotation] = &structpb.Value{
									Kind: &structpb.Value_StringValue{
										StringValue: storedValue,
									},
								}

								// Add timestamp annotation
								annotationsStruct.Fields[ExternalNameStoredAnnotation] = &structpb.Value{
									Kind: &structpb.Value_StringValue{
										StringValue: timestamp,
									},
								}

								f.log.Info("Added tracking and timestamp annotations after successful store",
									"resource", resourceName,
									"stored-external-name", storedValue,
									"timestamp", timestamp,
								)
							}
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
