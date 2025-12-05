package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/crossplane/function-sdk-go/logging"
)

// DynamoDBStore implements ExternalNameStore using AWS DynamoDB
type DynamoDBStore struct {
	client    *dynamodb.Client
	tableName string
	log       logging.Logger
}

// NewDynamoDBStore creates a new DynamoDB store with provided configuration
func NewDynamoDBStore(ctx context.Context, log logging.Logger, tableName, region string, awsCreds map[string]string) (*DynamoDBStore, error) {
	var cfg aws.Config
	var err error

	if len(awsCreds) > 0 {
		// Use provided credentials
		accessKeyID := awsCreds["accessKeyId"]
		secretAccessKey := awsCreds["secretAccessKey"]
		sessionToken := awsCreds["sessionToken"] // Optional for temporary credentials

		if accessKeyID == "" || secretAccessKey == "" {
			return nil, fmt.Errorf("AWS credentials missing required fields (accessKeyId, secretAccessKey)")
		}

		creds := credentials.NewStaticCredentialsProvider(accessKeyID, secretAccessKey, sessionToken)
		cfg, err = config.LoadDefaultConfig(ctx,
			config.WithRegion(region),
			config.WithCredentialsProvider(creds),
		)
		if err != nil {
			return nil, fmt.Errorf("failed to load AWS config with provided credentials: %w", err)
		}
		log.Info("Using provided AWS credentials for DynamoDB")
	} else {
		// Fall back to default credential chain (environment, IAM role, etc.)
		cfg, err = config.LoadDefaultConfig(ctx, config.WithRegion(region))
		if err != nil {
			return nil, fmt.Errorf("failed to load AWS config with default credentials: %w", err)
		}
		log.Info("Using default AWS credential chain for DynamoDB")
	}

	client := dynamodb.NewFromConfig(cfg)

	store := &DynamoDBStore{
		client:    client,
		tableName: tableName,
		log:       log,
	}

	// Health check: verify table exists and is accessible
	_, err = client.DescribeTable(ctx, &dynamodb.DescribeTableInput{
		TableName: aws.String(tableName),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to access DynamoDB table '%s': %w", tableName, err)
	}

	log.Info("Successfully connected to DynamoDB table", "table", tableName, "region", region)
	return store, nil
}

// Save stores resource data for an entire composition in DynamoDB
func (d *DynamoDBStore) Save(ctx context.Context, clusterID, compositionKey string, resources map[string]ResourceData) error {
	// Create the resources map as DynamoDB attribute
	resourcesAttr := make(map[string]types.AttributeValue)
	for resourceKey, data := range resources {
		// Each resource is stored as a nested map with externalName and resourceName
		resourceMap := make(map[string]types.AttributeValue)
		if data.ExternalName != "" {
			resourceMap["externalName"] = &types.AttributeValueMemberS{Value: data.ExternalName}
		}
		if data.ResourceName != "" {
			resourceMap["resourceName"] = &types.AttributeValueMemberS{Value: data.ResourceName}
		}
		resourcesAttr[resourceKey] = &types.AttributeValueMemberM{Value: resourceMap}
	}

	// Create the item
	item := map[string]types.AttributeValue{
		"cluster_id":      &types.AttributeValueMemberS{Value: clusterID},
		"composition_key": &types.AttributeValueMemberS{Value: compositionKey},
		"resources":       &types.AttributeValueMemberM{Value: resourcesAttr},
	}

	// Put item to DynamoDB
	_, err := d.client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(d.tableName),
		Item:      item,
	})

	if err != nil {
		return fmt.Errorf("failed to save resource data to DynamoDB: %w", err)
	}

	d.log.Info("Saved resource data to DynamoDB",
		"cluster-id", clusterID,
		"composition-key", compositionKey,
		"count", len(resources))

	return nil
}

// Load retrieves all resource data for a composition from DynamoDB
func (d *DynamoDBStore) Load(ctx context.Context, clusterID, compositionKey string) (map[string]ResourceData, error) {
	// Get the specific item for this cluster_id and composition_key
	result, err := d.client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(d.tableName),
		Key: map[string]types.AttributeValue{
			"cluster_id":      &types.AttributeValueMemberS{Value: clusterID},
			"composition_key": &types.AttributeValueMemberS{Value: compositionKey},
		},
	})

	if err != nil {
		return nil, fmt.Errorf("failed to get item from DynamoDB: %w", err)
	}

	// If no item found, return empty map
	if result.Item == nil {
		d.log.Info("No resource data found in DynamoDB",
			"cluster-id", clusterID,
			"composition-key", compositionKey)
		return make(map[string]ResourceData), nil
	}

	resources := make(map[string]ResourceData)

	if resourcesAttr, ok := result.Item["resources"].(*types.AttributeValueMemberM); ok {
		for resourceKey, resourceAttr := range resourcesAttr.Value {
			data := ResourceData{}
			if resourceMap, ok := resourceAttr.(*types.AttributeValueMemberM); ok {
				if externalName, ok := resourceMap.Value["externalName"].(*types.AttributeValueMemberS); ok {
					data.ExternalName = externalName.Value
				}
				if resourceName, ok := resourceMap.Value["resourceName"].(*types.AttributeValueMemberS); ok {
					data.ResourceName = resourceName.Value
				}
			}
			resources[resourceKey] = data
		}
	}

	d.log.Info("Loaded resource data from DynamoDB",
		"cluster-id", clusterID,
		"composition-key", compositionKey,
		"count", len(resources))

	return resources, nil
}

// Purge removes all external names for a composition from DynamoDB
func (d *DynamoDBStore) Purge(ctx context.Context, clusterID, compositionKey string) error {
	// Delete the specific item for this cluster_id and composition_key
	_, err := d.client.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String(d.tableName),
		Key: map[string]types.AttributeValue{
			"cluster_id":      &types.AttributeValueMemberS{Value: clusterID},
			"composition_key": &types.AttributeValueMemberS{Value: compositionKey},
		},
	})

	if err != nil {
		return fmt.Errorf("failed to purge composition from DynamoDB: %w", err)
	}

	d.log.Info("Purged composition from DynamoDB",
		"cluster-id", clusterID,
		"composition-key", compositionKey)

	return nil
}

// DeleteResource removes a specific resource's data from a composition in DynamoDB
func (d *DynamoDBStore) DeleteResource(ctx context.Context, clusterID, compositionKey, resourceKey string) error {
	d.log.Info("Attempting to delete resource from DynamoDB",
		"cluster-id", clusterID,
		"composition-key", compositionKey,
		"resource-key", resourceKey)

	// Use UpdateItem to remove the specific resource key from the resources map
	_, err := d.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(d.tableName),
		Key: map[string]types.AttributeValue{
			"cluster_id":      &types.AttributeValueMemberS{Value: clusterID},
			"composition_key": &types.AttributeValueMemberS{Value: compositionKey},
		},
		UpdateExpression: aws.String("REMOVE #resources.#rk"),
		ExpressionAttributeNames: map[string]string{
			"#resources": "resources",
			"#rk":        resourceKey,
		},
		// Only update if the item exists and the resource key exists in the map
		ConditionExpression: aws.String("attribute_exists(cluster_id) AND attribute_exists(#resources.#rk)"),
	})

	if err != nil {
		// Check if the error is because the item doesn't exist - that's not an error for us
		if isConditionalCheckFailedException(err) {
			d.log.Info("Composition not found for resource deletion (already cleaned up)",
				"cluster-id", clusterID,
				"composition-key", compositionKey,
				"resource-key", resourceKey)
			return nil
		}
		return fmt.Errorf("failed to delete resource from DynamoDB: %w", err)
	}

	d.log.Info("Successfully deleted resource from DynamoDB composition",
		"cluster-id", clusterID,
		"composition-key", compositionKey,
		"resource-key", resourceKey)

	return nil
}

// isConditionalCheckFailedException checks if the error is a conditional check failed exception
func isConditionalCheckFailedException(err error) bool {
	if err == nil {
		return false
	}
	// Check if it's a ConditionalCheckFailedException
	var conditionalCheckFailedException *types.ConditionalCheckFailedException
	return errors.As(err, &conditionalCheckFailedException)
}
