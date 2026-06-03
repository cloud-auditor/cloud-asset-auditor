package oci

import (
	"testing"
	"time"

	"github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/containerengine"
	"github.com/oracle/oci-go-sdk/v65/containerinstances"
	occore "github.com/oracle/oci-go-sdk/v65/core"
	"github.com/oracle/oci-go-sdk/v65/database"
	"github.com/oracle/oci-go-sdk/v65/functions"
	"github.com/oracle/oci-go-sdk/v65/identity"
	"github.com/oracle/oci-go-sdk/v65/keymanagement"
	"github.com/oracle/oci-go-sdk/v65/objectstorage"
)

// testProvider is the shared receiver for the pure mapping tests below.
func testProvider() *Provider { return &Provider{tenancyOCID: "ocid1.tenancy.oc1..root"} }

func ptrInt64(i int64) *int64 { return &i }
func ptrIntVal(i int) *int    { return &i }

func TestVCNToAsset(t *testing.T) {
	created := time.Date(2025, 4, 1, 0, 0, 0, 0, time.UTC)
	a := testProvider().vcnToAsset(occore.Vcn{
		Id:             ptrString("ocid1.vcn.oc1..v1"),
		CompartmentId:  ptrString("ocid1.compartment.oc1..net"),
		DisplayName:    ptrString("prod-vcn"),
		LifecycleState: occore.VcnLifecycleStateEnum("AVAILABLE"),
		TimeCreated:    &common.SDKTime{Time: created},
		CidrBlocks:     []string{"10.0.0.0/16", "10.1.0.0/16"},
		FreeformTags:   map[string]string{"team": "net"},
	}, "me-jeddah-1")

	if a.Type != "oci.vcn" || a.ID != "ocid1.vcn.oc1..v1" || a.Name != "prod-vcn" {
		t.Errorf("vcn core fields: %+v", a)
	}
	if a.Status != "AVAILABLE" {
		t.Errorf("Status = %q", a.Status)
	}
	if a.CreatedAt == nil || !a.CreatedAt.Equal(created) {
		t.Errorf("CreatedAt = %v", a.CreatedAt)
	}
	if a.Tags["cidr_blocks"] != "10.0.0.0/16,10.1.0.0/16" {
		t.Errorf("Tags[cidr_blocks] = %q", a.Tags["cidr_blocks"])
	}
	if a.Tags["team"] != "net" {
		t.Error("freeform tag missing")
	}
}

func TestVCNToAsset_FallsBackToDeprecatedCidrBlock(t *testing.T) {
	a := testProvider().vcnToAsset(occore.Vcn{
		Id:        ptrString("ocid1.vcn.oc1..v2"),
		CidrBlock: ptrString("192.168.0.0/24"),
	}, "me-jeddah-1")
	if a.Tags["cidr_blocks"] != "192.168.0.0/24" {
		t.Errorf("expected fallback to single CidrBlock, got %q", a.Tags["cidr_blocks"])
	}
}

func TestSubnetToAsset(t *testing.T) {
	a := testProvider().subnetToAsset(occore.Subnet{
		Id:             ptrString("ocid1.subnet.oc1..s1"),
		CompartmentId:  ptrString("ocid1.compartment.oc1..net"),
		DisplayName:    ptrString("prod-subnet"),
		VcnId:          ptrString("ocid1.vcn.oc1..v1"),
		CidrBlock:      ptrString("10.0.1.0/24"),
		LifecycleState: occore.SubnetLifecycleStateEnum("AVAILABLE"),
	}, "me-jeddah-1")

	if a.Type != "oci.subnet" || a.ID != "ocid1.subnet.oc1..s1" {
		t.Errorf("subnet core fields: %+v", a)
	}
	if a.Tags["vcn_id"] != "ocid1.vcn.oc1..v1" || a.Tags["cidr_block"] != "10.0.1.0/24" {
		t.Errorf("subnet tags: %v", a.Tags)
	}
}

func TestBlockVolumeToAsset(t *testing.T) {
	a := testProvider().blockVolumeToAsset(occore.Volume{
		Id:                 ptrString("ocid1.volume.oc1..bv1"),
		CompartmentId:      ptrString("ocid1.compartment.oc1..app"),
		DisplayName:        ptrString("data-vol"),
		AvailabilityDomain: ptrString("xvlu:ME-JEDDAH-1-AD-1"),
		SizeInGBs:          ptrInt64(200),
		LifecycleState:     occore.VolumeLifecycleStateEnum("AVAILABLE"),
	}, "me-jeddah-1")

	if a.Type != "oci.block_volume" || a.Name != "data-vol" {
		t.Errorf("block volume core fields: %+v", a)
	}
	if a.Tags["size_gb"] != "200" {
		t.Errorf("Tags[size_gb] = %q", a.Tags["size_gb"])
	}
	if a.Tags["availability_domain"] != "xvlu:ME-JEDDAH-1-AD-1" {
		t.Errorf("Tags[availability_domain] = %q", a.Tags["availability_domain"])
	}
}

func TestBootVolumeToAsset(t *testing.T) {
	a := testProvider().bootVolumeToAsset(occore.BootVolume{
		Id:             ptrString("ocid1.bootvolume.oc1..boot1"),
		CompartmentId:  ptrString("ocid1.compartment.oc1..app"),
		DisplayName:    ptrString("node-boot"),
		SizeInGBs:      ptrInt64(50),
		ImageId:        ptrString("ocid1.image.oc1..img1"),
		LifecycleState: occore.BootVolumeLifecycleStateEnum("AVAILABLE"),
	}, "me-jeddah-1")

	if a.Type != "oci.boot_volume" {
		t.Errorf("Type = %q", a.Type)
	}
	if a.Tags["size_gb"] != "50" || a.Tags["image_id"] != "ocid1.image.oc1..img1" {
		t.Errorf("boot volume tags: %v", a.Tags)
	}
}

func TestBucketToAsset(t *testing.T) {
	created := time.Date(2025, 5, 6, 7, 8, 9, 0, time.UTC)
	a := testProvider().bucketToAsset(objectstorage.BucketSummary{
		Name:          ptrString("backups"),
		CompartmentId: ptrString("ocid1.compartment.oc1..app"),
		TimeCreated:   &common.SDKTime{Time: created},
		FreeformTags:  map[string]string{"tier": "cold"},
	}, "axabcdef", "me-jeddah-1")

	if a.Type != "oci.object_storage.bucket" {
		t.Errorf("Type = %q", a.Type)
	}
	// Buckets have no OCID in the list response — name doubles as the ID.
	if a.ID != "backups" || a.Name != "backups" {
		t.Errorf("bucket ID/Name = %q/%q", a.ID, a.Name)
	}
	// Buckets have no lifecycle state.
	if a.Status != "" {
		t.Errorf("Status = %q, want empty", a.Status)
	}
	if a.Tags["namespace"] != "axabcdef" || a.Tags["tier"] != "cold" {
		t.Errorf("bucket tags: %v", a.Tags)
	}
	if a.CreatedAt == nil || !a.CreatedAt.Equal(created) {
		t.Errorf("CreatedAt = %v", a.CreatedAt)
	}
}

func TestAutonomousDatabaseToAsset(t *testing.T) {
	a := testProvider().autonomousDatabaseToAsset(database.AutonomousDatabaseSummary{
		Id:             ptrString("ocid1.autonomousdatabase.oc1..adb1"),
		CompartmentId:  ptrString("ocid1.compartment.oc1..data"),
		DisplayName:    ptrString("analytics-adb"),
		DbName:         ptrString("ANALYTICS"),
		DbVersion:      ptrString("19c"),
		LifecycleState: database.AutonomousDatabaseSummaryLifecycleStateEnum("AVAILABLE"),
	}, "me-jeddah-1")

	if a.Type != "oci.autonomous_database" || a.Name != "analytics-adb" {
		t.Errorf("adb core fields: %+v", a)
	}
	if a.Tags["db_name"] != "ANALYTICS" || a.Tags["db_version"] != "19c" {
		t.Errorf("adb tags: %v", a.Tags)
	}
}

func TestDBSystemToAsset(t *testing.T) {
	a := testProvider().dbSystemToAsset(database.DbSystemSummary{
		Id:              ptrString("ocid1.dbsystem.oc1..dbs1"),
		CompartmentId:   ptrString("ocid1.compartment.oc1..data"),
		DisplayName:     ptrString("oltp-db"),
		Shape:           ptrString("VM.Standard2.4"),
		DatabaseEdition: database.DbSystemSummaryDatabaseEditionEnum("ENTERPRISE_EDITION"),
		LifecycleState:  database.DbSystemSummaryLifecycleStateEnum("AVAILABLE"),
	}, "me-jeddah-1")

	if a.Type != "oci.db_system" {
		t.Errorf("Type = %q", a.Type)
	}
	if a.Tags["shape"] != "VM.Standard2.4" || a.Tags["database_edition"] != "ENTERPRISE_EDITION" {
		t.Errorf("db system tags: %v", a.Tags)
	}
}

func TestFunctionsApplicationToAsset(t *testing.T) {
	a := testProvider().functionsApplicationToAsset(functions.ApplicationSummary{
		Id:             ptrString("ocid1.fnapp.oc1..app1"),
		CompartmentId:  ptrString("ocid1.compartment.oc1..app"),
		DisplayName:    ptrString("payments-app"),
		SubnetIds:      []string{"ocid1.subnet.oc1..s1", "ocid1.subnet.oc1..s2"},
		LifecycleState: functions.ApplicationLifecycleStateEnum("ACTIVE"),
	}, "me-jeddah-1")

	if a.Type != "oci.functions.application" {
		t.Errorf("Type = %q", a.Type)
	}
	if a.Tags["subnet_ids"] != "ocid1.subnet.oc1..s1,ocid1.subnet.oc1..s2" {
		t.Errorf("Tags[subnet_ids] = %q", a.Tags["subnet_ids"])
	}
}

func TestFunctionToAsset(t *testing.T) {
	a := testProvider().functionToAsset(functions.FunctionSummary{
		Id:             ptrString("ocid1.fnfunc.oc1..fn1"),
		CompartmentId:  ptrString("ocid1.compartment.oc1..app"),
		ApplicationId:  ptrString("ocid1.fnapp.oc1..app1"),
		DisplayName:    ptrString("charge-card"),
		Image:          ptrString("iad.ocir.io/ns/charge:1.0"),
		MemoryInMBs:    ptrInt64(256),
		LifecycleState: functions.FunctionLifecycleStateEnum("ACTIVE"),
	}, "me-jeddah-1")

	if a.Type != "oci.functions.function" || a.Name != "charge-card" {
		t.Errorf("function core fields: %+v", a)
	}
	if a.Tags["application_id"] != "ocid1.fnapp.oc1..app1" || a.Tags["memory_mb"] != "256" {
		t.Errorf("function tags: %v", a.Tags)
	}
}

func TestContainerInstanceToAsset(t *testing.T) {
	a := testProvider().containerInstanceToAsset(containerinstances.ContainerInstanceSummary{
		Id:             ptrString("ocid1.containerinstance.oc1..ci1"),
		CompartmentId:  ptrString("ocid1.compartment.oc1..app"),
		DisplayName:    ptrString("batch-ci"),
		Shape:          ptrString("CI.Standard.E4.Flex"),
		ContainerCount: ptrIntVal(3),
		LifecycleState: containerinstances.ContainerInstanceLifecycleStateEnum("ACTIVE"),
	}, "me-jeddah-1")

	if a.Type != "oci.container_instance" {
		t.Errorf("Type = %q", a.Type)
	}
	if a.Tags["shape"] != "CI.Standard.E4.Flex" || a.Tags["container_count"] != "3" {
		t.Errorf("container instance tags: %v", a.Tags)
	}
}

func TestOKEClusterToAsset(t *testing.T) {
	created := time.Date(2025, 5, 31, 21, 0, 0, 0, time.UTC)
	a := testProvider().okeClusterToAsset(containerengine.ClusterSummary{
		Id:                ptrString("ocid1.cluster.oc1..c1"),
		Name:              ptrString("prod-oke"), // ClusterSummary uses Name, not DisplayName
		CompartmentId:     ptrString("ocid1.compartment.oc1..app"),
		VcnId:             ptrString("ocid1.vcn.oc1..v1"),
		KubernetesVersion: ptrString("v1.30.1"),
		LifecycleState:    containerengine.ClusterLifecycleStateEnum("ACTIVE"),
		FreeformTags:      map[string]string{"team": "platform"},
		Metadata:          &containerengine.ClusterMetadata{TimeCreated: &common.SDKTime{Time: created}},
	}, "me-jeddah-1")

	if a.Type != "oci.oke.cluster" || a.Name != "prod-oke" {
		t.Errorf("oke core fields: %+v", a)
	}
	// Creation time comes from nested Metadata, not a top-level field.
	if a.CreatedAt == nil || !a.CreatedAt.Equal(created) {
		t.Errorf("CreatedAt = %v, want %v", a.CreatedAt, created)
	}
	if a.Tags["kubernetes_version"] != "v1.30.1" || a.Tags["vcn_id"] != "ocid1.vcn.oc1..v1" {
		t.Errorf("oke tags: %v", a.Tags)
	}
}

func TestOKEClusterToAsset_NilMetadata(t *testing.T) {
	a := testProvider().okeClusterToAsset(containerengine.ClusterSummary{
		Id:   ptrString("ocid1.cluster.oc1..c2"),
		Name: ptrString("no-meta"),
	}, "me-jeddah-1")
	if a.CreatedAt != nil {
		t.Errorf("CreatedAt should be nil when Metadata is absent, got %v", a.CreatedAt)
	}
}

func TestVaultToAsset(t *testing.T) {
	a := testProvider().vaultToAsset(keymanagement.VaultSummary{
		Id:                 ptrString("ocid1.vault.oc1..vlt1"),
		CompartmentId:      ptrString("ocid1.compartment.oc1..sec"),
		DisplayName:        ptrString("app-vault"),
		VaultType:          keymanagement.VaultSummaryVaultTypeEnum("DEFAULT"),
		ManagementEndpoint: ptrString("https://mgmt.kms.example"),
		LifecycleState:     keymanagement.VaultSummaryLifecycleStateEnum("ACTIVE"),
	}, "me-jeddah-1")

	if a.Type != "oci.vault" || a.Name != "app-vault" {
		t.Errorf("vault core fields: %+v", a)
	}
	if a.Tags["vault_type"] != "DEFAULT" {
		t.Errorf("Tags[vault_type] = %q", a.Tags["vault_type"])
	}
}

func TestPolicyToAsset(t *testing.T) {
	a := testProvider().policyToAsset(identity.Policy{
		Id:             ptrString("ocid1.policy.oc1..pol1"),
		CompartmentId:  ptrString("ocid1.compartment.oc1..app"),
		Name:           ptrString("app-admins"), // identity types use Name
		Statements:     []string{"Allow group Admins to manage all-resources in tenancy", "Allow group Devs to read instances in compartment app"},
		LifecycleState: identity.PolicyLifecycleStateActive,
	})

	if a.Type != "oci.iam.policy" || a.Name != "app-admins" {
		t.Errorf("policy core fields: %+v", a)
	}
	// Policies are tenancy-global assets, no region.
	if a.Region != "" {
		t.Errorf("Region = %q, want empty", a.Region)
	}
	if a.Tags["statement_count"] != "2" {
		t.Errorf("Tags[statement_count] = %q", a.Tags["statement_count"])
	}
}

func TestUserToAsset(t *testing.T) {
	a := testProvider().userToAsset(identity.User{
		Id:             ptrString("ocid1.user.oc1..u1"),
		CompartmentId:  ptrString("ocid1.tenancy.oc1..root"),
		Name:           ptrString("alice@example.com"),
		Email:          ptrString("alice@example.com"),
		LifecycleState: identity.UserLifecycleStateActive,
	})

	if a.Type != "oci.iam.user" || a.Name != "alice@example.com" {
		t.Errorf("user core fields: %+v", a)
	}
	if a.Tags["email"] != "alice@example.com" {
		t.Errorf("Tags[email] = %q", a.Tags["email"])
	}
}

func TestGroupToAsset(t *testing.T) {
	a := testProvider().groupToAsset(identity.Group{
		Id:             ptrString("ocid1.group.oc1..g1"),
		CompartmentId:  ptrString("ocid1.tenancy.oc1..root"),
		Name:           ptrString("Admins"),
		LifecycleState: identity.GroupLifecycleStateActive,
	})

	if a.Type != "oci.iam.group" || a.Name != "Admins" {
		t.Errorf("group core fields: %+v", a)
	}
}

func TestDynamicGroupToAsset(t *testing.T) {
	a := testProvider().dynamicGroupToAsset(identity.DynamicGroup{
		Id:             ptrString("ocid1.dynamicgroup.oc1..dg1"),
		CompartmentId:  ptrString("ocid1.tenancy.oc1..root"),
		Name:           ptrString("oke-nodes"),
		MatchingRule:   ptrString("ALL {instance.compartment.id = 'ocid1.compartment.oc1..app'}"),
		LifecycleState: identity.DynamicGroupLifecycleStateActive,
	})

	if a.Type != "oci.iam.dynamic_group" || a.Name != "oke-nodes" {
		t.Errorf("dynamic group core fields: %+v", a)
	}
	if a.Tags["matching_rule"] == "" {
		t.Error("Tags[matching_rule] should be populated")
	}
}
