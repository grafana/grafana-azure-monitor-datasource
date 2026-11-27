package healthmodels

import (
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

const DataPlaneAPIVersion = "2026-10-01-preview"

var entityPathPattern = regexp.MustCompile(`^/?api/entities/[a-zA-Z0-9][a-zA-Z0-9-]{1,258}[a-zA-Z0-9](/transitions)?$`)

func ValidateEndpoint(endpoint, configuredURL string) (*url.URL, error) {
	u, err := url.Parse(endpoint)
	allowed, allowedErr := url.Parse(configuredURL)
	if err != nil || allowedErr != nil || allowed.Hostname() == "" || u.Scheme != "https" ||
		u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" ||
		(u.Port() != "" && u.Port() != "443") || strings.Contains(u.EscapedPath(), "%") {
		return nil, fmt.Errorf("Health Models data-plane endpoint must be an HTTPS URL in the configured Cloud Health environment")
	}
	host, expected := strings.ToLower(u.Hostname()), strings.ToLower(allowed.Hostname())
	if host != expected && !strings.HasSuffix(host, "."+expected) {
		return nil, fmt.Errorf("Health Models data-plane endpoint is outside the configured Cloud Health environment")
	}
	for _, part := range strings.Split(u.Path, "/") {
		if part == "." || part == ".." {
			return nil, fmt.Errorf("Health Models data-plane endpoint must not contain relative path segments")
		}
	}
	return u, nil
}

func NewDataPlaneRequest(req *http.Request, configuredURL string) (*http.Request, error) {
	params := req.URL.Query()
	endpoint, err := ValidateEndpoint(params.Get("dataplaneUrl"), configuredURL)
	if err != nil {
		return nil, err
	}
	operation, err := url.Parse(params.Get("path"))
	if err != nil || operation.IsAbs() || operation.Host != "" || operation.User != nil ||
		operation.Fragment != "" || !entityPathPattern.MatchString(operation.EscapedPath()) {
		return nil, fmt.Errorf("path must be a Health Models entity snapshot or transitions operation")
	}
	query := operation.Query()
	for key, values := range params {
		if key != "dataplaneUrl" && key != "path" {
			query[key] = values
		}
	}
	if query.Get("api-version") != DataPlaneAPIVersion {
		return nil, fmt.Errorf("Health Models data-plane requests require api-version=%s", DataPlaneAPIVersion)
	}
	target := endpoint.JoinPath(operation.Path)
	target.RawQuery = query.Encode()
	upstream, err := http.NewRequestWithContext(req.Context(), http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, err
	}
	upstream.Header.Set("Accept", "application/json")
	return upstream, nil
}
