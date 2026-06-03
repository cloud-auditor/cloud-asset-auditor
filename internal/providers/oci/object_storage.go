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

// objectStorageNamespace resolves the tenancy's Object Storage namespace
// exactly once. It's a tenancy-global value, so the result (or error) is
// cached and shared by every concurrent bucket collector.
func (p *Provider) objectStorageNamespace(ctx context.Context, client objectstorage.ObjectStorageClient) (string, error) {
	p.nsOnce.Do(func() {
		resp, err := client.GetNamespace(ctx, objectstorage.GetNamespaceRequest{})
		if err != nil {
			p.nsErr = err
			return
		}
		p.nsName = derefStr(resp.Value)
	})
	return p.nsName, p.nsErr
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
