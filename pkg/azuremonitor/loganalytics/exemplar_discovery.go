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
	"time"

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

// componentCheck is the outcome of checking one component for an operation ID.
type componentCheck int

const (
	// componentAbsent means the component answered and has no telemetry for the operation.
	componentAbsent componentCheck = iota
	// componentHasTrace means the component has at least one row for the operation.
	componentHasTrace
	// componentSkipped means the component could not be checked, for example because of a 403.
	componentSkipped
)

// operationIDPattern limits operation IDs to hex before they are placed in KQL.
var operationIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{1,64}$`)

// componentHasOperation runs a one-row KQL query against one Application Insights component.
// The error return is non-nil only for requests that cannot be built. Any HTTP failure from the
// component is reported as componentSkipped so one unreadable component does not fail the lookup.
func componentHasOperation(ctx context.Context, dsInfo types.DatasourceInfo, componentID, operationID string, timeRange backend.TimeRange) (componentCheck, error) {
	if !operationIDPattern.MatchString(operationID) {
		return componentAbsent, fmt.Errorf("invalid operation id for trace exemplar lookup")
	}
	svc, ok := dsInfo.Services["Azure Log Analytics"]
	if !ok || svc.HTTPClient == nil {
		return componentAbsent, fmt.Errorf("azure log analytics client is not configured")
	}

	query := fmt.Sprintf(
		"union requests, dependencies, traces, exceptions, pageViews, customEvents, availabilityResults | where operation_Id == '%s' | take 1",
		operationID,
	)
	raw, err := json.Marshal(map[string]string{
		"query":    query,
		"timespan": timeRange.From.Format(time.RFC3339) + "/" + timeRange.To.Format(time.RFC3339),
	})
	if err != nil {
		return componentAbsent, err
	}

	endpoint := strings.TrimRight(svc.URL, "/") + "/" + getApiURL(componentID, true, false)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return componentAbsent, err
	}
	req.Header.Set("Content-Type", "application/json")

	res, err := svc.HTTPClient.Do(req)
	if err != nil {
		return componentSkipped, nil
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode/100 != 2 {
		return componentSkipped, nil
	}

	var parsed AzureLogAnalyticsResponse
	if err := json.NewDecoder(res.Body).Decode(&parsed); err != nil {
		return componentSkipped, nil
	}
	table, err := parsed.GetPrimaryResultTable()
	if err != nil || len(table.Rows) == 0 {
		return componentAbsent, nil
	}
	return componentHasTrace, nil
}
