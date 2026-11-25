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
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/grafana/grafana-plugin-sdk-go/backend/log"
	"github.com/open-feature/go-sdk/openfeature"

	"github.com/grafana/grafana-azure-monitor-datasource/pkg/azuremonitor/types"
)

const (
	// exemplarComponentCap bounds the total number of components one exemplar lookup checks.
	// It is a safety bound, not a tuned value.
	exemplarComponentCap = 500
	// exemplarUnionPairLimit is the most (component, table) pairs one union query may span. This is
	// measured, not documented: against the test tenant, 25 components x 4 tables and 14 x 7 succeed,
	// and 27 x 4 fails with BadArgumentError. The documented limit is 100 Application Insights
	// resources per cross-resource query, which batches of 50 components stay under. A batch
	// that is rejected with BadArgumentError is split in half.
	exemplarUnionPairLimit = 100
	// argAPIVersion matches resourcegraph.ArgAPIVersion. It is repeated here because the
	// resourcegraph package imports this package.
	argAPIVersion = "2021-06-01-preview"
	// insufficientAccessCode is the error code Log Analytics returns when a union names a
	// resource the caller cannot read.
	insufficientAccessCode = "InsufficientAccessToResource"
	// exemplarMaxQueries bounds the Log Analytics queries one lookup may send, including retries,
	// splits, and per-component checks. It is half of Azure Monitor's 200 queries per 30 seconds
	// per user. Checking exemplarComponentCap components needs 10 union queries when none fail.
	exemplarMaxQueries = 100
	// exemplarComponentDiscoveryFlag gates component discovery for trace exemplars that have no
	// default subscription. It is a datasource-scoped flag defined in grafana/deployment_tools
	// (ksonnet/environments/hosted-grafana/waves/feature-toggles/goff/datasources), not in
	// grafana/grafana's registry. While it is off, exemplar queries use the default subscription.
	exemplarComponentDiscoveryFlag = "grafana-azure-monitor-datasource.exemplarComponentDiscovery"
)

// exemplarComponentDiscoveryEnabled evaluates the discovery flag at call time. Flag values can
// change during a session, so the result is not cached.
func exemplarComponentDiscoveryEnabled(ctx context.Context) bool {
	return openfeature.NewDefaultClient().Boolean(ctx, exemplarComponentDiscoveryFlag, false, openfeature.TransactionContext(ctx))
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
	// componentFailed means the check hit a service-wide problem (authentication, throttling, or a
	// server error). The lookup must stop, because the other components would fail the same way.
	componentFailed
)

// componentsQuery lists Application Insights components with their app IDs. It has no subscription
// filter, so Resource Graph returns only resources the caller can read. It returns one row more than
// exemplarComponentCap so truncation can be reported, which keeps the list to one page.
var componentsQuery = fmt.Sprintf(
	"resources | where type =~ 'microsoft.insights/components' | project id, subscriptionId, appId = tostring(properties.AppId) | order by subscriptionId asc, id asc | take %d",
	exemplarComponentCap+1,
)

// exemplarUnionBatchSize is how many components one union query spans, derived from the pair limit.
// 50 components x 2 tables is 100 pairs, the same count as the measured 25 x 4 success. A batch the
// service rejects is split in half, so a batch that is too large costs extra queries, not a wrong answer.
var exemplarUnionBatchSize = exemplarUnionPairLimit / len(unionTables)

// unionTables are the Application Insights tables checked for an operation ID. Server and consumer
// spans become requests rows, and client, producer, and internal spans become dependencies rows.
// Exemplars point to spans, so these two tables cover exemplar traces. Traces, exceptions, page views,
// custom events, and availability results are not checked, so a trace whose only rows are in those
// tables would be missed.
var unionTables = []string{"requests", "dependencies"}

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

// queryBudget counts the Log Analytics queries one lookup may still send.
type queryBudget struct{ left int }

// take reserves one query, or returns an error when the lookup has used its budget.
func (b *queryBudget) take() error {
	if b.left <= 0 {
		return fmt.Errorf("reached the limit of %d Log Analytics queries for one lookup", exemplarMaxQueries)
	}
	b.left--
	return nil
}

// checkUnionBatch returns the components in batch that hold operationID. Components in union queries
// are checked first. Components without a valid app ID are checked on their own only when no union
// query finds the trace. An error means the lookup cannot continue, for example because of
// throttling, a server failure, or an exhausted budget.
func checkUnionBatch(ctx context.Context, dsInfo types.DatasourceInfo, batch []appInsightsComponent, operationID string, timeRange backend.TimeRange, logger log.Logger, skipped map[string]string, budget *queryBudget) ([]appInsightsComponent, error) {
	var unionable, perComponent []appInsightsComponent
	for _, c := range batch {
		if appIDPattern.MatchString(c.AppID) {
			unionable = append(unionable, c)
		} else {
			perComponent = append(perComponent, c)
		}
	}

	if len(unionable) > 0 {
		unionMatches, err := checkUnionGroup(ctx, dsInfo, unionable, operationID, timeRange, logger, skipped, budget)
		if err != nil {
			return nil, err
		}
		if len(unionMatches) > 0 {
			return unionMatches, nil
		}
	}
	if len(perComponent) == 0 {
		return nil, nil
	}
	logger.Warn("exemplar component has no valid app ID, checking it on its own", "components", len(perComponent))
	return checkOneByOne(ctx, dsInfo, perComponent, operationID, timeRange, logger, skipped, budget)
}

// checkUnionGroup checks components in union queries. Components the caller cannot read are recorded
// in skipped and the query is retried without them. A 403 or 404 means the component whose route
// carried the query cannot be read, so it is skipped. A query rejected with BadArgumentError is split
// in half, and a single component that is still rejected is checked on its own. Any other failure is
// returned, so the lookup stops instead of multiplying requests.
func checkUnionGroup(ctx context.Context, dsInfo types.DatasourceInfo, batch []appInsightsComponent, operationID string, timeRange backend.TimeRange, logger log.Logger, skipped map[string]string, budget *queryBudget) ([]appInsightsComponent, error) {
	remaining := batch
	// Each pass either returns or drops at least one component, so the loop ends within len(batch) passes.
	for len(remaining) > 0 {
		if err := budget.take(); err != nil {
			return nil, err
		}
		res, err := queryUnion(ctx, dsInfo, remaining, operationID, timeRange)
		if err != nil {
			var failure *unionFailure
			if !errors.As(err, &failure) {
				return nil, err
			}
			switch failure.status {
			case http.StatusForbidden, http.StatusNotFound:
				skipped[remaining[0].ID] = fmt.Sprintf("route component returned %d", failure.status)
				logger.Warn("exemplar component skipped, union route not readable", "component", remaining[0].ID, "status", failure.status)
				remaining = remaining[1:]
				continue
			case http.StatusBadRequest:
				if len(remaining) == 1 {
					logger.Warn("exemplar union rejected for one component, checking it on its own", "component", remaining[0].ID, "err", err)
					return checkOneByOne(ctx, dsInfo, remaining, operationID, timeRange, logger, skipped, budget)
				}
				logger.Warn("exemplar union rejected, splitting batch", "components", len(remaining), "err", err)
				mid := len(remaining) / 2
				first, err := checkUnionGroup(ctx, dsInfo, remaining[:mid], operationID, timeRange, logger, skipped, budget)
				if err != nil {
					return nil, err
				}
				second, err := checkUnionGroup(ctx, dsInfo, remaining[mid:], operationID, timeRange, logger, skipped, budget)
				if err != nil {
					return nil, err
				}
				return append(first, second...), nil
			default:
				return nil, err
			}
		}
		if len(res.denied) == 0 {
			return res.matches, nil
		}

		denied := make(map[string]bool, len(res.denied))
		for _, id := range res.denied {
			denied[strings.ToLower(id)] = true
		}
		var kept []appInsightsComponent
		for _, c := range remaining {
			if denied[strings.ToLower(c.AppID)] {
				skipped[c.ID] = "not readable by this identity"
				logger.Warn("exemplar component skipped, not readable", "component", c.ID)
				continue
			}
			kept = append(kept, c)
		}
		if len(kept) == len(remaining) {
			// The denied IDs matched nothing in this batch, so retrying would repeat the same request.
			return checkOneByOne(ctx, dsInfo, remaining, operationID, timeRange, logger, skipped, budget)
		}
		remaining = kept
	}
	if len(remaining) == 0 {
		return nil, nil
	}
	return checkOneByOne(ctx, dsInfo, remaining, operationID, timeRange, logger, skipped, budget)
}

// checkOneByOne checks each component with its own presence query. It is used for components that
// cannot go in a union. Components that fail are recorded in skipped. An exhausted budget stops the
// checks and is returned.
func checkOneByOne(ctx context.Context, dsInfo types.DatasourceInfo, components []appInsightsComponent, operationID string, timeRange backend.TimeRange, logger log.Logger, skipped map[string]string, budget *queryBudget) ([]appInsightsComponent, error) {
	var matches []appInsightsComponent
	for _, c := range components {
		if err := budget.take(); err != nil {
			return nil, err
		}
		status, err := componentHasOperation(ctx, dsInfo, c.ID, operationID, timeRange)
		if status == componentFailed {
			return nil, err
		}
		if err != nil {
			logger.Warn("exemplar component check failed", "component", c.ID, "err", err)
			skipped[c.ID] = err.Error()
			continue
		}
		if status == componentHasTrace {
			matches = append(matches, c)
		}
	}
	return matches, nil
}

// chunkComponents splits components into consecutive batches of at most size.
func chunkComponents(components []appInsightsComponent, size int) [][]appInsightsComponent {
	var out [][]appInsightsComponent
	for start := 0; start < len(components); start += size {
		end := start + size
		if end > len(components) {
			end = len(components)
		}
		out = append(out, components[start:end])
	}
	return out
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

// maxSkipReasonLength keeps one skipped component's reason short in the error message.
const maxSkipReasonLength = 160

// skippedList groups skipped components by reason, then by subscription, for example
// "not readable by this identity: subscription sub-a (app-a, app-b), subscription sub-c (app-c)".
// Groups, subscriptions, and names are sorted.
func skippedList(skipped map[string]string, subscriptionByID map[string]string) string {
	bySubscription := make(map[string]map[string][]string) // reason -> subscription -> app names
	for id, reason := range skipped {
		subscription := subscriptionByID[id]
		if bySubscription[reason] == nil {
			bySubscription[reason] = make(map[string][]string)
		}
		bySubscription[reason][subscription] = append(bySubscription[reason][subscription], path.Base(id))
	}
	groups := make([]string, 0, len(bySubscription))
	for reason, bySub := range bySubscription {
		subscriptions := make([]string, 0, len(bySub))
		for subscription := range bySub {
			subscriptions = append(subscriptions, subscription)
		}
		sort.Strings(subscriptions)
		parts := make([]string, 0, len(subscriptions))
		for _, subscription := range subscriptions {
			names := bySub[subscription]
			sort.Strings(names)
			parts = append(parts, fmt.Sprintf("subscription %s (%s)", subscription, strings.Join(names, ", ")))
		}
		if len(reason) > maxSkipReasonLength {
			reason = reason[:maxSkipReasonLength] + "..."
		}
		groups = append(groups, fmt.Sprintf("%s: %s", reason, strings.Join(parts, ", ")))
	}
	sort.Strings(groups)
	return strings.Join(groups, "; ")
}

// discoverExemplarComponent returns the resource ID of the readable Application Insights component
// that holds operationID. Components are checked in union batches, ordered by subscription and then
// resource ID. Batches run one at a time, so the lookup sends one Log Analytics query at a time.
// The lookup stops at the first batch that has the trace.
func discoverExemplarComponent(ctx context.Context, dsInfo types.DatasourceInfo, operationID string, timeRange backend.TimeRange, logger log.Logger) (string, error) {
	if operationID == "" {
		return "", backend.DownstreamError(fmt.Errorf("trace exemplar lookup needs an operation ID"))
	}
	if !operationIDPattern.MatchString(operationID) {
		return "", backend.DownstreamError(fmt.Errorf("operation ID for trace exemplar lookup must be 1 to 64 hex characters"))
	}

	components, truncated, err := listReadableAppInsightsComponents(ctx, dsInfo)
	if err != nil {
		return "", err
	}
	if len(components) == 0 {
		return "", backend.DownstreamError(fmt.Errorf("no readable Application Insights components"))
	}

	subscriptionByID := make(map[string]string, len(components))
	for _, c := range components {
		subscriptionByID[c.ID] = c.SubscriptionID
	}
	skipped := make(map[string]string)
	checked := 0
	budget := &queryBudget{left: exemplarMaxQueries}
	batches := chunkComponents(components, exemplarUnionBatchSize)
	for _, batch := range batches {
		batchSkipped := make(map[string]string)
		matches, err := checkUnionBatch(ctx, dsInfo, batch, operationID, timeRange, logger, batchSkipped, budget)
		if err != nil {
			return "", backend.DownstreamError(fmt.Errorf("trace exemplar lookup stopped: %w", err))
		}
		for id, reason := range batchSkipped {
			skipped[id] = reason
		}
		checked += len(batch) - len(batchSkipped)
		// If several union components in the batch have the trace, the lowest ID wins. Components
		// without an app ID are only checked when no union component has it.
		var match string
		for _, c := range matches {
			if match == "" || c.ID < match {
				match = c.ID
			}
		}
		if match != "" {
			return match, nil
		}
	}

	if checked == 0 {
		// Every component failed, so nothing was searched. Do not report this as "not found".
		return "", backend.DownstreamError(fmt.Errorf("could not check any of %d Application Insights components for trace %s: %s",
			len(components), operationID, skippedList(skipped, subscriptionByID)))
	}
	msg := fmt.Sprintf("trace %s was not found in %d Application Insights components this identity can read", operationID, checked)
	if len(skipped) > 0 {
		msg += fmt.Sprintf(" (skipped %d: %s)", len(skipped), skippedList(skipped, subscriptionByID))
	}
	if truncated {
		msg += fmt.Sprintf("; only the first %d components ordered by subscription and resource id were checked", exemplarComponentCap)
	}
	return "", backend.DownstreamError(errors.New(msg))
}
