package main

import (
	"context"
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

	if awsCreds != nil && len(awsCreds) > 0 {
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

// Save stores external names for an entire composition in DynamoDB
func (d *DynamoDBStore) Save(ctx context.Context, clusterID, compositionKey string, externalNames map[string]string) error {
	// Create the external names map as DynamoDB attribute
	externalNamesAttr := make(map[string]types.AttributeValue)
	for resourceKey, externalName := range externalNames {
		externalNamesAttr[resourceKey] = &types.AttributeValueMemberS{Value: externalName}
	}

	// Create the item
	item := map[string]types.AttributeValue{
		"cluster_id":      &types.AttributeValueMemberS{Value: clusterID},
		"composition_key": &types.AttributeValueMemberS{Value: compositionKey},
		"external_names":  &types.AttributeValueMemberM{Value: externalNamesAttr},
	}

	// Put item to DynamoDB
	_, err := d.client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(d.tableName),
		Item:      item,
	})

	if err != nil {
		return fmt.Errorf("failed to save external names to DynamoDB: %w", err)
	}

	d.log.Info("Saved external names to DynamoDB",
		"cluster-id", clusterID,
		"composition-key", compositionKey,
		"count", len(externalNames))

	return nil
}

// Load retrieves all external names for a composition from DynamoDB
func (d *DynamoDBStore) Load(ctx context.Context, clusterID, compositionKey string) (map[string]string, error) {
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
		d.log.Info("No external names found in DynamoDB",
			"cluster-id", clusterID,
			"composition-key", compositionKey)
		return make(map[string]string), nil
	}

	// Extract external names from the item
	externalNames := make(map[string]string)
	if externalNamesAttr, ok := result.Item["external_names"].(*types.AttributeValueMemberM); ok {
		for resourceKey, externalNameAttr := range externalNamesAttr.Value {
			if externalNameStr, ok := externalNameAttr.(*types.AttributeValueMemberS); ok {
				externalNames[resourceKey] = externalNameStr.Value
			}
		}
	}

	d.log.Info("Loaded external names from DynamoDB",
		"cluster-id", clusterID,
		"composition-key", compositionKey,
		"count", len(externalNames))

	return externalNames, nil
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

// DeleteResource removes a specific resource's external name from a composition in DynamoDB
func (d *DynamoDBStore) DeleteResource(ctx context.Context, clusterID, compositionKey, resourceKey string) error {
	d.log.Info("Attempting to delete resource from DynamoDB",
		"cluster-id", clusterID,
		"composition-key", compositionKey,
		"resource-key", resourceKey)

	// Use UpdateItem to remove the specific resource key from the external_names map
	_, err := d.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(d.tableName),
		Key: map[string]types.AttributeValue{
			"cluster_id":      &types.AttributeValueMemberS{Value: clusterID},
			"composition_key": &types.AttributeValueMemberS{Value: compositionKey},
		},
		UpdateExpression: aws.String("REMOVE #external_names.#rk"),
		ExpressionAttributeNames: map[string]string{
			"#external_names": "external_names",
			"#rk":             resourceKey,
		},
		// Only update if the item exists and the resource key exists in the map
		ConditionExpression: aws.String("attribute_exists(cluster_id) AND attribute_exists(#external_names.#rk)"),
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
	_, ok := err.(*types.ConditionalCheckFailedException)
	return ok
}
