package loganalytics

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"

	"github.com/grafana/grafana-plugin-sdk-go/backend"

	"github.com/grafana/grafana-azure-monitor-datasource/pkg/azuremonitor/types"
)

// componentsQuery lists Application Insights components with their app IDs. It has no subscription
// filter, so Resource Graph returns only resources the caller can read. It returns one row more than
// exemplarComponentCap so truncation can be reported, which keeps the list to one page.
var componentsQuery = fmt.Sprintf(
	"resources | where type =~ 'microsoft.insights/components' | project id, subscriptionId, appId = tostring(properties.AppId) | order by subscriptionId asc, id asc | take %d",
	exemplarComponentCap+1,
)

// appIDPattern limits app IDs to GUIDs before they are placed in KQL.
var appIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// appInsightsComponent is an Application Insights component the caller can read.
type appInsightsComponent struct {
	ID             string
	SubscriptionID string
	AppID          string
}

// listReadableAppInsightsComponents returns the Application Insights components the caller can
// read, sorted by subscription and then resource ID, and capped at exemplarComponentCap.
// truncated reports whether more components existed than the cap allows.
func listReadableAppInsightsComponents(ctx context.Context, dsInfo types.DatasourceInfo) ([]appInsightsComponent, bool, error) {
	svc, ok := dsInfo.Services["Azure Resource Graph"]
	if !ok || svc.HTTPClient == nil {
		return nil, false, fmt.Errorf("azure resource graph client is not configured")
	}
	endpoint := fmt.Sprintf("%s/providers/Microsoft.ResourceGraph/resources?api-version=%s",
		strings.TrimRight(svc.URL, "/"), argAPIVersion)

	var skipToken string
	var out []appInsightsComponent
	for {
		options := map[string]any{"resultFormat": "objectArray", "$top": exemplarComponentCap + 1}
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
		if res.StatusCode == http.StatusForbidden {
			// The caller cannot read any resources, so there is nothing to search. Return a short
			// message instead of the raw Azure error body.
			return nil, false, backend.DownstreamError(fmt.Errorf("no Application Insights components are readable by this identity (Azure Resource Graph returned 403 Forbidden)"))
		}
		if res.StatusCode/100 != 2 {
			return nil, false, backend.DownstreamError(fmt.Errorf("resource graph query failed: %s %s", res.Status, payload))
		}

		var parsed struct {
			Data []struct {
				ID             string `json:"id"`
				SubscriptionID string `json:"subscriptionId"`
				AppID          string `json:"appId"`
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
			out = append(out, appInsightsComponent{ID: row.ID, SubscriptionID: row.SubscriptionID, AppID: row.AppID})
		}
		// Stop once there are enough rows to tell that the list is truncated. The query is ordered,
		// so the first rows are the ones to keep.
		if parsed.SkipToken == "" || len(out) > exemplarComponentCap {
			break
		}
		skipToken = parsed.SkipToken
	}

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].SubscriptionID != out[j].SubscriptionID {
			return out[i].SubscriptionID < out[j].SubscriptionID
		}
		return out[i].ID < out[j].ID
	})
	truncated := len(out) > exemplarComponentCap
	if truncated {
		out = out[:exemplarComponentCap]
	}
	return out, truncated, nil
}
