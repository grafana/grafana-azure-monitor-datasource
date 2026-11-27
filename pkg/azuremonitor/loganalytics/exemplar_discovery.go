package loganalytics

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path"
	"sort"
	"strings"

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
	// default subscription. Defaults to off, where exemplar queries use the default subscription.
	exemplarComponentDiscoveryFlag = "grafana-azure-monitor-datasource.exemplarComponentDiscovery"
	// maxSkipReasonLength keeps one skipped component's reason short in the error message.
	maxSkipReasonLength = 160
)

// exemplarComponentDiscoveryEnabled evaluates the discovery flag at call time. Flag values can
// change during a session, so the result is not cached.
func exemplarComponentDiscoveryEnabled(ctx context.Context) bool {
	return openfeature.NewDefaultClient().Boolean(ctx, exemplarComponentDiscoveryFlag, false, openfeature.TransactionContext(ctx))
}

// exemplarUnionBatchSize is how many components one union query spans, derived from the pair limit.
// 50 components x 2 tables is 100 pairs, the same count as the measured 25 x 4 success. A batch the
// service rejects is split in half, so a batch that is too large costs extra queries, not a wrong answer.
var exemplarUnionBatchSize = exemplarUnionPairLimit / len(unionTables)

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
