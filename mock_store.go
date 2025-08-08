package main

import (
	"context"
	"sync"

	"github.com/crossplane/function-sdk-go/logging"
)

// Global registry for test stores
var testStoreRegistry *MockExternalNameStore

// MockExternalNameStore implements ExternalNameStore for testing
type MockExternalNameStore struct {
	mu   sync.RWMutex
	data map[string]map[string]map[string]string // clusterID -> compositionKey -> resourceKey -> externalName
}

// NewMockStore creates a new MockExternalNameStore
//
//nolint:unparam // error return maintained for interface consistency
func NewMockStore(_ context.Context, _ logging.Logger) (*MockExternalNameStore, error) {
	// If a test store is registered, return it
	if testStoreRegistry != nil {
		return testStoreRegistry, nil
	}

	// Otherwise create a new one
	return &MockExternalNameStore{
		data: make(map[string]map[string]map[string]string),
	}, nil
}

// SetTestStore sets the global test store (for testing only)
func SetTestStore(store *MockExternalNameStore) {
	testStoreRegistry = store
}

// ClearTestStore clears the global test store
func ClearTestStore() {
	testStoreRegistry = nil
}

// Save stores external names in the mock store
func (m *MockExternalNameStore) Save(_ context.Context, clusterID, compositionKey string, externalNames map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.data[clusterID] == nil {
		m.data[clusterID] = make(map[string]map[string]string)
	}
	m.data[clusterID][compositionKey] = make(map[string]string)
	for k, v := range externalNames {
		m.data[clusterID][compositionKey][k] = v
	}
	return nil
}

// Load retrieves external names from the mock store
func (m *MockExternalNameStore) Load(_ context.Context, clusterID, compositionKey string) (map[string]string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if clusterData, exists := m.data[clusterID]; exists {
		if compositionData, exists := clusterData[compositionKey]; exists {
			result := make(map[string]string)
			for k, v := range compositionData {
				result[k] = v
			}
			return result, nil
		}
	}
	return make(map[string]string), nil
}

// DeleteResource removes a specific resource from the mock store
func (m *MockExternalNameStore) DeleteResource(_ context.Context, clusterID, compositionKey, resourceKey string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if clusterData, exists := m.data[clusterID]; exists {
		if compositionData, exists := clusterData[compositionKey]; exists {
			delete(compositionData, resourceKey)
		}
	}
	return nil
}

// Purge removes all data for a composition from the mock store
func (m *MockExternalNameStore) Purge(_ context.Context, clusterID, compositionKey string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if clusterData, exists := m.data[clusterID]; exists {
		delete(clusterData, compositionKey)
	}
	return nil
}
