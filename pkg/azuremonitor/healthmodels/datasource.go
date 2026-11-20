package healthmodels

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/grafana/grafana-plugin-sdk-go/data"

	"github.com/grafana/grafana-azure-monitor-datasource/pkg/azuremonitor/kinds/dataquery"
	"github.com/grafana/grafana-azure-monitor-datasource/pkg/azuremonitor/types"
)

type Datasource struct{}

// Query extends the extracted query schema without hand-editing generated files.
type Query struct {
	dataquery.AzureHealthModelsQuery
	DataplaneURL   string `json:"dataplaneUrl,omitempty"`
	DataplaneModel *Model `json:"dataplaneModel,omitempty"`
}

func (d *Datasource) ResourceRequest(rw http.ResponseWriter, _ *http.Request, _ *http.Client) (http.ResponseWriter, error) {
	return rw, fmt.Errorf("Health Models data-plane reads are only available through query execution")
}

func (d *Datasource) ExecuteTimeSeriesQuery(ctx context.Context, queries []backend.DataQuery, info types.DatasourceInfo, client *http.Client, endpoint string, _ bool) (*backend.QueryDataResponse, error) {
	arm, ok := info.Services[types.RouteAzureMonitor]
	if !ok {
		return nil, fmt.Errorf("Health Models requires the Azure Monitor ARM service")
	}
	c := NewClient(arm.HTTPClient, client, arm.URL, endpoint)
	response := backend.NewQueryDataResponse()
	// Discovery is shared only within this execution, not across users or dashboard refreshes.
	discovered := map[string]*HealthModel{}
	for _, query := range queries {
		result, err := executeQuery(ctx, c, query, discovered)
		if err != nil {
			response.Responses[query.RefID] = backend.ErrorResponseWithErrorSource(backend.DownstreamError(err))
		} else {
			response.Responses[query.RefID] = result
		}
	}
	return response, nil
}

func executeQuery(ctx context.Context, client *Client, query backend.DataQuery, discovered map[string]*HealthModel) (backend.DataResponse, error) {
	var model struct {
		Subscription      string `json:"subscription"`
		AzureHealthModels *Query `json:"azureHealthModels"`
	}
	if err := json.Unmarshal(query.JSON, &model); err != nil {
		return backend.DataResponse{}, fmt.Errorf("decode Health Models query: %w", err)
	}
	healthQuery := model.AzureHealthModels
	if healthQuery == nil || healthQuery.HealthModelResourceId == nil || healthQuery.QueryType == nil {
		return backend.DataResponse{}, fmt.Errorf("select a Health Model and query type")
	}
	modelID := *healthQuery.HealthModelResourceId
	if err := ValidateModelID(modelID); err != nil {
		return backend.DataResponse{}, err
	}
	if model.Subscription != "" && !strings.EqualFold(strings.Split(modelID, "/")[2], model.Subscription) {
		return backend.DataResponse{}, fmt.Errorf("selected Health Model does not belong to the selected subscription; select the model again")
	}
	if query.TimeRange.To.IsZero() {
		return backend.DataResponse{}, fmt.Errorf("Health Models requires a query time range")
	}
	history := *healthQuery.QueryType == dataquery.AzureHealthModelsQueryTypeEntityHealthHistory
	if !history && *healthQuery.QueryType != dataquery.AzureHealthModelsQueryTypeEntityHealthSnapshot {
		return backend.DataResponse{}, fmt.Errorf("unsupported Health Models query type %q", *healthQuery.QueryType)
	}
	if history && (query.TimeRange.From.IsZero() || !query.TimeRange.From.Before(query.TimeRange.To)) {
		return backend.DataResponse{}, fmt.Errorf("history requires a time range with from before to")
	}
	var cachedModel *Model
	if saved := healthQuery.DataplaneModel; saved != nil && strings.EqualFold(saved.ID, modelID) &&
		saved.Name != "" && healthQuery.DataplaneURL != "" {
		if _, err := ValidateEndpoint(healthQuery.DataplaneURL, client.dataPlaneURL); err != nil {
			return backend.DataResponse{}, fmt.Errorf("invalid cached dataplaneUrl: %w", err)
		}
		model := *saved
		model.Properties.DataPlaneURI = healthQuery.DataplaneURL
		cachedModel = &model
	}
	key := strings.ToLower(modelID) + "|" + query.TimeRange.To.UTC().String()
	if cachedModel != nil {
		key += "|" + cachedModel.Properties.DataPlaneURI
	}
	healthModel := discovered[key]
	if healthModel == nil {
		var err error
		if cachedModel != nil {
			// Only immutable model metadata is cached. ARM still authorizes each entity/relationship read.
			healthModel, err = client.getModelEntities(ctx, *cachedModel, query.TimeRange.To)
		} else {
			healthModel, err = client.GetHealthModel(ctx, modelID, query.TimeRange.To)
		}
		if err != nil {
			return backend.DataResponse{}, err
		}
		discovered[key] = healthModel
	}
	relationships, err := client.GetRelationships(ctx, modelID, query.TimeRange.To)
	if err != nil {
		return backend.DataResponse{}, err
	}
	endpoint := healthModel.Model.Properties.DataPlaneURI
	var results []EntityResult
	if history {
		results = client.GetAllEntityHistory(ctx, healthModel.Entities, endpoint, query.TimeRange.From, query.TimeRange.To)
	} else {
		results = client.GetAllEntityHealth(ctx, healthModel.Entities, endpoint, query.TimeRange.To)
	}
	modelFrame, err := jsonFrame("healthModel", query.RefID, []Model{healthModel.Model})
	if err != nil {
		return backend.DataResponse{}, err
	}
	entityFrame, err := jsonFrame("entities", query.RefID, healthModel.Entities)
	if err != nil {
		return backend.DataResponse{}, err
	}
	relationshipFrame, err := jsonFrame("relationships", query.RefID, relationships)
	if err != nil {
		return backend.DataResponse{}, err
	}
	resultName := "snapshots"
	if history {
		resultName = "transitions"
	}
	resultFrame, err := jsonFrame(resultName, query.RefID, results)
	if err != nil {
		return backend.DataResponse{}, err
	}
	resultFrame.Meta = &data.FrameMeta{Custom: map[string]any{
		"healthModelResourceId": modelID,
		"dataplaneUrl":          endpoint,
		"dataplaneModel":        healthModel.Model,
	}}
	failed := 0
	for _, result := range results {
		if result.Error != "" {
			failed++
		}
	}
	if failed > 0 {
		resultFrame.AppendNotices(data.Notice{
			Severity: data.NoticeSeverityWarning,
			Text:     fmt.Sprintf("%d of %d Health Model entity reads failed; see each entity's error field", failed, len(results)),
		})
	}
	response := backend.DataResponse{Frames: data.Frames{modelFrame, entityFrame, relationshipFrame, resultFrame}}
	if failed > 0 && failed == len(results) {
		response = backend.ErrorResponseWithErrorSource(backend.DownstreamError(
			fmt.Errorf("all %d Health Model entity reads failed; first failure for %s: %s", failed, results[0].EntityName, results[0].Error),
		))
		response.Frames = data.Frames{modelFrame, entityFrame, relationshipFrame, resultFrame}
	}
	return response, nil
}

func jsonFrame[T any](name, refID string, records []T) (*data.Frame, error) {
	values := make([]*json.RawMessage, 0, len(records))
	for _, record := range records {
		body, err := json.Marshal(record)
		if err != nil {
			return nil, fmt.Errorf("encode Health Models %s: %w", name, err)
		}
		value := json.RawMessage(body)
		values = append(values, &value)
	}
	frame := data.NewFrame(name, data.NewField("data", nil, values))
	frame.RefID = refID
	return frame, nil
}
