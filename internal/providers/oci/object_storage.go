package oci

import (
	"context"
	"fmt"

	"github.com/oracle/oci-go-sdk/v65/objectstorage"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// collectObjectStorageBuckets enumerates buckets in one compartment from one
// region. Object Storage lists are scoped by the tenancy namespace, which is
// resolved once (see objectStorageNamespace) and reused across collectors.
func (p *Provider) collectObjectStorageBuckets(ctx context.Context, region, compartmentOCID string, out chan<- core.Asset) error {
	client, err := objectstorage.NewObjectStorageClientWithConfigurationProvider(p.auth)
	if err != nil {
		return fmt.Errorf("object storage client: %w", err)
	}
	client.SetRegion(region)
	p.retarget(&client.BaseClient)

	ns, err := p.objectStorageNamespace(ctx, client)
	if err != nil {
		return fmt.Errorf("object storage namespace: %w", err)
	}

	var page *string
	for {
		resp, err := client.ListBuckets(ctx, objectstorage.ListBucketsRequest{
			NamespaceName: &ns,
			CompartmentId: &compartmentOCID,
			Page:          page,
		})
		if err != nil {
			return fmt.Errorf("list buckets: %w", err)
		}
		for _, b := range resp.Items {
			if !sendAsset(ctx, out, p.bucketToAsset(b, ns, region)) {
				return nil
			}
		}
		if resp.OpcNextPage == nil || *resp.OpcNextPage == "" {
			return nil
		}
		page = resp.OpcNextPage
	}
}

// objectStorageNamespace resolves the tenancy's Object Storage namespace once
// and shares it with every concurrent bucket collector.
//
// Only success is cached. A failure is left uncached so the next collector
// retries: the namespace is required to list a bucket, so caching a transient
// error would drop Object Storage from the entire audit on one unlucky call.
// The mutex means a failing lookup is retried at most once per waiting
// collector rather than by all of them at once.
func (p *Provider) objectStorageNamespace(ctx context.Context, client objectstorage.ObjectStorageClient) (string, error) {
	p.nsMu.Lock()
	defer p.nsMu.Unlock()

	if p.nsName != "" {
		return p.nsName, nil
	}
	resp, err := client.GetNamespace(ctx, objectstorage.GetNamespaceRequest{})
	if err != nil {
		return "", err
	}
	p.nsName = derefStr(resp.Value)
	return p.nsName, nil
}

// bucketToAsset maps a bucket summary. Buckets have no OCID in the list
// response — their name is unique within the namespace, so it serves as the ID.
func (p *Provider) bucketToAsset(b objectstorage.BucketSummary, namespace, region string) core.Asset {
	return core.Asset{
		Provider:  providerName,
		AccountID: p.tenancyOCID,
		Region:    region,
		Type:      "oci.object_storage.bucket",
		ID:        derefStr(b.Name),
		Name:      derefStr(b.Name),
		CreatedAt: derefTime(b.TimeCreated),
		Tags: mergeFreeformTags(b.FreeformTags,
			[2]string{"compartment_id", derefStr(b.CompartmentId)},
			[2]string{"namespace", namespace},
		),
		Raw: p.rawOf(b),
	}
}
