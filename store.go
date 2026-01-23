package main

import (
	"context"
)

// ResourceData holds backup data for a composed resource
type ResourceData struct {
	// ExternalName is the crossplane.io/external-name annotation value
	ExternalName string `json:"externalName,omitempty"`
	// ResourceName is the metadata.name of the composed resource (useful for XR backup)
	ResourceName string `json:"resourceName,omitempty"`
}

// ResourceStore defines the interface for resource data storage
type ResourceStore interface {
	// Save stores resource data for an entire composition
	Save(ctx context.Context, clusterID, compositionKey string, resources map[string]ResourceData) error

	// Load retrieves all resource data for a composition
	Load(ctx context.Context, clusterID, compositionKey string) (map[string]ResourceData, error)

	// Purge removes all resource data for a composition
	Purge(ctx context.Context, clusterID, compositionKey string) error

	// DeleteResource removes a specific resource's data from a composition
	DeleteResource(ctx context.Context, clusterID, compositionKey, resourceKey string) error
}

// ExternalNameStore is an alias for ResourceStore for backward compatibility
type ExternalNameStore = ResourceStore
