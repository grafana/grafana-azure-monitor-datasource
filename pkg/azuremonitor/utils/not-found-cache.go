package utils

import (
	"fmt"
	"net/http"
	"regexp"
	"sync"
	"time"

	"github.com/grafana/grafana-azure-sdk-go/v2/azcredentials"
)

var (
	timeNow = time.Now

	notFoundTTL = 65 * time.Second

	missingResourceCodes = regexp.MustCompile(`"code"\s*:\s*"(ResourceNotFound|ResourceGroupNotFound|SubscriptionNotFound|ParentResourceNotFound|MissingSubscription|WorkspaceNotFoundError)"`)
)

// NotFoundCache returns the Azure error for a missing resource until notFoundTTL passes.
type NotFoundCache struct {
	entries sync.Map
}

type notFoundEntry struct {
	err       error
	expiresAt time.Time
}

// Get returns the held error for key, or nil when there is none.
func (c *NotFoundCache) Get(key string) error {
	v, ok := c.entries.Load(key)
	if !ok {
		return nil
	}
	entry := v.(notFoundEntry)
	if !timeNow().Before(entry.expiresAt) {
		c.entries.Delete(key)
		return nil
	}
	return fmt.Errorf("%w. Grafana holds this error for up to %d seconds and sends no request to Azure", entry.err, int(notFoundTTL.Seconds()))
}

// Hold keeps err for key when it is a 404 from Azure for a resource that does not exist.
func (c *NotFoundCache) Hold(key string, statusCode int, err error) {
	if statusCode == http.StatusNotFound && missingResourceCodes.MatchString(err.Error()) {
		c.entries.Store(key, notFoundEntry{err: err, expiresAt: timeNow().Add(notFoundTTL)})
	}
}

// IsUserScopedAuth reports whether the data source sends requests with an identity that varies per Grafana user.
// Responses must not be cached across requests in these modes, or a user who lost access still gets a cached result.
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
