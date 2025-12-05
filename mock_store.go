package main

import (
	"context"
	"sync"

	"github.com/crossplane/function-sdk-go/logging"
)

// Global registry for test stores
var testStoreRegistry *MockResourceStore

// MockResourceStore implements ResourceStore for testing
type MockResourceStore struct {
	mu   sync.RWMutex
	data map[string]map[string]map[string]ResourceData // clusterID -> compositionKey -> resourceKey -> ResourceData
}

// NewMockStore creates a new MockResourceStore
//
//nolint:unparam // error return maintained for interface consistency
func NewMockStore(_ context.Context, _ logging.Logger) (*MockResourceStore, error) {
	// If a test store is registered, return it
	if testStoreRegistry != nil {
		return testStoreRegistry, nil
	}

	// Otherwise create a new one
	return &MockResourceStore{
		data: make(map[string]map[string]map[string]ResourceData),
	}, nil
}

// SetTestStore sets the global test store (for testing only)
func SetTestStore(store *MockResourceStore) {
	testStoreRegistry = store
}

// ClearTestStore clears the global test store
func ClearTestStore() {
	testStoreRegistry = nil
}

// Save stores resource data in the mock store
func (m *MockResourceStore) Save(_ context.Context, clusterID, compositionKey string, resources map[string]ResourceData) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.data[clusterID] == nil {
		m.data[clusterID] = make(map[string]map[string]ResourceData)
	}
	m.data[clusterID][compositionKey] = make(map[string]ResourceData)
	for k, v := range resources {
		m.data[clusterID][compositionKey][k] = v
	}
	return nil
}

// Load retrieves resource data from the mock store
func (m *MockResourceStore) Load(_ context.Context, clusterID, compositionKey string) (map[string]ResourceData, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if clusterData, exists := m.data[clusterID]; exists {
		if compositionData, exists := clusterData[compositionKey]; exists {
			result := make(map[string]ResourceData)
			for k, v := range compositionData {
				result[k] = v
			}
			return result, nil
		}
	}
	return make(map[string]ResourceData), nil
}

// DeleteResource removes a specific resource from the mock store
func (m *MockResourceStore) DeleteResource(_ context.Context, clusterID, compositionKey, resourceKey string) error {
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
func (m *MockResourceStore) Purge(_ context.Context, clusterID, compositionKey string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if clusterData, exists := m.data[clusterID]; exists {
		delete(clusterData, compositionKey)
	}
	return nil
}
