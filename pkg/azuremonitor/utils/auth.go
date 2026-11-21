package utils

import "github.com/grafana/grafana-azure-sdk-go/v2/azcredentials"

// IsUserScopedAuth reports whether the data source sends requests with an identity that varies per Grafana user.
// Do not share caches or holds across requests in these modes: each user has their own access and their own Azure limits.
func IsUserScopedAuth(creds azcredentials.AzureCredentials) bool {
	if creds == nil {
		return false
	}
	switch creds.AzureAuthType() {
	case azcredentials.AzureAuthCurrentUserIdentity, azcredentials.AzureAuthClientSecretObo:
		return true
	}
	return false
}
