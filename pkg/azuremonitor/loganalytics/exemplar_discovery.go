package loganalytics

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/grafana/grafana-plugin-sdk-go/backend"

	"github.com/grafana/grafana-azure-monitor-datasource/pkg/azuremonitor/types"
)

const (
	// exemplarComponentCap bounds how many readable components an exemplar lookup checks.
	exemplarComponentCap = 25
	// argAPIVersion matches resourcegraph.ArgAPIVersion. It is repeated here because the
	// resourcegraph package imports this package.
	argAPIVersion = "2021-06-01-preview"
	// argMaxPages bounds how many Resource Graph pages are read when listing components.
	argMaxPages = 10
	// componentsQuery lists Application Insights components. It has no subscription filter,
	// so Resource Graph returns only resources the caller can read.
	componentsQuery = "resources | where type =~ 'microsoft.insights/components' | project id, subscriptionId | order by id asc"
)

// appInsightsComponent is an Application Insights component the caller can read.
type appInsightsComponent struct {
	ID             string
	SubscriptionID string
}

// listReadableAppInsightsComponents returns the Application Insights components the caller can
// read, sorted by resource ID and capped at exemplarComponentCap. truncated reports whether more
// components existed than the cap allows.
func listReadableAppInsightsComponents(ctx context.Context, dsInfo types.DatasourceInfo) ([]appInsightsComponent, bool, error) {
	svc, ok := dsInfo.Services["Azure Resource Graph"]
	if !ok || svc.HTTPClient == nil {
		return nil, false, fmt.Errorf("azure resource graph client is not configured")
	}
	endpoint := fmt.Sprintf("%s/providers/Microsoft.ResourceGraph/resources?api-version=%s",
		strings.TrimRight(svc.URL, "/"), argAPIVersion)

	var skipToken string
	var out []appInsightsComponent
	for page := 0; page < argMaxPages; page++ {
		options := map[string]string{"resultFormat": "objectArray"}
		if skipToken != "" {
			options["$skipToken"] = skipToken
		}
		raw, err := json.Marshal(map[string]any{
			"query":   componentsQuery,
			"options": options,
		})
		if err != nil {
			return nil, false, err
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
		if err != nil {
			return nil, false, err
		}
		req.Header.Set("Content-Type", "application/json")

		res, err := svc.HTTPClient.Do(req)
		if err != nil {
			return nil, false, backend.DownstreamError(err)
		}
		payload, readErr := io.ReadAll(res.Body)
		_ = res.Body.Close()
		if readErr != nil {
			return nil, false, backend.DownstreamError(readErr)
		}
		if res.StatusCode/100 != 2 {
			return nil, false, backend.DownstreamError(fmt.Errorf("resource graph query failed: %s %s", res.Status, payload))
		}

		var parsed struct {
			Data []struct {
				ID             string `json:"id"`
				SubscriptionID string `json:"subscriptionId"`
			} `json:"data"`
			SkipToken string `json:"$skipToken"`
		}
		if err := json.Unmarshal(payload, &parsed); err != nil {
			return nil, false, err
		}
		for _, row := range parsed.Data {
			if row.ID == "" {
				continue
			}
			out = append(out, appInsightsComponent{ID: row.ID, SubscriptionID: row.SubscriptionID})
		}
		if parsed.SkipToken == "" {
			break
		}
		skipToken = parsed.SkipToken
	}

	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	truncated := len(out) > exemplarComponentCap
	if truncated {
		out = out[:exemplarComponentCap]
	}
	return out, truncated, nil
}
