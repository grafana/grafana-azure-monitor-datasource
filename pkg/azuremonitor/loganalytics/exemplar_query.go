package loganalytics

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/backend"

	"github.com/grafana/grafana-azure-monitor-datasource/pkg/azuremonitor/types"
)

// componentCheck is the outcome of checking one component for an operation ID.
type componentCheck int

const (
	// componentAbsent means the component answered and has no telemetry for the operation.
	componentAbsent componentCheck = iota
	// componentHasTrace means the component has at least one row for the operation.
	componentHasTrace
	// componentSkipped means the component could not be checked, for example because of a 403.
	componentSkipped
	// componentFailed means the check hit a service-wide problem (authentication, throttling, or a
	// server error). The lookup must stop, because the other components would fail the same way.
	componentFailed
)

// unionTables are the Application Insights tables checked for an operation ID. Server and consumer
// spans become requests rows, and client, producer, and internal spans become dependencies rows.
// Exemplars point to spans, so these two tables cover exemplar traces. Traces, exceptions, page views,
// custom events, and availability results are not checked, so a trace whose only rows are in those
// tables would be missed.
var unionTables = []string{"requests", "dependencies"}

// operationIDPattern limits operation IDs to hex before they are placed in KQL.
var operationIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{1,64}$`)

// componentHasOperation runs a one-row KQL query against one Application Insights component.
// Failures specific to the component (403, 404, other HTTP errors) return componentSkipped with a
// non-nil error, so one unreadable component does not fail the lookup. Authentication failures
// (401), throttling (429), server errors (5xx), and transport or read failures return componentFailed,
// because they would affect every component.
func componentHasOperation(ctx context.Context, dsInfo types.DatasourceInfo, componentID, operationID string, timeRange backend.TimeRange) (componentCheck, error) {
	if !operationIDPattern.MatchString(operationID) {
		return componentAbsent, fmt.Errorf("invalid operation id for trace exemplar lookup")
	}
	svc, ok := dsInfo.Services["Azure Log Analytics"]
	if !ok || svc.HTTPClient == nil {
		return componentAbsent, fmt.Errorf("azure log analytics client is not configured")
	}

	query := fmt.Sprintf(
		"union requests, dependencies | where operation_Id == '%s' | take 1",
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
		return componentFailed, unreachableError(err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 300))
		err := fmt.Errorf("presence check returned %s: %s", res.Status, strings.TrimSpace(string(body)))
		if res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusTooManyRequests || res.StatusCode/100 == 5 {
			return componentFailed, err
		}
		return componentSkipped, err
	}

	var parsed AzureLogAnalyticsResponse
	if err := json.NewDecoder(res.Body).Decode(&parsed); err != nil {
		// A response cut off mid-body is a transport problem, not a rejection of this component.
		return componentFailed, fmt.Errorf("could not read Azure Log Analytics response: %w", err)
	}
	table, err := parsed.GetPrimaryResultTable()
	if err != nil || len(table.Rows) == 0 {
		return componentAbsent, nil
	}
	return componentHasTrace, nil
}

// buildUnionQuery returns one KQL query that checks every component's tables for operationID and
// reports the matching rows per resource. Callers must pass components with valid app IDs.
func buildUnionQuery(components []appInsightsComponent, operationID string) string {
	parts := make([]string, 0, len(components)*len(unionTables))
	for _, c := range components {
		for _, table := range unionTables {
			parts = append(parts, fmt.Sprintf("app('%s').%s", c.AppID, table))
		}
	}
	return fmt.Sprintf("union %s | where operation_Id == '%s' | summarize rows = count() by _ResourceId",
		strings.Join(parts, ", "), operationID)
}

// deniedAppIDs returns the app IDs named as InsufficientAccessToResource in a Log Analytics error.
func deniedAppIDs(payload []byte) []string {
	var body struct {
		Error struct {
			Details []struct {
				Code  string `json:"code"`
				Value string `json:"value"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(payload, &body); err != nil {
		return nil
	}
	var out []string
	for _, d := range body.Error.Details {
		if d.Code == insufficientAccessCode && d.Value != "" {
			out = append(out, d.Value)
		}
	}
	return out
}

// unionFailure is a union query that did not return usable results. status is the HTTP status, or
// 0 when no usable response was received.
type unionFailure struct {
	status int
	err    error
}

func (e *unionFailure) Error() string { return e.err.Error() }

func (e *unionFailure) Unwrap() error { return e.err }

// unionResult is the outcome of one union query. denied lists app IDs the caller cannot read.
type unionResult struct {
	matches []appInsightsComponent
	denied  []string
}

// queryUnion runs buildUnionQuery against the route of the first component in the batch.
func queryUnion(ctx context.Context, dsInfo types.DatasourceInfo, batch []appInsightsComponent, operationID string, timeRange backend.TimeRange) (unionResult, error) {
	svc, ok := dsInfo.Services["Azure Log Analytics"]
	if !ok || svc.HTTPClient == nil {
		return unionResult{}, &unionFailure{err: fmt.Errorf("azure log analytics client is not configured")}
	}
	raw, err := json.Marshal(map[string]string{
		"query":    buildUnionQuery(batch, operationID),
		"timespan": timeRange.From.Format(time.RFC3339) + "/" + timeRange.To.Format(time.RFC3339),
	})
	if err != nil {
		return unionResult{}, &unionFailure{err: err}
	}
	endpoint := strings.TrimRight(svc.URL, "/") + "/" + getApiURL(batch[0].ID, false, false)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return unionResult{}, &unionFailure{err: err}
	}
	req.Header.Set("Content-Type", "application/json")

	res, err := svc.HTTPClient.Do(req)
	if err != nil {
		return unionResult{}, &unionFailure{err: unreachableError(err)}
	}
	defer func() { _ = res.Body.Close() }()
	payload, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return unionResult{}, &unionFailure{err: fmt.Errorf("union presence check response could not be read: %w", err)}
	}
	if res.StatusCode/100 != 2 {
		if denied := deniedAppIDs(payload); len(denied) > 0 {
			return unionResult{denied: denied}, nil
		}
		body := payload
		if len(body) > 300 {
			body = body[:300]
		}
		return unionResult{}, &unionFailure{
			status: res.StatusCode,
			err:    fmt.Errorf("union presence check returned %s: %s", res.Status, strings.TrimSpace(string(body))),
		}
	}

	var parsed AzureLogAnalyticsResponse
	if err := json.Unmarshal(payload, &parsed); err != nil {
		return unionResult{}, &unionFailure{err: fmt.Errorf("union presence check response could not be decoded: %w", err)}
	}
	table, err := parsed.GetPrimaryResultTable()
	if err != nil {
		return unionResult{}, &unionFailure{err: err}
	}
	ridIndex := -1
	for i, col := range table.Columns {
		if col.Name == "_ResourceId" {
			ridIndex = i
		}
	}
	if ridIndex < 0 {
		return unionResult{}, &unionFailure{err: fmt.Errorf("union presence check response has no _ResourceId column")}
	}

	// Log Analytics returns _ResourceId in lowercase, so match case-insensitively.
	byID := make(map[string]appInsightsComponent, len(batch))
	for _, c := range batch {
		byID[strings.ToLower(c.ID)] = c
	}
	seen := make(map[string]bool)
	var result unionResult
	for _, row := range table.Rows {
		if ridIndex >= len(row) {
			continue
		}
		rid, ok := row[ridIndex].(string)
		if !ok {
			continue
		}
		c, ok := byID[strings.ToLower(rid)]
		if !ok || seen[c.ID] {
			continue
		}
		seen[c.ID] = true
		result.matches = append(result.matches, c)
	}
	return result, nil
}

// unreachableError reports that the request never got an HTTP response. The url.Error wrapper is
// dropped so the message names the cause, such as a dial error or cancellation, not the request URL.
func unreachableError(err error) error {
	// The HTTP client may wrap a url.Error around another url.Error, so unwrap until the cause.
	for {
		var urlErr *url.Error
		if !errors.As(err, &urlErr) {
			break
		}
		err = urlErr.Err
	}
	return fmt.Errorf("could not reach Azure Log Analytics: %w", err)
}
