package oci

import (
	"errors"
	"net/http"
	"os"
	"time"

	"github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/common/auth"
)

// resolveAuth walks the chain from init-plan.md §3:
//  1. Instance principal — works on OCI Compute instances (probes the IMDS
//     endpoint with a short timeout so off-OCI machines pay almost no cost).
//  2. Resource principal — works in OKE workloads; we only attempt it when
//     OCI_RESOURCE_PRINCIPAL_VERSION is set, since the SDK call is slow
//     otherwise.
//  3. Config file — ~/.oci/config, using `profile` or DEFAULT.
//  4. Environment variables — OCI_TENANCY_OCID, OCI_USER_OCID, etc.
//
// The first step that yields a working TenancyOCID wins.
func resolveAuth(profile string) (common.ConfigurationProvider, error) {
	var firstErr error

	if onOCIInstance() {
		if p, err := auth.InstancePrincipalConfigurationProvider(); err == nil {
			return p, nil
		} else if firstErr == nil {
			firstErr = err
		}
	}

	if os.Getenv("OCI_RESOURCE_PRINCIPAL_VERSION") != "" {
		if p, err := auth.ResourcePrincipalConfigurationProvider(); err == nil {
			return p, nil
		} else if firstErr == nil {
			firstErr = err
		}
	}

	var fileProvider common.ConfigurationProvider
	if profile != "" {
		fileProvider = common.CustomProfileConfigProvider("", profile)
	} else {
		fileProvider = common.DefaultConfigProvider()
	}
	if _, err := fileProvider.TenancyOCID(); err == nil {
		return fileProvider, nil
	} else if firstErr == nil {
		firstErr = err
	}

	envProvider := common.ConfigurationProviderEnvironmentVariables("OCI", "")
	if _, err := envProvider.TenancyOCID(); err == nil {
		return envProvider, nil
	} else if firstErr == nil {
		firstErr = err
	}

	if firstErr == nil {
		firstErr = errors.New("no provider in chain produced credentials")
	}
	return nil, errors.Join(ErrNoCredentials, firstErr)
}

// onOCIInstance probes the IMDS endpoint with a short timeout. Returns true
// only on an actual OCI Compute instance — avoids the multi-second hang the
// SDK's instance-principal call would otherwise incur on a laptop.
func onOCIInstance() bool {
	client := &http.Client{Timeout: 250 * time.Millisecond}
	resp, err := client.Get("http://169.254.169.254/opc/v2/instance/")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}
