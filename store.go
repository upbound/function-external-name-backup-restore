package main

import (
	"context"
)

// ExternalNameStore defines the interface for external name storage
type ExternalNameStore interface {
	// Save stores external names for an entire composition
	Save(ctx context.Context, clusterID, compositionKey string, externalNames map[string]string) error

	// Load retrieves all external names for a composition
	Load(ctx context.Context, clusterID, compositionKey string) (map[string]string, error)

	// Purge removes all external names for a composition
	Purge(ctx context.Context, clusterID, compositionKey string) error

	// DeleteResource removes a specific resource's external name from a composition
	DeleteResource(ctx context.Context, clusterID, compositionKey, resourceKey string) error
}
