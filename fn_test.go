package main

import (
	"context"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/crossplane/function-sdk-go/logging"
	fnv1 "github.com/crossplane/function-sdk-go/proto/v1"
	"github.com/crossplane/function-sdk-go/resource"
)

func TestRunFunction(t *testing.T) {
	type args struct {
		ctx context.Context
		req *fnv1.RunFunctionRequest
	}
	type want struct {
		err                   error
		storeContains         map[string]string            // resourceKey -> externalName that should be in store after test
		storeNotContains      []string                     // resourceKeys that should NOT be in store after test
		desiredAnnotations    map[string]map[string]string // resourceName -> annotation -> value
		desiredNotAnnotations map[string][]string          // resourceName -> annotations that should NOT exist
	}

	cases := map[string]struct {
		reason string
		args   args
		want   want
		setup  func(*MockExternalNameStore) // Setup function to prepare mock store
	}{
		"StoreExternalNameForOrphanedResource": {
			reason: "Should store external name for orphaned resources with external-name annotation",
			args: args{
				ctx: context.Background(),
				req: &fnv1.RunFunctionRequest{
					Meta: &fnv1.RequestMeta{Tag: "test"},
					Input: resource.MustStructJSON(`{
						"apiVersion": "externalname.fn.crossplane.io/v1beta1",
						"kind": "Input"
					}`),
					Observed: &fnv1.State{
						Composite: &fnv1.Resource{
							Resource: resource.MustStructJSON(`{
								"apiVersion": "example.io/v1alpha1",
								"kind": "XExample",
								"metadata": {
									"name": "test-xr",
									"annotations": {
										"fn.crossplane.io/enable-external-store": "true",
										"fn.crossplane.io/store-type": "mock"
									},
									"labels": {
										"crossplane.io/claim-name": "test-claim",
										"crossplane.io/claim-namespace": "default"
									}
								}
							}`),
						},
						Resources: map[string]*fnv1.Resource{
							"bucket": {
								Resource: resource.MustStructJSON(`{
									"apiVersion": "s3.aws.upbound.io/v1beta1",
									"kind": "Bucket",
									"metadata": {
										"annotations": {
											"crossplane.io/external-name": "my-test-bucket"
										}
									}
								}`),
							},
						},
					},
					Desired: &fnv1.State{
						Composite: &fnv1.Resource{
							Resource: resource.MustStructJSON(`{
								"apiVersion": "example.io/v1alpha1",
								"kind": "XExample",
								"metadata": {
									"name": "test-xr",
									"annotations": {
										"fn.crossplane.io/enable-external-store": "true",
										"fn.crossplane.io/store-type": "mock"
									}
								}
							}`),
						},
						Resources: map[string]*fnv1.Resource{
							"bucket": {
								Resource: resource.MustStructJSON(`{
									"apiVersion": "s3.aws.upbound.io/v1beta1",
									"kind": "Bucket",
									"spec": {
										"deletionPolicy": "Orphan",
										"managementPolicies": ["*"]
									}
								}`),
							},
						},
					},
				},
			},
			want: want{
				err: nil,
				storeContains: map[string]string{
					"s3.aws.upbound.io/v1beta1/Bucket/bucket": "my-test-bucket",
				},
				desiredAnnotations: map[string]map[string]string{
					"bucket": {
						"fn.crossplane.io/stored-external-name": "my-test-bucket",
						"fn.crossplane.io/external-name-stored": "", // timestamp will vary
					},
				},
			},
		},

		"RestoreExternalNameFromStore": {
			reason: "Should restore external name from store for orphaned resources without external-name",
			setup: func(store *MockExternalNameStore) {
				store.Save(context.Background(), "default",
					"default/test-claim/example.io/v1alpha1/XExample/test-xr",
					map[string]string{
						"s3.aws.upbound.io/v1beta1/Bucket/bucket": "stored-bucket-name",
					})
			},
			args: args{
				ctx: context.Background(),
				req: &fnv1.RunFunctionRequest{
					Meta: &fnv1.RequestMeta{Tag: "test"},
					Input: resource.MustStructJSON(`{
						"apiVersion": "externalname.fn.crossplane.io/v1beta1",
						"kind": "Input"
					}`),
					Observed: &fnv1.State{
						Composite: &fnv1.Resource{
							Resource: resource.MustStructJSON(`{
								"apiVersion": "example.io/v1alpha1",
								"kind": "XExample",
								"metadata": {
									"name": "test-xr",
									"annotations": {
										"fn.crossplane.io/enable-external-store": "true",
										"fn.crossplane.io/store-type": "mock"
									},
									"labels": {
										"crossplane.io/claim-name": "test-claim",
										"crossplane.io/claim-namespace": "default"
									}
								}
							}`),
						},
					},
					Desired: &fnv1.State{
						Composite: &fnv1.Resource{
							Resource: resource.MustStructJSON(`{
								"apiVersion": "example.io/v1alpha1",
								"kind": "XExample",
								"metadata": {
									"name": "test-xr",
									"annotations": {
										"fn.crossplane.io/enable-external-store": "true",
										"fn.crossplane.io/store-type": "mock"
									}
								}
							}`),
						},
						Resources: map[string]*fnv1.Resource{
							"bucket": {
								Resource: resource.MustStructJSON(`{
									"apiVersion": "s3.aws.upbound.io/v1beta1",
									"kind": "Bucket",
									"spec": {
										"deletionPolicy": "Orphan",
										"managementPolicies": ["*"]
									}
								}`),
							},
						},
					},
				},
			},
			want: want{
				err: nil,
				desiredAnnotations: map[string]map[string]string{
					"bucket": {
						"crossplane.io/external-name":             "stored-bucket-name",
						"fn.crossplane.io/external-name-restored": "", // timestamp will vary
					},
				},
			},
		},

		"DeleteExternalNameOnPolicyChange": {
			reason: "Should delete external name from store when resource changes from Orphan to Delete policy",
			setup: func(store *MockExternalNameStore) {
				store.Save(context.Background(), "default",
					"default/test-claim/example.io/v1alpha1/XExample/test-xr",
					map[string]string{
						"s3.aws.upbound.io/v1beta1/Bucket/bucket": "bucket-to-delete",
					})
			},
			args: args{
				ctx: context.Background(),
				req: &fnv1.RunFunctionRequest{
					Meta: &fnv1.RequestMeta{Tag: "test"},
					Input: resource.MustStructJSON(`{
						"apiVersion": "externalname.fn.crossplane.io/v1beta1",
						"kind": "Input"
					}`),
					Observed: &fnv1.State{
						Composite: &fnv1.Resource{
							Resource: resource.MustStructJSON(`{
								"apiVersion": "example.io/v1alpha1",
								"kind": "XExample",
								"metadata": {
									"name": "test-xr",
									"annotations": {
										"fn.crossplane.io/enable-external-store": "true",
										"fn.crossplane.io/store-type": "mock"
									},
									"labels": {
										"crossplane.io/claim-name": "test-claim",
										"crossplane.io/claim-namespace": "default"
									}
								}
							}`),
						},
						Resources: map[string]*fnv1.Resource{
							"bucket": {
								Resource: resource.MustStructJSON(`{
									"apiVersion": "s3.aws.upbound.io/v1beta1",
									"kind": "Bucket",
									"metadata": {
										"annotations": {
											"fn.crossplane.io/stored-external-name": "bucket-to-delete"
										}
									}
								}`),
							},
						},
					},
					Desired: &fnv1.State{
						Composite: &fnv1.Resource{
							Resource: resource.MustStructJSON(`{
								"apiVersion": "example.io/v1alpha1",
								"kind": "XExample",
								"metadata": {
									"name": "test-xr",
									"annotations": {
										"fn.crossplane.io/enable-external-store": "true",
										"fn.crossplane.io/store-type": "mock"
									}
								}
							}`),
						},
						Resources: map[string]*fnv1.Resource{
							"bucket": {
								Resource: resource.MustStructJSON(`{
									"apiVersion": "s3.aws.upbound.io/v1beta1",
									"kind": "Bucket",
									"spec": {
										"deletionPolicy": "Delete",
										"managementPolicies": ["*"]
									}
								}`),
							},
						},
					},
				},
			},
			want: want{
				err: nil,
				storeNotContains: []string{
					"s3.aws.upbound.io/v1beta1/Bucket/bucket",
				},
				desiredAnnotations: map[string]map[string]string{
					"bucket": {
						"fn.crossplane.io/external-name-deleted": "", // timestamp will vary
					},
				},
				desiredNotAnnotations: map[string][]string{
					"bucket": {
						"fn.crossplane.io/stored-external-name",
						"fn.crossplane.io/external-name-stored",
					},
				},
			},
		},

		"SkipNonOrphanedResources": {
			reason: "Should skip storing external names for resources that will actually be deleted (not orphaned)",
			args: args{
				ctx: context.Background(),
				req: &fnv1.RunFunctionRequest{
					Meta: &fnv1.RequestMeta{Tag: "test"},
					Input: resource.MustStructJSON(`{
						"apiVersion": "externalname.fn.crossplane.io/v1beta1",
						"kind": "Input"
					}`),
					Observed: &fnv1.State{
						Composite: &fnv1.Resource{
							Resource: resource.MustStructJSON(`{
								"apiVersion": "example.io/v1alpha1",
								"kind": "XExample",
								"metadata": {
									"name": "test-xr",
									"annotations": {
										"fn.crossplane.io/enable-external-store": "true",
										"fn.crossplane.io/store-type": "mock"
									},
									"labels": {
										"crossplane.io/claim-name": "test-claim",
										"crossplane.io/claim-namespace": "default"
									}
								}
							}`),
						},
						Resources: map[string]*fnv1.Resource{
							"bucket": {
								Resource: resource.MustStructJSON(`{
									"apiVersion": "s3.aws.upbound.io/v1beta1",
									"kind": "Bucket",
									"metadata": {
										"annotations": {
											"crossplane.io/external-name": "non-orphaned-bucket"
										}
									}
								}`),
							},
						},
					},
					Desired: &fnv1.State{
						Composite: &fnv1.Resource{
							Resource: resource.MustStructJSON(`{
								"apiVersion": "example.io/v1alpha1",
								"kind": "XExample",
								"metadata": {
									"name": "test-xr",
									"annotations": {
										"fn.crossplane.io/enable-external-store": "true",
										"fn.crossplane.io/store-type": "mock"
									}
								}
							}`),
						},
						Resources: map[string]*fnv1.Resource{
							"bucket": {
								Resource: resource.MustStructJSON(`{
									"apiVersion": "s3.aws.upbound.io/v1beta1",
									"kind": "Bucket",
									"spec": {
										"deletionPolicy": "Delete",
										"managementPolicies": ["*"]
									}
								}`),
							},
						},
					},
				},
			},
			want: want{
				err: nil,
				storeNotContains: []string{
					"s3.aws.upbound.io/v1beta1/Bucket/bucket",
				},
			},
		},

		"AnnotationFallbackDesiredToObserved": {
			reason: "Should find stored-external-name annotation in observed resource when not in desired",
			setup: func(store *MockExternalNameStore) {
				store.Save(context.Background(), "default",
					"default/test-claim/example.io/v1alpha1/XExample/test-xr",
					map[string]string{
						"s3.aws.upbound.io/v1beta1/Bucket/bucket": "bucket-to-delete",
					})
			},
			args: args{
				ctx: context.Background(),
				req: &fnv1.RunFunctionRequest{
					Meta: &fnv1.RequestMeta{Tag: "test"},
					Input: resource.MustStructJSON(`{
						"apiVersion": "externalname.fn.crossplane.io/v1beta1",
						"kind": "Input"
					}`),
					Observed: &fnv1.State{
						Composite: &fnv1.Resource{
							Resource: resource.MustStructJSON(`{
								"apiVersion": "example.io/v1alpha1",
								"kind": "XExample",
								"metadata": {
									"name": "test-xr",
									"annotations": {
										"fn.crossplane.io/enable-external-store": "true",
										"fn.crossplane.io/store-type": "mock"
									},
									"labels": {
										"crossplane.io/claim-name": "test-claim",
										"crossplane.io/claim-namespace": "default"
									}
								}
							}`),
						},
						Resources: map[string]*fnv1.Resource{
							"bucket": {
								Resource: resource.MustStructJSON(`{
									"apiVersion": "s3.aws.upbound.io/v1beta1",
									"kind": "Bucket",
									"metadata": {
										"annotations": {
											"fn.crossplane.io/stored-external-name": "bucket-to-delete"
										}
									}
								}`),
							},
						},
					},
					Desired: &fnv1.State{
						Composite: &fnv1.Resource{
							Resource: resource.MustStructJSON(`{
								"apiVersion": "example.io/v1alpha1",
								"kind": "XExample",
								"metadata": {
									"name": "test-xr",
									"annotations": {
										"fn.crossplane.io/enable-external-store": "true",
										"fn.crossplane.io/store-type": "mock"
									}
								}
							}`),
						},
						Resources: map[string]*fnv1.Resource{
							"bucket": {
								Resource: resource.MustStructJSON(`{
									"apiVersion": "s3.aws.upbound.io/v1beta1",
									"kind": "Bucket",
									"spec": {
										"deletionPolicy": "Delete",
										"managementPolicies": ["*"]
									}
								}`),
							},
						},
					},
				},
			},
			want: want{
				err: nil,
				storeNotContains: []string{
					"s3.aws.upbound.io/v1beta1/Bucket/bucket",
				},
				desiredAnnotations: map[string]map[string]string{
					"bucket": {
						"fn.crossplane.io/external-name-deleted": "", // timestamp will vary
					},
				},
			},
		},

		"DeletionPolicyFallbackDesiredToObserved": {
			reason: "Should fallback to observed resource spec when desired resource has no spec",
			setup: func(store *MockExternalNameStore) {
				store.Save(context.Background(), "default",
					"default/test-claim/example.io/v1alpha1/XExample/test-xr",
					map[string]string{
						"s3.aws.upbound.io/v1beta1/Bucket/bucket": "bucket-to-delete",
					})
			},
			args: args{
				ctx: context.Background(),
				req: &fnv1.RunFunctionRequest{
					Meta: &fnv1.RequestMeta{Tag: "test"},
					Input: resource.MustStructJSON(`{
						"apiVersion": "externalname.fn.crossplane.io/v1beta1",
						"kind": "Input"
					}`),
					Observed: &fnv1.State{
						Composite: &fnv1.Resource{
							Resource: resource.MustStructJSON(`{
								"apiVersion": "example.io/v1alpha1",
								"kind": "XExample",
								"metadata": {
									"name": "test-xr",
									"annotations": {
										"fn.crossplane.io/enable-external-store": "true",
										"fn.crossplane.io/store-type": "mock"
									},
									"labels": {
										"crossplane.io/claim-name": "test-claim",
										"crossplane.io/claim-namespace": "default"
									}
								}
							}`),
						},
						Resources: map[string]*fnv1.Resource{
							"bucket": {
								Resource: resource.MustStructJSON(`{
									"apiVersion": "s3.aws.upbound.io/v1beta1",
									"kind": "Bucket",
									"metadata": {
										"annotations": {
											"fn.crossplane.io/stored-external-name": "bucket-to-delete"
										}
									},
									"spec": {
										"deletionPolicy": "Delete",
										"managementPolicies": ["*"]
									}
								}`),
							},
						},
					},
					Desired: &fnv1.State{
						Composite: &fnv1.Resource{
							Resource: resource.MustStructJSON(`{
								"apiVersion": "example.io/v1alpha1",
								"kind": "XExample",
								"metadata": {
									"name": "test-xr",
									"annotations": {
										"fn.crossplane.io/enable-external-store": "true",
										"fn.crossplane.io/store-type": "mock"
									}
								}
							}`),
						},
						Resources: map[string]*fnv1.Resource{
							"bucket": {
								Resource: resource.MustStructJSON(`{
									"apiVersion": "s3.aws.upbound.io/v1beta1",
									"kind": "Bucket"
								}`),
							},
						},
					},
				},
			},
			want: want{
				err: nil,
				storeNotContains: []string{
					"s3.aws.upbound.io/v1beta1/Bucket/bucket",
				},
				desiredAnnotations: map[string]map[string]string{
					"bucket": {
						"fn.crossplane.io/external-name-deleted": "", // timestamp will vary
					},
				},
			},
		},

		"SkipAlreadyStoredResource": {
			reason: "Should skip storing external name when resource already has stored-external-name annotation with same value",
			args: args{
				ctx: context.Background(),
				req: &fnv1.RunFunctionRequest{
					Meta: &fnv1.RequestMeta{Tag: "test"},
					Input: resource.MustStructJSON(`{
						"apiVersion": "externalname.fn.crossplane.io/v1beta1",
						"kind": "Input"
					}`),
					Observed: &fnv1.State{
						Composite: &fnv1.Resource{
							Resource: resource.MustStructJSON(`{
								"apiVersion": "example.io/v1alpha1",
								"kind": "XExample",
								"metadata": {
									"name": "test-xr",
									"annotations": {
										"fn.crossplane.io/enable-external-store": "true",
										"fn.crossplane.io/store-type": "mock"
									},
									"labels": {
										"crossplane.io/claim-name": "test-claim",
										"crossplane.io/claim-namespace": "default"
									}
								}
							}`),
						},
						Resources: map[string]*fnv1.Resource{
							"bucket": {
								Resource: resource.MustStructJSON(`{
									"apiVersion": "s3.aws.upbound.io/v1beta1",
									"kind": "Bucket",
									"metadata": {
										"annotations": {
											"crossplane.io/external-name": "my-bucket",
											"fn.crossplane.io/stored-external-name": "my-bucket"
										}
									}
								}`),
							},
						},
					},
					Desired: &fnv1.State{
						Composite: &fnv1.Resource{
							Resource: resource.MustStructJSON(`{
								"apiVersion": "example.io/v1alpha1",
								"kind": "XExample",
								"metadata": {
									"name": "test-xr",
									"annotations": {
										"fn.crossplane.io/enable-external-store": "true",
										"fn.crossplane.io/store-type": "mock"
									}
								}
							}`),
						},
						Resources: map[string]*fnv1.Resource{
							"bucket": {
								Resource: resource.MustStructJSON(`{
									"apiVersion": "s3.aws.upbound.io/v1beta1",
									"kind": "Bucket",
									"spec": {
										"deletionPolicy": "Orphan",
										"managementPolicies": ["*"]
									}
								}`),
							},
						},
					},
				},
			},
			want: want{
				err: nil,
				storeNotContains: []string{
					"s3.aws.upbound.io/v1beta1/Bucket/bucket", // Should not be stored again
				},
			},
		},

		"SkipRestorationWhenExternalNameExists": {
			reason: "Should skip restoration when desired resource already has external-name annotation",
			setup: func(store *MockExternalNameStore) {
				store.Save(context.Background(), "default",
					"default/test-claim/example.io/v1alpha1/XExample/test-xr",
					map[string]string{
						"s3.aws.upbound.io/v1beta1/Bucket/bucket": "stored-bucket-name",
					})
			},
			args: args{
				ctx: context.Background(),
				req: &fnv1.RunFunctionRequest{
					Meta: &fnv1.RequestMeta{Tag: "test"},
					Input: resource.MustStructJSON(`{
						"apiVersion": "externalname.fn.crossplane.io/v1beta1",
						"kind": "Input"
					}`),
					Observed: &fnv1.State{
						Composite: &fnv1.Resource{
							Resource: resource.MustStructJSON(`{
								"apiVersion": "example.io/v1alpha1",
								"kind": "XExample",
								"metadata": {
									"name": "test-xr",
									"annotations": {
										"fn.crossplane.io/enable-external-store": "true",
										"fn.crossplane.io/store-type": "mock"
									},
									"labels": {
										"crossplane.io/claim-name": "test-claim",
										"crossplane.io/claim-namespace": "default"
									}
								}
							}`),
						},
					},
					Desired: &fnv1.State{
						Composite: &fnv1.Resource{
							Resource: resource.MustStructJSON(`{
								"apiVersion": "example.io/v1alpha1",
								"kind": "XExample",
								"metadata": {
									"name": "test-xr",
									"annotations": {
										"fn.crossplane.io/enable-external-store": "true",
										"fn.crossplane.io/store-type": "mock"
									}
								}
							}`),
						},
						Resources: map[string]*fnv1.Resource{
							"bucket": {
								Resource: resource.MustStructJSON(`{
									"apiVersion": "s3.aws.upbound.io/v1beta1",
									"kind": "Bucket",
									"metadata": {
										"annotations": {
											"crossplane.io/external-name": "existing-external-name"
										}
									},
									"spec": {
										"deletionPolicy": "Orphan",
										"managementPolicies": ["*"]
									}
								}`),
							},
						},
					},
				},
			},
			want: want{
				err: nil,
				desiredAnnotations: map[string]map[string]string{
					"bucket": {
						"crossplane.io/external-name": "existing-external-name", // Should remain unchanged
					},
				},
				desiredNotAnnotations: map[string][]string{
					"bucket": {
						"fn.crossplane.io/external-name-restored", // Should not have restoration annotation
					},
				},
			},
		},

		"AllResourcesMode": {
			reason: "Should process all resources regardless of deletion policy when in all-resources mode",
			args: args{
				ctx: context.Background(),
				req: &fnv1.RunFunctionRequest{
					Meta: &fnv1.RequestMeta{Tag: "test"},
					Input: resource.MustStructJSON(`{
						"apiVersion": "externalname.fn.crossplane.io/v1beta1",
						"kind": "Input"
					}`),
					Observed: &fnv1.State{
						Composite: &fnv1.Resource{
							Resource: resource.MustStructJSON(`{
								"apiVersion": "example.io/v1alpha1",
								"kind": "XExample",
								"metadata": {
									"name": "test-xr",
									"annotations": {
										"fn.crossplane.io/enable-external-store": "true",
										"fn.crossplane.io/store-type": "mock",
										"fn.crossplane.io/operation-mode": "all-resources"
									},
									"labels": {
										"crossplane.io/claim-name": "test-claim",
										"crossplane.io/claim-namespace": "default"
									}
								}
							}`),
						},
						Resources: map[string]*fnv1.Resource{
							"bucket": {
								Resource: resource.MustStructJSON(`{
									"apiVersion": "s3.aws.upbound.io/v1beta1",
									"kind": "Bucket",
									"metadata": {
										"annotations": {
											"crossplane.io/external-name": "delete-policy-bucket"
										}
									}
								}`),
							},
						},
					},
					Desired: &fnv1.State{
						Composite: &fnv1.Resource{
							Resource: resource.MustStructJSON(`{
								"apiVersion": "example.io/v1alpha1",
								"kind": "XExample",
								"metadata": {
									"name": "test-xr",
									"annotations": {
										"fn.crossplane.io/enable-external-store": "true",
										"fn.crossplane.io/store-type": "mock",
										"fn.crossplane.io/operation-mode": "all-resources"
									}
								}
							}`),
						},
						Resources: map[string]*fnv1.Resource{
							"bucket": {
								Resource: resource.MustStructJSON(`{
									"apiVersion": "s3.aws.upbound.io/v1beta1",
									"kind": "Bucket",
									"spec": {
										"deletionPolicy": "Delete",
										"managementPolicies": ["*"]
									}
								}`),
							},
						},
					},
				},
			},
			want: want{
				err: nil,
				storeContains: map[string]string{
					"s3.aws.upbound.io/v1beta1/Bucket/bucket": "delete-policy-bucket",
				},
				desiredAnnotations: map[string]map[string]string{
					"bucket": {
						"fn.crossplane.io/stored-external-name": "delete-policy-bucket",
						"fn.crossplane.io/external-name-stored": "", // timestamp will vary
					},
				},
			},
		},

		"MissingClaimInfo": {
			reason: "Should handle missing claim namespace/name gracefully",
			args: args{
				ctx: context.Background(),
				req: &fnv1.RunFunctionRequest{
					Meta: &fnv1.RequestMeta{Tag: "test"},
					Input: resource.MustStructJSON(`{
						"apiVersion": "externalname.fn.crossplane.io/v1beta1",
						"kind": "Input"
					}`),
					Observed: &fnv1.State{
						Composite: &fnv1.Resource{
							Resource: resource.MustStructJSON(`{
								"apiVersion": "example.io/v1alpha1",
								"kind": "XExample",
								"metadata": {
									"name": "test-xr",
									"annotations": {
										"fn.crossplane.io/enable-external-store": "true",
										"fn.crossplane.io/store-type": "mock"
									}
								}
							}`),
						},
						Resources: map[string]*fnv1.Resource{
							"bucket": {
								Resource: resource.MustStructJSON(`{
									"apiVersion": "s3.aws.upbound.io/v1beta1",
									"kind": "Bucket",
									"metadata": {
										"annotations": {
											"crossplane.io/external-name": "standalone-bucket"
										}
									}
								}`),
							},
						},
					},
					Desired: &fnv1.State{
						Composite: &fnv1.Resource{
							Resource: resource.MustStructJSON(`{
								"apiVersion": "example.io/v1alpha1",
								"kind": "XExample",
								"metadata": {
									"name": "test-xr",
									"annotations": {
										"fn.crossplane.io/enable-external-store": "true",
										"fn.crossplane.io/store-type": "mock"
									}
								}
							}`),
						},
						Resources: map[string]*fnv1.Resource{
							"bucket": {
								Resource: resource.MustStructJSON(`{
									"apiVersion": "s3.aws.upbound.io/v1beta1",
									"kind": "Bucket",
									"spec": {
										"deletionPolicy": "Orphan",
										"managementPolicies": ["*"]
									}
								}`),
							},
						},
					},
				},
			},
			want: want{
				err: nil,
				storeContains: map[string]string{
					"s3.aws.upbound.io/v1beta1/Bucket/bucket": "standalone-bucket",
				},
			},
		},

		"DifferentXRTypes": {
			reason: "Should handle different XR apiVersions and kinds correctly",
			args: args{
				ctx: context.Background(),
				req: &fnv1.RunFunctionRequest{
					Meta: &fnv1.RequestMeta{Tag: "test"},
					Input: resource.MustStructJSON(`{
						"apiVersion": "externalname.fn.crossplane.io/v1beta1",
						"kind": "Input"
					}`),
					Observed: &fnv1.State{
						Composite: &fnv1.Resource{
							Resource: resource.MustStructJSON(`{
								"apiVersion": "platform.example.com/v1beta2", 
								"kind": "XDatabase",
								"metadata": {
									"name": "my-db-xr",
									"annotations": {
										"fn.crossplane.io/enable-external-store": "true",
										"fn.crossplane.io/store-type": "mock"
									},
									"labels": {
										"crossplane.io/claim-name": "my-database",
										"crossplane.io/claim-namespace": "production"
									}
								}
							}`),
						},
						Resources: map[string]*fnv1.Resource{
							"database": {
								Resource: resource.MustStructJSON(`{
									"apiVersion": "rds.aws.upbound.io/v1beta1",
									"kind": "Instance",
									"metadata": {
										"annotations": {
											"crossplane.io/external-name": "prod-db-instance"
										}
									}
								}`),
							},
						},
					},
					Desired: &fnv1.State{
						Composite: &fnv1.Resource{
							Resource: resource.MustStructJSON(`{
								"apiVersion": "platform.example.com/v1beta2",
								"kind": "XDatabase", 
								"metadata": {
									"name": "my-db-xr",
									"annotations": {
										"fn.crossplane.io/enable-external-store": "true",
										"fn.crossplane.io/store-type": "mock"
									}
								}
							}`),
						},
						Resources: map[string]*fnv1.Resource{
							"database": {
								Resource: resource.MustStructJSON(`{
									"apiVersion": "rds.aws.upbound.io/v1beta1",
									"kind": "Instance",
									"spec": {
										"deletionPolicy": "Orphan",
										"managementPolicies": ["*"]
									}
								}`),
							},
						},
					},
				},
			},
			want: want{
				err: nil,
				storeContains: map[string]string{
					"rds.aws.upbound.io/v1beta1/Instance/database": "prod-db-instance",
				},
			},
		},

		"ResourceWithoutSpec": {
			reason: "Should handle resources without spec gracefully in only-orphaned mode",
			args: args{
				ctx: context.Background(),
				req: &fnv1.RunFunctionRequest{
					Meta: &fnv1.RequestMeta{Tag: "test"},
					Input: resource.MustStructJSON(`{
						"apiVersion": "externalname.fn.crossplane.io/v1beta1",
						"kind": "Input"
					}`),
					Observed: &fnv1.State{
						Composite: &fnv1.Resource{
							Resource: resource.MustStructJSON(`{
								"apiVersion": "example.io/v1alpha1",
								"kind": "XExample",
								"metadata": {
									"name": "test-xr",
									"annotations": {
										"fn.crossplane.io/enable-external-store": "true",
										"fn.crossplane.io/store-type": "mock"
									},
									"labels": {
										"crossplane.io/claim-name": "test-claim",
										"crossplane.io/claim-namespace": "default"
									}
								}
							}`),
						},
						Resources: map[string]*fnv1.Resource{
							"configmap": {
								Resource: resource.MustStructJSON(`{
									"apiVersion": "v1",
									"kind": "ConfigMap",
									"metadata": {
										"annotations": {
											"crossplane.io/external-name": "my-configmap"
										}
									}
								}`),
							},
						},
					},
					Desired: &fnv1.State{
						Composite: &fnv1.Resource{
							Resource: resource.MustStructJSON(`{
								"apiVersion": "example.io/v1alpha1",
								"kind": "XExample",
								"metadata": {
									"name": "test-xr",
									"annotations": {
										"fn.crossplane.io/enable-external-store": "true",
										"fn.crossplane.io/store-type": "mock"
									}
								}
							}`),
						},
						Resources: map[string]*fnv1.Resource{
							"configmap": {
								Resource: resource.MustStructJSON(`{
									"apiVersion": "v1",
									"kind": "ConfigMap"
								}`),
							},
						},
					},
				},
			},
			want: want{
				err: nil,
				storeNotContains: []string{
					"v1/ConfigMap/configmap", // Should not be stored due to missing spec
				},
			},
		},

		"MultiResourceMixedOperations": {
			reason: "Should handle storage, restoration, and deletion simultaneously for different resources",
			setup: func(store *MockExternalNameStore) {
				store.Save(context.Background(), "default",
					"default/test-claim/example.io/v1alpha1/XExample/test-xr",
					map[string]string{
						"s3.aws.upbound.io/v1beta1/Bucket/bucket-restore": "old-bucket-name",
						"s3.aws.upbound.io/v1beta1/Bucket/bucket-delete":  "bucket-to-delete",
					})
			},
			args: args{
				ctx: context.Background(),
				req: &fnv1.RunFunctionRequest{
					Meta: &fnv1.RequestMeta{Tag: "test"},
					Input: resource.MustStructJSON(`{
						"apiVersion": "externalname.fn.crossplane.io/v1beta1",
						"kind": "Input"
					}`),
					Observed: &fnv1.State{
						Composite: &fnv1.Resource{
							Resource: resource.MustStructJSON(`{
								"apiVersion": "example.io/v1alpha1",
								"kind": "XExample",
								"metadata": {
									"name": "test-xr",
									"annotations": {
										"fn.crossplane.io/enable-external-store": "true",
										"fn.crossplane.io/store-type": "mock"
									},
									"labels": {
										"crossplane.io/claim-name": "test-claim",
										"crossplane.io/claim-namespace": "default"
									}
								}
							}`),
						},
						Resources: map[string]*fnv1.Resource{
							"bucket-store": {
								Resource: resource.MustStructJSON(`{
									"apiVersion": "s3.aws.upbound.io/v1beta1",
									"kind": "Bucket",
									"metadata": {
										"annotations": {
											"crossplane.io/external-name": "new-bucket-to-store"
										}
									}
								}`),
							},
							"bucket-delete": {
								Resource: resource.MustStructJSON(`{
									"apiVersion": "s3.aws.upbound.io/v1beta1",
									"kind": "Bucket",
									"metadata": {
										"annotations": {
											"fn.crossplane.io/stored-external-name": "bucket-to-delete"
										}
									}
								}`),
							},
						},
					},
					Desired: &fnv1.State{
						Composite: &fnv1.Resource{
							Resource: resource.MustStructJSON(`{
								"apiVersion": "example.io/v1alpha1",
								"kind": "XExample",
								"metadata": {
									"name": "test-xr",
									"annotations": {
										"fn.crossplane.io/enable-external-store": "true",
										"fn.crossplane.io/store-type": "mock"
									}
								}
							}`),
						},
						Resources: map[string]*fnv1.Resource{
							"bucket-store": {
								Resource: resource.MustStructJSON(`{
									"apiVersion": "s3.aws.upbound.io/v1beta1",
									"kind": "Bucket",
									"spec": {
										"deletionPolicy": "Orphan",
										"managementPolicies": ["*"]
									}
								}`),
							},
							"bucket-restore": {
								Resource: resource.MustStructJSON(`{
									"apiVersion": "s3.aws.upbound.io/v1beta1",
									"kind": "Bucket",
									"spec": {
										"deletionPolicy": "Orphan",
										"managementPolicies": ["*"]
									}
								}`),
							},
							"bucket-delete": {
								Resource: resource.MustStructJSON(`{
									"apiVersion": "s3.aws.upbound.io/v1beta1",
									"kind": "Bucket",
									"spec": {
										"deletionPolicy": "Delete",
										"managementPolicies": ["*"]
									}
								}`),
							},
						},
					},
				},
			},
			want: want{
				err: nil,
				storeContains: map[string]string{
					"s3.aws.upbound.io/v1beta1/Bucket/bucket-store":   "new-bucket-to-store",
					"s3.aws.upbound.io/v1beta1/Bucket/bucket-restore": "old-bucket-name", // Should remain from setup
				},
				storeNotContains: []string{
					"s3.aws.upbound.io/v1beta1/Bucket/bucket-delete", // Should be deleted
				},
				desiredAnnotations: map[string]map[string]string{
					"bucket-store": {
						"fn.crossplane.io/stored-external-name": "new-bucket-to-store",
						"fn.crossplane.io/external-name-stored": "", // timestamp will vary
					},
					"bucket-restore": {
						"crossplane.io/external-name":             "old-bucket-name",
						"fn.crossplane.io/external-name-restored": "", // timestamp will vary
					},
					"bucket-delete": {
						"fn.crossplane.io/external-name-deleted": "", // timestamp will vary
					},
				},
				desiredNotAnnotations: map[string][]string{
					"bucket-delete": {
						"fn.crossplane.io/stored-external-name",
						"fn.crossplane.io/external-name-stored",
					},
				},
			},
		},

		"PartialCompositionUpdate": {
			reason: "Should handle partial updates where some resources change while others remain the same",
			setup: func(store *MockExternalNameStore) {
				store.Save(context.Background(), "default",
					"default/test-claim/example.io/v1alpha1/XExample/test-xr",
					map[string]string{
						"s3.aws.upbound.io/v1beta1/Bucket/bucket-unchanged": "unchanged-bucket",
						"s3.aws.upbound.io/v1beta1/Bucket/bucket-changing":  "old-changing-bucket",
					})
			},
			args: args{
				ctx: context.Background(),
				req: &fnv1.RunFunctionRequest{
					Meta: &fnv1.RequestMeta{Tag: "test"},
					Input: resource.MustStructJSON(`{
						"apiVersion": "externalname.fn.crossplane.io/v1beta1",
						"kind": "Input"
					}`),
					Observed: &fnv1.State{
						Composite: &fnv1.Resource{
							Resource: resource.MustStructJSON(`{
								"apiVersion": "example.io/v1alpha1",
								"kind": "XExample",
								"metadata": {
									"name": "test-xr",
									"annotations": {
										"fn.crossplane.io/enable-external-store": "true",
										"fn.crossplane.io/store-type": "mock"
									},
									"labels": {
										"crossplane.io/claim-name": "test-claim",
										"crossplane.io/claim-namespace": "default"
									}
								}
							}`),
						},
						Resources: map[string]*fnv1.Resource{
							"bucket-unchanged": {
								Resource: resource.MustStructJSON(`{
									"apiVersion": "s3.aws.upbound.io/v1beta1",
									"kind": "Bucket",
									"metadata": {
										"annotations": {
											"crossplane.io/external-name": "unchanged-bucket",
											"fn.crossplane.io/stored-external-name": "unchanged-bucket"
										}
									}
								}`),
							},
							"bucket-changing": {
								Resource: resource.MustStructJSON(`{
									"apiVersion": "s3.aws.upbound.io/v1beta1",
									"kind": "Bucket",
									"metadata": {
										"annotations": {
											"crossplane.io/external-name": "new-changing-bucket"
										}
									}
								}`),
							},
						},
					},
					Desired: &fnv1.State{
						Composite: &fnv1.Resource{
							Resource: resource.MustStructJSON(`{
								"apiVersion": "example.io/v1alpha1",
								"kind": "XExample",
								"metadata": {
									"name": "test-xr",
									"annotations": {
										"fn.crossplane.io/enable-external-store": "true",
										"fn.crossplane.io/store-type": "mock"
									}
								}
							}`),
						},
						Resources: map[string]*fnv1.Resource{
							"bucket-unchanged": {
								Resource: resource.MustStructJSON(`{
									"apiVersion": "s3.aws.upbound.io/v1beta1",
									"kind": "Bucket",
									"spec": {
										"deletionPolicy": "Orphan",
										"managementPolicies": ["*"]
									}
								}`),
							},
							"bucket-changing": {
								Resource: resource.MustStructJSON(`{
									"apiVersion": "s3.aws.upbound.io/v1beta1",
									"kind": "Bucket",
									"spec": {
										"deletionPolicy": "Orphan",
										"managementPolicies": ["*"]
									}
								}`),
							},
						},
					},
				},
			},
			want: want{
				err: nil,
				storeContains: map[string]string{
					"s3.aws.upbound.io/v1beta1/Bucket/bucket-unchanged": "unchanged-bucket",
					"s3.aws.upbound.io/v1beta1/Bucket/bucket-changing":  "new-changing-bucket", // Should be updated
				},
				desiredAnnotations: map[string]map[string]string{
					"bucket-changing": {
						"fn.crossplane.io/stored-external-name": "new-changing-bucket",
						"fn.crossplane.io/external-name-stored": "", // timestamp will vary
					},
				},
			},
		},

		"ResourceTransitioningPolicies": {
			reason: "Should handle resources transitioning between orphan and delete policies correctly",
			setup: func(store *MockExternalNameStore) {
				store.Save(context.Background(), "default",
					"default/test-claim/example.io/v1alpha1/XExample/test-xr",
					map[string]string{
						"s3.aws.upbound.io/v1beta1/Bucket/bucket-orphan-to-delete": "bucket1",
						"s3.aws.upbound.io/v1beta1/Bucket/bucket-delete-to-orphan": "bucket2",
					})
			},
			args: args{
				ctx: context.Background(),
				req: &fnv1.RunFunctionRequest{
					Meta: &fnv1.RequestMeta{Tag: "test"},
					Input: resource.MustStructJSON(`{
						"apiVersion": "externalname.fn.crossplane.io/v1beta1",
						"kind": "Input"
					}`),
					Observed: &fnv1.State{
						Composite: &fnv1.Resource{
							Resource: resource.MustStructJSON(`{
								"apiVersion": "example.io/v1alpha1",
								"kind": "XExample",
								"metadata": {
									"name": "test-xr",
									"annotations": {
										"fn.crossplane.io/enable-external-store": "true",
										"fn.crossplane.io/store-type": "mock"
									},
									"labels": {
										"crossplane.io/claim-name": "test-claim",
										"crossplane.io/claim-namespace": "default"
									}
								}
							}`),
						},
						Resources: map[string]*fnv1.Resource{
							"bucket-orphan-to-delete": {
								Resource: resource.MustStructJSON(`{
									"apiVersion": "s3.aws.upbound.io/v1beta1",
									"kind": "Bucket",
									"metadata": {
										"annotations": {
											"fn.crossplane.io/stored-external-name": "bucket1"
										}
									}
								}`),
							},
							"bucket-delete-to-orphan": {
								Resource: resource.MustStructJSON(`{
									"apiVersion": "s3.aws.upbound.io/v1beta1",
									"kind": "Bucket",
									"metadata": {
										"annotations": {
											"crossplane.io/external-name": "bucket2"
										}
									}
								}`),
							},
						},
					},
					Desired: &fnv1.State{
						Composite: &fnv1.Resource{
							Resource: resource.MustStructJSON(`{
								"apiVersion": "example.io/v1alpha1",
								"kind": "XExample",
								"metadata": {
									"name": "test-xr",
									"annotations": {
										"fn.crossplane.io/enable-external-store": "true",
										"fn.crossplane.io/store-type": "mock"
									}
								}
							}`),
						},
						Resources: map[string]*fnv1.Resource{
							"bucket-orphan-to-delete": {
								Resource: resource.MustStructJSON(`{
									"apiVersion": "s3.aws.upbound.io/v1beta1",
									"kind": "Bucket",
									"spec": {
										"deletionPolicy": "Delete",
										"managementPolicies": ["*"]
									}
								}`),
							},
							"bucket-delete-to-orphan": {
								Resource: resource.MustStructJSON(`{
									"apiVersion": "s3.aws.upbound.io/v1beta1",
									"kind": "Bucket",
									"spec": {
										"deletionPolicy": "Orphan",
										"managementPolicies": ["*"]
									}
								}`),
							},
						},
					},
				},
			},
			want: want{
				err: nil,
				storeContains: map[string]string{
					"s3.aws.upbound.io/v1beta1/Bucket/bucket-delete-to-orphan": "bucket2", // Should be stored
				},
				storeNotContains: []string{
					"s3.aws.upbound.io/v1beta1/Bucket/bucket-orphan-to-delete", // Should be deleted
				},
				desiredAnnotations: map[string]map[string]string{
					"bucket-orphan-to-delete": {
						"fn.crossplane.io/external-name-deleted": "", // timestamp will vary
					},
					"bucket-delete-to-orphan": {
						"fn.crossplane.io/stored-external-name": "bucket2",
						"fn.crossplane.io/external-name-stored": "", // timestamp will vary
					},
				},
			},
		},
		"ExternalStoreDisabledByDefault": {
			reason: "Should skip all external store operations when no enable annotation is present",
			args: args{
				ctx: context.Background(),
				req: &fnv1.RunFunctionRequest{
					Observed: &fnv1.State{
						Composite: &fnv1.Resource{
							Resource: resource.MustStructJSON(`{
								"apiVersion": "example.io/v1alpha1",
								"kind": "XExample",
								"metadata": {
									"name": "test-xr",
									"labels": {
										"crossplane.io/claim-name": "test-claim",
										"crossplane.io/claim-namespace": "default"
									}
								}
							}`),
						},
						Resources: map[string]*fnv1.Resource{
							"bucket": {
								Resource: resource.MustStructJSON(`{
									"apiVersion": "s3.aws.upbound.io/v1beta1",
									"kind": "Bucket",
									"metadata": {
										"annotations": {
											"crossplane.io/external-name": "my-test-bucket"
										}
									}
								}`),
							},
						},
					},
					Desired: &fnv1.State{
						Composite: &fnv1.Resource{
							Resource: resource.MustStructJSON(`{
								"apiVersion": "example.io/v1alpha1",
								"kind": "XExample",
								"metadata": {
									"name": "test-xr"
								}
							}`),
						},
						Resources: map[string]*fnv1.Resource{
							"bucket": {
								Resource: resource.MustStructJSON(`{
									"apiVersion": "s3.aws.upbound.io/v1beta1",
									"kind": "Bucket"
								}`),
							},
						},
					},
				},
			},
			want: want{
				err: nil,
				storeNotContains: []string{
					"s3.aws.upbound.io/v1beta1/Bucket/bucket", // Should not be stored without enable annotation
				},
			},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			// Create fresh mock store for each test
			mockStore := &MockExternalNameStore{
				mu:   sync.RWMutex{},
				data: make(map[string]map[string]map[string]string),
			}
			SetTestStore(mockStore)
			defer ClearTestStore()

			// Run setup if provided
			if tc.setup != nil {
				tc.setup(mockStore)
			}

			f := &Function{
				log: logging.NewNopLogger(),
			}

			rsp, err := f.RunFunction(tc.args.ctx, tc.args.req)

			// Check error
			if diff := cmp.Diff(tc.want.err, err, cmpopts.EquateErrors()); diff != "" {
				t.Errorf("%s\nf.RunFunction(...): -want err, +got err:\n%s", tc.reason, diff)
			}

			// Function should always return a response
			if rsp == nil {
				t.Errorf("%s\nExpected response, got nil", tc.reason)
				return
			}

			// Check that the function succeeded (no fatal errors)
			if rsp.GetResults() != nil {
				for _, result := range rsp.GetResults() {
					if result.GetSeverity() == fnv1.Severity_SEVERITY_FATAL {
						t.Errorf("%s\nFunction returned fatal error: %s", tc.reason, result.GetMessage())
					}
				}
			}

			// Check store state - generate expected composition key based on test case
			var compositionKey string
			switch name {
			case "MissingClaimInfo":
				compositionKey = "none/none/example.io/v1alpha1/XExample/test-xr"
			case "DifferentXRTypes":
				compositionKey = "production/my-database/platform.example.com/v1beta2/XDatabase/my-db-xr"
			default:
				compositionKey = "default/test-claim/example.io/v1alpha1/XExample/test-xr"
			}
			storeData, _ := mockStore.Load(context.Background(), "default", compositionKey)

			for resourceKey, expectedValue := range tc.want.storeContains {
				if actualValue, exists := storeData[resourceKey]; !exists {
					t.Errorf("%s\nExpected store to contain %s, but it was missing", tc.reason, resourceKey)
				} else if actualValue != expectedValue {
					t.Errorf("%s\nExpected store[%s] = %s, got %s", tc.reason, resourceKey, expectedValue, actualValue)
				}
			}

			for _, resourceKey := range tc.want.storeNotContains {
				if _, exists := storeData[resourceKey]; exists {
					t.Errorf("%s\nExpected store to NOT contain %s, but it was present", tc.reason, resourceKey)
				}
			}

			// Check desired resource annotations
			if rsp != nil && rsp.GetDesired() != nil {
				for resourceName, expectedAnnotations := range tc.want.desiredAnnotations {
					if resource, exists := rsp.GetDesired().GetResources()[resourceName]; exists {
						resourceStruct := resource.GetResource()
						if resourceStruct != nil && resourceStruct.GetFields() != nil {
							fields := resourceStruct.GetFields()
							if metadata := fields["metadata"]; metadata != nil {
								if metadataStruct := metadata.GetStructValue(); metadataStruct != nil {
									metadataFields := metadataStruct.GetFields()
									if annotations := metadataFields["annotations"]; annotations != nil {
										if annotationsStruct := annotations.GetStructValue(); annotationsStruct != nil {
											for expectedKey, expectedValue := range expectedAnnotations {
												if actualAnnotation := annotationsStruct.GetFields()[expectedKey]; actualAnnotation == nil {
													t.Errorf("%s\nExpected annotation %s on resource %s, but it was missing", tc.reason, expectedKey, resourceName)
												} else if expectedValue != "" && actualAnnotation.GetStringValue() != expectedValue {
													t.Errorf("%s\nExpected annotation %s=%s on resource %s, got %s", tc.reason, expectedKey, expectedValue, resourceName, actualAnnotation.GetStringValue())
												}
											}
										}
									}
								}
							}
						}
					}
				}

				for resourceName, notExpectedAnnotations := range tc.want.desiredNotAnnotations {
					if resource, exists := rsp.GetDesired().GetResources()[resourceName]; exists {
						resourceStruct := resource.GetResource()
						if resourceStruct != nil && resourceStruct.GetFields() != nil {
							fields := resourceStruct.GetFields()
							if metadata := fields["metadata"]; metadata != nil {
								if metadataStruct := metadata.GetStructValue(); metadataStruct != nil {
									metadataFields := metadataStruct.GetFields()
									if annotations := metadataFields["annotations"]; annotations != nil {
										if annotationsStruct := annotations.GetStructValue(); annotationsStruct != nil {
											for _, notExpectedKey := range notExpectedAnnotations {
												if annotationsStruct.GetFields()[notExpectedKey] != nil {
													t.Errorf("%s\nExpected annotation %s to NOT exist on resource %s, but it was present", tc.reason, notExpectedKey, resourceName)
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
		})
	}
}

func TestParseAWSINICredentials(t *testing.T) {
	tests := []struct {
		name        string
		iniContent  string
		expected    map[string]string
		expectError bool
	}{
		{
			name: "Valid AWS CLI INI format",
			iniContent: `[default]
aws_access_key_id=AKIAIOSFODNN7EXAMPLE
aws_secret_access_key=wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY`,
			expected: map[string]string{
				"accessKeyId":     "AKIAIOSFODNN7EXAMPLE",
				"secretAccessKey": "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
			},
			expectError: false,
		},
		{
			name: "Valid AWS CLI INI format with session token",
			iniContent: `[default]
aws_access_key_id=AKIAIOSFODNN7EXAMPLE
aws_secret_access_key=wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY
aws_session_token=SessionTokenExample`,
			expected: map[string]string{
				"accessKeyId":     "AKIAIOSFODNN7EXAMPLE",
				"secretAccessKey": "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
				"sessionToken":    "SessionTokenExample",
			},
			expectError: false,
		},
		{
			name: "INI format with comments and empty lines",
			iniContent: `# AWS Credentials
[default]
# Access key
aws_access_key_id=AKIAIOSFODNN7EXAMPLE

# Secret key  
aws_secret_access_key=wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY`,
			expected: map[string]string{
				"accessKeyId":     "AKIAIOSFODNN7EXAMPLE",
				"secretAccessKey": "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
			},
			expectError: false,
		},
		{
			name: "INI format with multiple sections (only default used)",
			iniContent: `[profile1]
aws_access_key_id=IGNORE_THIS
aws_secret_access_key=IGNORE_THIS

[default]
aws_access_key_id=AKIAIOSFODNN7EXAMPLE
aws_secret_access_key=wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY

[profile2]
aws_access_key_id=IGNORE_THIS_TOO`,
			expected: map[string]string{
				"accessKeyId":     "AKIAIOSFODNN7EXAMPLE",
				"secretAccessKey": "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
			},
			expectError: false,
		},
		{
			name: "Missing access key",
			iniContent: `[default]
aws_secret_access_key=wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY`,
			expected:    nil,
			expectError: true,
		},
		{
			name: "Missing secret key",
			iniContent: `[default]
aws_access_key_id=AKIAIOSFODNN7EXAMPLE`,
			expected:    nil,
			expectError: true,
		},
		{
			name: "No default section",
			iniContent: `[profile1]
aws_access_key_id=AKIAIOSFODNN7EXAMPLE
aws_secret_access_key=wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY`,
			expected:    nil,
			expectError: true,
		},
		{
			name:        "Empty content",
			iniContent:  "",
			expected:    nil,
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := parseAWSINICredentials(tt.iniContent)

			if tt.expectError {
				if err == nil {
					t.Errorf("Expected error but got nil")
				}
				return
			}

			if err != nil {
				t.Errorf("Unexpected error: %v", err)
				return
			}

			if diff := cmp.Diff(tt.expected, result); diff != "" {
				t.Errorf("Credential parsing mismatch (-expected +got):\n%s", diff)
			}
		})
	}
}

func TestGetAWSCredentials(t *testing.T) {
	tests := []struct {
		name        string
		credentials map[string][]byte
		expected    map[string]string
		expectError bool
	}{
		{
			name: "Valid JSON format",
			credentials: map[string][]byte{
				"credentials": []byte(`{"accessKeyId":"AKIAIOSFODNN7EXAMPLE","secretAccessKey":"wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"}`),
			},
			expected: map[string]string{
				"accessKeyId":     "AKIAIOSFODNN7EXAMPLE",
				"secretAccessKey": "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
			},
			expectError: false,
		},
		{
			name: "Valid AWS CLI INI format",
			credentials: map[string][]byte{
				"credentials": []byte(`[default]
aws_access_key_id=AKIAIOSFODNN7EXAMPLE
aws_secret_access_key=wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY`),
			},
			expected: map[string]string{
				"accessKeyId":     "AKIAIOSFODNN7EXAMPLE",
				"secretAccessKey": "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
			},
			expectError: false,
		},
		{
			name: "JSON format with session token",
			credentials: map[string][]byte{
				"credentials": []byte(`{"accessKeyId":"AKIAIOSFODNN7EXAMPLE","secretAccessKey":"wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY","sessionToken":"SessionTokenExample"}`),
			},
			expected: map[string]string{
				"accessKeyId":     "AKIAIOSFODNN7EXAMPLE",
				"secretAccessKey": "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
				"sessionToken":    "SessionTokenExample",
			},
			expectError: false,
		},
		{
			name:        "No aws-creds key",
			credentials: map[string][]byte{},
			expected:    nil,
			expectError: false,
		},
		{
			name: "Invalid format (neither JSON nor INI)",
			credentials: map[string][]byte{
				"credentials": []byte(`invalid format`),
			},
			expected:    nil,
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create a mock request with credentials
			req := &fnv1.RunFunctionRequest{
				Credentials: make(map[string]*fnv1.Credentials),
			}

			if len(tt.credentials) > 0 {
				req.Credentials["aws-creds"] = &fnv1.Credentials{
					Source: &fnv1.Credentials_CredentialData{
						CredentialData: &fnv1.CredentialData{
							Data: tt.credentials,
						},
					},
				}
			}

			result, err := getAWSCredentials(req)

			if tt.expectError {
				if err == nil {
					t.Errorf("Expected error but got nil")
				}
				return
			}

			if err != nil {
				t.Errorf("Unexpected error: %v", err)
				return
			}

			if diff := cmp.Diff(tt.expected, result); diff != "" {
				t.Errorf("Credential parsing mismatch (-expected +got):\n%s", diff)
			}
		})
	}
}
