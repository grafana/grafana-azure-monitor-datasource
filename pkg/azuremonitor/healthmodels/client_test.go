package healthmodels

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const testAudience = "https://data.cloudhealth.azure.com"
const testEndpoint = "https://model-1.data.cloudhealth.azure.com"

func TestValidateEndpoint(t *testing.T) {
	for _, endpoint := range []string{testAudience, testEndpoint, testEndpoint + "/models/model-1", testEndpoint + ":443"} {
		_, err := ValidateEndpoint(endpoint, testAudience)
		require.NoError(t, err, endpoint)
	}
	for _, endpoint := range []string{
		"", "http://model-1.data.cloudhealth.azure.com", "https://data.cloudhealth.azure.com.attacker.test",
		testEndpoint + ":8443", testEndpoint + "?token=value", testEndpoint + "#fragment",
		"https://user@model-1.data.cloudhealth.azure.com", testEndpoint + "/../other", testEndpoint + "/%2e%2e/other",
	} {
		_, err := ValidateEndpoint(endpoint, testAudience)
		require.Error(t, err, endpoint)
	}
	_, err := ValidateEndpoint("https://model.custom.example", "https://custom.example")
	require.NoError(t, err, "customized environments use their configured endpoint")
}

func TestNewDataPlaneRequest(t *testing.T) {
	type contextKey struct{}
	for _, operation := range []string{"api/entities/child-entity", "/api/entities/child-entity/transitions"} {
		t.Run(operation, func(t *testing.T) {
			params := url.Values{
				"dataplaneUrl": {testEndpoint + "/models/model-1"},
				"path":         {operation + "?api-version=" + DataPlaneAPIVersion + "&nextMarker=page-2"},
				"timestamp":    {"2026-11-24T23:00:00Z"},
			}
			req := httptest.NewRequest(http.MethodGet, "/healthmodels?"+params.Encode(), nil)
			req = req.WithContext(context.WithValue(req.Context(), contextKey{}, "resource-context"))
			req.Header.Set("Authorization", "browser-token")
			req.Header.Set("Cookie", "browser-cookie")
			upstream, err := NewDataPlaneRequest(req, testAudience)
			require.NoError(t, err)
			require.Equal(t, "resource-context", upstream.Context().Value(contextKey{}))
			require.Equal(t, "model-1.data.cloudhealth.azure.com", upstream.URL.Host)
			require.Equal(t, "/models/model-1/"+strings.TrimLeft(operation, "/"), upstream.URL.Path)
			require.Equal(t, DataPlaneAPIVersion, upstream.URL.Query().Get("api-version"))
			require.Equal(t, "page-2", upstream.URL.Query().Get("nextMarker"))
			require.Equal(t, "2026-11-24T23:00:00Z", upstream.URL.Query().Get("timestamp"))
			require.Empty(t, upstream.Header.Get("Authorization"))
			require.Empty(t, upstream.Header.Get("Cookie"))
			require.Equal(t, "application/json", upstream.Header.Get("Accept"))
			require.Nil(t, upstream.Body)
		})
	}
}

func TestNewDataPlaneRequestRejectsUnsupportedOperations(t *testing.T) {
	for _, operation := range []string{
		"", "https://attacker.test/api/entities/child-entity", "//attacker.test/api/entities/child-entity",
		"api/entities/../other", "api/entities/%2e%2e", "api/entities/ab",
		"api/entities/child-entity/delete", "api/entities/child-entity#fragment",
		"api/entities/child-entity%2ftransitions", "api/entities/child-entity?api-version=other",
	} {
		t.Run(operation, func(t *testing.T) {
			params := url.Values{"dataplaneUrl": {testEndpoint}, "path": {operation}}
			req := httptest.NewRequest(http.MethodGet, "/healthmodels?"+params.Encode(), nil)
			_, err := NewDataPlaneRequest(req, testAudience)
			require.Error(t, err)
		})
	}
}
