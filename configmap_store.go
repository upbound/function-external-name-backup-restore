package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/crossplane/function-sdk-go/logging"
)

// ConfigMapStore implements ResourceStore using Kubernetes ConfigMaps
type ConfigMapStore struct {
	client    kubernetes.Interface
	namespace string
	log       logging.Logger
}

// NewConfigMapStore creates a new ConfigMap store
func NewConfigMapStore(ctx context.Context, log logging.Logger, namespace string) (*ConfigMapStore, error) {
	if namespace == "" {
		namespace = "crossplane-system"
	}

	// Create in-cluster config
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to create in-cluster config: %w", err)
	}

	// Create the clientset
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create kubernetes client: %w", err)
	}

	store := &ConfigMapStore{
		client:    clientset,
		namespace: namespace,
		log:       log,
	}

	// Verify namespace exists
	_, err = clientset.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to verify namespace '%s': %w", namespace, err)
	}

	log.Info("Successfully initialized ConfigMap store", "namespace", namespace)
	return store, nil
}

// getConfigMapName returns the ConfigMap name for a given cluster ID
func (c *ConfigMapStore) getConfigMapName(clusterID string) string {
	return fmt.Sprintf("external-name-backup-%s", clusterID)
}

// encodeKey base64-encodes a composition key for use as a ConfigMap data key
func (c *ConfigMapStore) encodeKey(compositionKey string) string {
	return base64.StdEncoding.EncodeToString([]byte(compositionKey))
}

// decodeKey decodes a base64-encoded ConfigMap data key back to a composition key
func (c *ConfigMapStore) decodeKey(encodedKey string) (string, error) {
	decoded, err := base64.StdEncoding.DecodeString(encodedKey)
	if err != nil {
		return "", fmt.Errorf("failed to decode key: %w", err)
	}
	return string(decoded), nil
}

// Save stores resource data for an entire composition in a ConfigMap
func (c *ConfigMapStore) Save(ctx context.Context, clusterID, compositionKey string, resources map[string]ResourceData) error {
	configMapName := c.getConfigMapName(clusterID)
	encodedKey := c.encodeKey(compositionKey)

	// Marshal resources to JSON
	resourcesJSON, err := json.Marshal(resources)
	if err != nil {
		return fmt.Errorf("failed to marshal resources to JSON: %w", err)
	}

	// Try to get existing ConfigMap
	configMap, err := c.client.CoreV1().ConfigMaps(c.namespace).Get(ctx, configMapName, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			// Create new ConfigMap
			configMap = &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      configMapName,
					Namespace: c.namespace,
				},
				Data: map[string]string{
					encodedKey: string(resourcesJSON),
				},
			}
			_, err = c.client.CoreV1().ConfigMaps(c.namespace).Create(ctx, configMap, metav1.CreateOptions{})
			if err != nil {
				return fmt.Errorf("failed to create ConfigMap: %w", err)
			}
			c.log.Debug("Created ConfigMap for cluster", "configmap", configMapName, "cluster-id", clusterID)
			return nil
		}
		return fmt.Errorf("failed to get ConfigMap: %w", err)
	}

	// Update existing ConfigMap
	if configMap.Data == nil {
		configMap.Data = make(map[string]string)
	}
	configMap.Data[encodedKey] = string(resourcesJSON)

	_, err = c.client.CoreV1().ConfigMaps(c.namespace).Update(ctx, configMap, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to update ConfigMap: %w", err)
	}

	c.log.Debug("Updated ConfigMap for composition", "configmap", configMapName, "composition-key", compositionKey)
	return nil
}

// Load retrieves all resource data for a composition from a ConfigMap
func (c *ConfigMapStore) Load(ctx context.Context, clusterID, compositionKey string) (map[string]ResourceData, error) {
	configMapName := c.getConfigMapName(clusterID)
	encodedKey := c.encodeKey(compositionKey)

	// Get the ConfigMap
	configMap, err := c.client.CoreV1().ConfigMaps(c.namespace).Get(ctx, configMapName, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			c.log.Debug("ConfigMap not found, returning empty data", "configmap", configMapName)
			return make(map[string]ResourceData), nil
		}
		return nil, fmt.Errorf("failed to get ConfigMap: %w", err)
	}

	// Get the data for this composition key
	resourcesJSON, exists := configMap.Data[encodedKey]
	if !exists {
		c.log.Debug("Composition key not found in ConfigMap", "composition-key", compositionKey)
		return make(map[string]ResourceData), nil
	}

	// Unmarshal the JSON data
	var resources map[string]ResourceData
	if err := json.Unmarshal([]byte(resourcesJSON), &resources); err != nil {
		return nil, fmt.Errorf("failed to unmarshal resource data: %w", err)
	}

	c.log.Debug("Loaded resource data from ConfigMap", "composition-key", compositionKey, "resource-count", len(resources))
	return resources, nil
}

// DeleteResource removes a specific resource's data from a composition
func (c *ConfigMapStore) DeleteResource(ctx context.Context, clusterID, compositionKey, resourceKey string) error {
	// Load all resources for this composition
	resources, err := c.Load(ctx, clusterID, compositionKey)
	if err != nil {
		return err
	}

	// Delete the specific resource
	delete(resources, resourceKey)

	// If no resources left, purge the composition key entirely
	if len(resources) == 0 {
		return c.Purge(ctx, clusterID, compositionKey)
	}

	// Save the updated resources
	return c.Save(ctx, clusterID, compositionKey, resources)
}

// Purge removes all data for a composition from the ConfigMap
func (c *ConfigMapStore) Purge(ctx context.Context, clusterID, compositionKey string) error {
	configMapName := c.getConfigMapName(clusterID)
	encodedKey := c.encodeKey(compositionKey)

	// Get the ConfigMap
	configMap, err := c.client.CoreV1().ConfigMaps(c.namespace).Get(ctx, configMapName, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			c.log.Debug("ConfigMap not found, nothing to purge", "configmap", configMapName)
			return nil
		}
		return fmt.Errorf("failed to get ConfigMap: %w", err)
	}

	// Remove the composition key from the ConfigMap
	if configMap.Data != nil {
		delete(configMap.Data, encodedKey)
	}

	// If ConfigMap is now empty, delete it
	if len(configMap.Data) == 0 {
		err = c.client.CoreV1().ConfigMaps(c.namespace).Delete(ctx, configMapName, metav1.DeleteOptions{})
		if err != nil {
			return fmt.Errorf("failed to delete ConfigMap: %w", err)
		}
		c.log.Debug("Deleted empty ConfigMap", "configmap", configMapName)
		return nil
	}

	// Update the ConfigMap
	_, err = c.client.CoreV1().ConfigMaps(c.namespace).Update(ctx, configMap, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to update ConfigMap: %w", err)
	}

	c.log.Debug("Purged composition from ConfigMap", "composition-key", compositionKey)
	return nil
}
