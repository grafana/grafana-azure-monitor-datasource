package loganalytics

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/grafana/grafana-plugin-sdk-go/backend/log"

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
// Any failure that is specific to the component (HTTP error, transport error, bad response) returns
// componentSkipped with a non-nil error. The caller logs the error and moves on, so one unreadable
// component does not fail the lookup.
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

	// Query by resource ID. The name-based /v1/apps/{name}/query route returns 404 for these components.
	endpoint := strings.TrimRight(svc.URL, "/") + "/" + getApiURL(componentID, false, false)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return componentAbsent, err
	}
	req.Header.Set("Content-Type", "application/json")

	res, err := svc.HTTPClient.Do(req)
	if err != nil {
		return componentSkipped, fmt.Errorf("presence check request failed: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 300))
		return componentSkipped, fmt.Errorf("presence check returned %s: %s", res.Status, strings.TrimSpace(string(body)))
	}

	var parsed AzureLogAnalyticsResponse
	if err := json.NewDecoder(res.Body).Decode(&parsed); err != nil {
		return componentSkipped, fmt.Errorf("presence check response could not be decoded: %w", err)
	}
	table, err := parsed.GetPrimaryResultTable()
	if err != nil || len(table.Rows) == 0 {
		return componentAbsent, nil
	}
	return componentHasTrace, nil
}

// exemplarCheckParallelism bounds concurrent KQL checks per batch.
const exemplarCheckParallelism = 4

// discoverExemplarComponent returns the resource ID of the readable Application Insights component
// that holds operationID. Components are checked in batches of exemplarCheckParallelism, ordered by
// resource ID. If several components in one batch have the trace, the lowest ID wins.
func discoverExemplarComponent(ctx context.Context, dsInfo types.DatasourceInfo, operationID string, timeRange backend.TimeRange, logger log.Logger) (string, error) {
	if !operationIDPattern.MatchString(operationID) {
		return "", fmt.Errorf("invalid operation id for trace exemplar lookup")
	}

	components, truncated, err := listReadableAppInsightsComponents(ctx, dsInfo)
	if err != nil {
		return "", err
	}
	if len(components) == 0 {
		return "", backend.DownstreamError(fmt.Errorf("no readable Application Insights components"))
	}

	var checked []string
	var skipped []string
	for start := 0; start < len(components); start += exemplarCheckParallelism {
		end := start + exemplarCheckParallelism
		if end > len(components) {
			end = len(components)
		}
		batch := components[start:end]
		results := make([]componentCheck, len(batch))

		var wg sync.WaitGroup
		for i, component := range batch {
			wg.Add(1)
			go func(i int, component appInsightsComponent) {
				defer wg.Done()
				status, checkErr := componentHasOperation(ctx, dsInfo, component.ID, operationID, timeRange)
				if checkErr != nil {
					logger.Warn("exemplar component check failed", "component", component.ID, "err", checkErr)
					results[i] = componentSkipped
					return
				}
				results[i] = status
			}(i, component)
		}
		wg.Wait()

		var match string
		for i, status := range results {
			name := path.Base(batch[i].ID)
			checked = append(checked, name)
			switch status {
			case componentHasTrace:
				if match == "" || batch[i].ID < match {
					match = batch[i].ID
				}
			case componentSkipped:
				skipped = append(skipped, fmt.Sprintf("%s (subscription %s)", name, batch[i].SubscriptionID))
			}
		}
		if match != "" {
			return match, nil
		}
	}

	msg := fmt.Sprintf(
		"trace %s was not found in %d Application Insights components this identity can read (checked: %s; skipped %d",
		operationID, len(checked), strings.Join(checked, ", "), len(skipped),
	)
	if len(skipped) > 0 {
		msg += ": " + strings.Join(skipped, ", ")
	}
	msg += ")"
	if truncated {
		msg += fmt.Sprintf("; only the first %d components ordered by resource id were checked", exemplarComponentCap)
	}
	return "", backend.DownstreamError(fmt.Errorf("%s", msg))
}
