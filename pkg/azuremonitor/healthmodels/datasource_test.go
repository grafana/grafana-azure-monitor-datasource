package healthmodels

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/grafana/grafana-plugin-sdk-go/data"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/grafana/grafana-azure-monitor-datasource/pkg/azuremonitor/kinds/dataquery"
	"github.com/grafana/grafana-azure-monitor-datasource/pkg/azuremonitor/types"
)

func queryFixture(t *testing.T, ref string, history bool) backend.DataQuery {
	t.Helper()
	queryType := dataquery.AzureHealthModelsQueryTypeEntityHealthSnapshot
	if history {
		queryType = dataquery.AzureHealthModelsQueryTypeEntityHealthHistory
	}
	body, err := json.Marshal(map[string]any{
		"subscription": "test-sub",
		"azureHealthModels": map[string]any{
			"healthModelResourceId": testModelID,
			"queryType":             queryType,
		},
	})
	require.NoError(t, err)
	return backend.DataQuery{
		RefID: ref,
		JSON:  body,
		TimeRange: backend.TimeRange{
			From: testTimestamp.Add(-time.Hour),
			To:   testTimestamp,
		},
	}
}

func decodeFrameRecord[T any](t *testing.T, frame *data.Frame, index int) T {
	t.Helper()
	require.Len(t, frame.Fields, 1)
	require.Equal(t, "data", frame.Fields[0].Name)
	value := frame.Fields[0].At(index)
	require.IsType(t, (*json.RawMessage)(nil), value)
	var record T
	require.NoError(t, json.Unmarshal(*value.(*json.RawMessage), &record))
	return record
}

func TestDatasourceExecutesSnapshotHistoryAndRefreshWithoutEditor(t *testing.T) {
	var metadataCalls, entityCalls, relationshipCalls int
	var snapshotCalls, historyCalls atomic.Int32
	arm := mockClient(func(req *http.Request) (*http.Response, error) {
		assert.Equal(t, "management.azure.com", req.URL.Host, "ARM client must not service data-plane requests")
		assert.Empty(t, req.Header.Get("Authorization"), "client must construct a fresh request")
		assert.Empty(t, req.Header.Get("Cookie"))
		assert.Equal(t, testTimestamp.UTC().Format(time.RFC3339Nano), req.URL.Query().Get("timestamp"))
		switch req.URL.Path {
		case testModelID:
			metadataCalls++
			return jsonResponse(http.StatusOK, modelFixture(testEndpoint))
		case testModelID + "/entities":
			entityCalls++
			return jsonResponse(http.StatusOK, listFixture([]Entity{entityFixture("test-model"), entityFixture("child-entity")}, ""))
		case testModelID + "/relationships":
			relationshipCalls++
			return jsonResponse(http.StatusOK, listFixture([]Relationship{relationshipFixture()}, ""))
		default:
			return nil, fmt.Errorf("unexpected ARM request %s", req.URL)
		}
	})
	dataPlane := mockClient(func(req *http.Request) (*http.Response, error) {
		assert.Equal(t, "test-model.data.cloudhealth.azure.com", req.URL.Host, "data-plane client must not service ARM requests")
		assert.Empty(t, req.Header.Get("Authorization"), "browser authorization must not be copied")
		assert.Empty(t, req.Header.Get("Cookie"))
		if strings.HasSuffix(req.URL.Path, "/transitions") {
			historyCalls.Add(1)
			assert.Equal(t, testTimestamp.Add(-time.Hour).UTC().Format(time.RFC3339Nano), req.URL.Query().Get("startAt"))
			assert.Equal(t, testTimestamp.UTC().Format(time.RFC3339Nano), req.URL.Query().Get("endAt"))
			return jsonResponse(http.StatusOK, listFixture([]Transition{{
				PreviousState: "Unknown", NewState: "Healthy", OccurredAt: testTimestamp.Add(-time.Minute),
			}}, ""))
		}
		snapshotCalls.Add(1)
		assert.Equal(t, testTimestamp.UTC().Format(time.RFC3339Nano), req.URL.Query().Get("timestamp"))
		name := req.URL.Path[strings.LastIndex(req.URL.Path, "/")+1:]
		return jsonResponse(http.StatusOK, Snapshot{EntityName: name, HealthState: "Healthy"})
	})
	info := types.DatasourceInfo{Services: map[string]types.DatasourceService{
		types.RouteAzureMonitor: {URL: testARMURL, HTTPClient: arm},
	}}
	queries := []backend.DataQuery{queryFixture(t, "A", false), queryFixture(t, "B", true)}
	for refresh := 1; refresh <= 2; refresh++ {
		response, err := (&Datasource{}).ExecuteTimeSeriesQuery(context.Background(), queries, info, dataPlane, testAudience, false)
		require.NoError(t, err)
		require.Len(t, response.Responses, 2)
		for _, ref := range []string{"A", "B"} {
			result := response.Responses[ref]
			require.NoError(t, result.Error)
			require.Len(t, result.Frames, 4)
			expectedNames := []string{"healthModel", "entities", "relationships", "snapshots"}
			if ref == "B" {
				expectedNames[3] = "transitions"
			}
			for i, frame := range result.Frames {
				assert.Equal(t, expectedNames[i], frame.Name)
				assert.Equal(t, ref, frame.RefID)
				require.Len(t, frame.Fields, 1)
				assert.Equal(t, data.FieldTypeNullableJSON, frame.Fields[0].Type())
			}
			assert.Equal(t, testModelID, decodeFrameRecord[Model](t, result.Frames[0], 0).ID)
			assert.Equal(t, "child-entity", decodeFrameRecord[Entity](t, result.Frames[1], 1).Name)
			assert.Equal(t, "child-entity", decodeFrameRecord[Relationship](t, result.Frames[2], 0).Properties.ChildEntityName)
			record := decodeFrameRecord[EntityResult](t, result.Frames[3], 0)
			assert.Empty(t, record.Error)
			assert.Equal(t, "test-model", record.EntityName)
			if ref == "A" {
				require.NotNil(t, record.Snapshot)
				assert.Equal(t, "Healthy", record.Snapshot.HealthState)
			} else {
				require.Len(t, record.Transitions, 1)
				assert.Equal(t, "Healthy", record.Transitions[0].NewState)
			}
			assert.Equal(t, map[string]any{
				"healthModelResourceId": testModelID, "dataplaneUrl": testEndpoint,
				"dataplaneModel": modelFixture(testEndpoint),
			}, result.Frames[3].Meta.Custom)
		}
		assert.Equal(t, refresh, metadataCalls, "share discovery inside an execution, not across refreshes")
		assert.Equal(t, refresh, entityCalls)
		assert.Equal(t, refresh*2, relationshipCalls)
		assert.EqualValues(t, refresh*2, snapshotCalls.Load())
		assert.EqualValues(t, refresh*2, historyCalls.Load())
	}
}

func withCachedModel(t *testing.T, query backend.DataQuery, model Model, endpoint string) backend.DataQuery {
	t.Helper()
	var body struct {
		Subscription string `json:"subscription"`
		HealthModels Query  `json:"azureHealthModels"`
	}
	require.NoError(t, json.Unmarshal(query.JSON, &body))
	body.HealthModels.DataplaneModel = &model
	body.HealthModels.DataplaneURL = endpoint
	var err error
	query.JSON, err = json.Marshal(body)
	require.NoError(t, err)
	return query
}

func TestDatasourcePersistsDiscoveryAndReusesEndpointAfterReload(t *testing.T) {
	var metadataCalls, entityCalls, relationshipCalls int
	var healthCalls atomic.Int32
	arm := mockClient(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case testModelID:
			metadataCalls++
			return jsonResponse(http.StatusOK, modelFixture(testEndpoint))
		case testModelID + "/entities":
			entityCalls++
			return jsonResponse(http.StatusOK, listFixture([]Entity{entityFixture("child-entity")}, ""))
		case testModelID + "/relationships":
			relationshipCalls++
			return jsonResponse(http.StatusOK, listFixture([]Relationship{}, ""))
		default:
			return nil, fmt.Errorf("unexpected ARM request %s", req.URL)
		}
	})
	dataPlane := mockClient(func(req *http.Request) (*http.Response, error) {
		healthCalls.Add(1)
		if strings.HasSuffix(req.URL.Path, "/transitions") {
			return jsonResponse(http.StatusOK, listFixture([]Transition{}, ""))
		}
		return jsonResponse(http.StatusOK, Snapshot{EntityName: "child-entity", HealthState: "Healthy"})
	})
	info := types.DatasourceInfo{Services: map[string]types.DatasourceService{
		types.RouteAzureMonitor: {URL: testARMURL, HTTPClient: arm},
	}}
	query := queryFixture(t, "A", false)
	for refresh := 0; refresh < 3; refresh++ {
		// A fresh executor and serialized query also exercise a saved dashboard/backend restart.
		response, err := (&Datasource{}).ExecuteTimeSeriesQuery(context.Background(), []backend.DataQuery{query}, info, dataPlane, testAudience, false)
		require.NoError(t, err)
		result := response.Responses["A"]
		require.NoError(t, result.Error)
		require.Len(t, result.Frames, 4)
		model := decodeFrameRecord[Model](t, result.Frames[0], 0)
		assert.Equal(t, modelFixture(testEndpoint), model)
		query = withCachedModel(t, queryFixture(t, "A", refresh == 0), model, model.Properties.DataPlaneURI)
		query.TimeRange.To = query.TimeRange.To.Add(time.Duration(refresh+1) * time.Minute)
		assert.Equal(t, 1, metadataCalls, "the saved endpoint must avoid repeated ARM model discovery")
		assert.Equal(t, refresh+1, entityCalls, "entity metadata must remain live")
		assert.Equal(t, refresh+1, relationshipCalls, "relationships must remain live")
		assert.EqualValues(t, refresh+1, healthCalls.Load(), "health must never be cached")
	}
}

func TestDatasourceCachedEndpointStillRequiresARMAuthorization(t *testing.T) {
	armCalls := 0
	arm := mockClient(func(req *http.Request) (*http.Response, error) {
		armCalls++
		assert.Equal(t, testModelID+"/entities", req.URL.Path)
		return jsonResponse(http.StatusForbidden, map[string]any{})
	})
	dataPlane := mockClient(func(req *http.Request) (*http.Response, error) {
		t.Errorf("ARM denial must prevent data-plane reads: %s", req.URL)
		return nil, fmt.Errorf("unexpected request")
	})
	query := withCachedModel(t, queryFixture(t, "A", false), modelFixture(testEndpoint), testEndpoint)
	_, err := executeQuery(context.Background(), NewClient(arm, dataPlane, testARMURL, testAudience), query, map[string]*HealthModel{})
	require.ErrorContains(t, err, "HTTP 403")
	assert.Equal(t, 1, armCalls)
}

func TestDatasourceRejectsUnsafeCachedEndpoints(t *testing.T) {
	for _, endpoint := range []string{
		"https://example.test", "http://data.cloudhealth.azure.com", "https://untrusted.invalid",
	} {
		t.Run(endpoint, func(t *testing.T) {
			noCalls := mockClient(func(req *http.Request) (*http.Response, error) {
				t.Errorf("unsafe cached endpoint must not be used: %s", req.URL)
				return nil, fmt.Errorf("unexpected request")
			})
			query := withCachedModel(t, queryFixture(t, "A", false), modelFixture(endpoint), endpoint)
			_, err := executeQuery(context.Background(), NewClient(noCalls, noCalls, testARMURL, testAudience), query, map[string]*HealthModel{})
			require.ErrorContains(t, err, "invalid cached dataplaneUrl")
		})
	}
}

func TestDatasourceRediscoversWhenCachedModelDoesNotMatch(t *testing.T) {
	cached := modelFixture("https://old-model.data.cloudhealth.azure.com")
	cached.ID += "-other"
	calls := 0
	arm := mockClient(func(req *http.Request) (*http.Response, error) {
		calls++
		assert.Equal(t, testModelID, req.URL.Path)
		return jsonResponse(http.StatusForbidden, map[string]any{})
	})
	query := withCachedModel(t, queryFixture(t, "A", false), cached, cached.Properties.DataPlaneURI)
	_, err := executeQuery(context.Background(), NewClient(arm, arm, testARMURL, testAudience), query, map[string]*HealthModel{})
	require.ErrorContains(t, err, "read Health Model metadata")
	assert.Equal(t, 1, calls, "a different model must trigger fresh discovery, not use the saved endpoint")
}

func TestDatasourceRetainsResultsAndReportsCompleteDataPlaneFailure(t *testing.T) {
	for _, history := range []bool{false, true} {
		t.Run(fmt.Sprintf("history=%t", history), func(t *testing.T) {
			for _, tc := range []struct {
				name     string
				entities []Entity
				failed   int
			}{
				{"success", []Entity{entityFixture("healthy-entity")}, 0},
				{"partial", []Entity{entityFixture("healthy-entity"), entityFixture("failed-entity")}, 1},
				{"all failed", []Entity{entityFixture("failed-entity"), entityFixture("also-failed-entity")}, 2},
				{"empty", []Entity{}, 0},
			} {
				t.Run(tc.name, func(t *testing.T) {
					arm := mockClient(func(req *http.Request) (*http.Response, error) {
						switch req.URL.Path {
						case testModelID:
							return jsonResponse(http.StatusOK, modelFixture(testEndpoint))
						case testModelID + "/entities":
							return jsonResponse(http.StatusOK, listFixture(tc.entities, ""))
						case testModelID + "/relationships":
							return jsonResponse(http.StatusOK, listFixture([]Relationship{}, ""))
						default:
							return nil, fmt.Errorf("unexpected ARM path %s", req.URL.Path)
						}
					})
					dataPlane := mockClient(func(req *http.Request) (*http.Response, error) {
						if strings.Contains(req.URL.Path, "failed-entity") {
							return jsonResponse(http.StatusForbidden, map[string]any{})
						}
						if history {
							return jsonResponse(http.StatusOK, listFixture([]Transition{}, ""))
						}
						return jsonResponse(http.StatusOK, Snapshot{EntityName: "healthy-entity", HealthState: "Healthy"})
					})
					info := types.DatasourceInfo{Services: map[string]types.DatasourceService{
						types.RouteAzureMonitor: {URL: testARMURL, HTTPClient: arm},
					}}
					response, err := (&Datasource{}).ExecuteTimeSeriesQuery(context.Background(),
						[]backend.DataQuery{queryFixture(t, "A", history)}, info, dataPlane, testAudience, false)
					require.NoError(t, err)
					result := response.Responses["A"]
					if tc.failed > 0 && tc.failed == len(tc.entities) {
						require.ErrorContains(t, result.Error, "all 2 Health Model entity reads failed")
						assert.Contains(t, result.Error.Error(), "failed-entity")
						assert.Contains(t, result.Error.Error(), "HTTP 403")
						assert.Equal(t, backend.ErrorSourceDownstream, result.ErrorSource)
						assert.NotEqual(t, backend.StatusOK, result.Status)
					} else {
						require.NoError(t, result.Error)
						assert.Empty(t, result.ErrorSource)
					}
					require.Len(t, result.Frames, 4)
					frame := result.Frames[3]
					assert.Equal(t, len(tc.entities), frame.Rows())
					failed := 0
					for i := range tc.entities {
						record := decodeFrameRecord[EntityResult](t, frame, i)
						assert.Equal(t, tc.entities[i].Name, record.EntityName)
						if record.Error != "" {
							failed++
							assert.Contains(t, record.Error, "HTTP 403")
							assert.Nil(t, record.Snapshot)
						}
					}
					assert.Equal(t, tc.failed, failed)
					if tc.failed > 0 {
						require.Len(t, frame.Meta.Notices, 1)
						assert.Equal(t, data.NoticeSeverityWarning, frame.Meta.Notices[0].Severity)
						assert.Contains(t, frame.Meta.Notices[0].Text, fmt.Sprintf("%d of %d", tc.failed, len(tc.entities)))
					} else {
						assert.Empty(t, frame.Meta.Notices)
					}
				})
			}
		})
	}
}

func TestDatasourceDoesNotFallbackWhenRealMetadataOmitsEndpoint(t *testing.T) {
	arm := mockClient(func(req *http.Request) (*http.Response, error) {
		assert.Equal(t, testModelID, req.URL.Path)
		return jsonResponse(http.StatusOK, modelFixture(""))
	})
	var dataPlaneCalls atomic.Int32
	dataPlane := mockClient(func(req *http.Request) (*http.Response, error) {
		dataPlaneCalls.Add(1)
		return nil, fmt.Errorf("missing endpoint must not trigger a request: %s", req.URL)
	})
	query := queryFixture(t, "A", false)
	var body map[string]any
	require.NoError(t, json.Unmarshal(query.JSON, &body))
	body["azureHealthModels"].(map[string]any)["dataplaneUrl"] = "https://old-model.data.cloudhealth.azure.com"
	var err error
	query.JSON, err = json.Marshal(body)
	require.NoError(t, err)
	info := types.DatasourceInfo{Services: map[string]types.DatasourceService{
		types.RouteAzureMonitor: {URL: testARMURL, HTTPClient: arm},
	}}
	response, err := (&Datasource{}).ExecuteTimeSeriesQuery(context.Background(), []backend.DataQuery{query}, info, dataPlane, testAudience, false)
	require.NoError(t, err)
	result := response.Responses["A"]
	require.ErrorContains(t, result.Error, "invalid properties.dataPlaneUri")
	assert.Empty(t, result.Frames)
	assert.Zero(t, dataPlaneCalls.Load(), "do not trust a stale saved URL or return fake health")
}

func TestDatasourceRejectsInvalidQueriesBeforeRequests(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*backend.DataQuery)
		err    string
	}{
		{"malformed JSON", func(q *backend.DataQuery) { q.JSON = []byte("{") }, "decode"},
		{"missing model", func(q *backend.DataQuery) { q.JSON = []byte(`{}`) }, "select a Health Model"},
		{"subscription mismatch", func(q *backend.DataQuery) {
			q.JSON = []byte(strings.Replace(string(q.JSON), `"subscription":"test-sub"`, `"subscription":"other-sub"`, 1))
		}, "does not belong"},
		{"missing time", func(q *backend.DataQuery) { q.TimeRange.To = time.Time{} }, "time range"},
		{"unsupported query type", func(q *backend.DataQuery) {
			q.JSON = []byte(strings.ReplaceAll(string(q.JSON), "Entity Health Snapshot", "unsupported"))
		}, "unsupported"},
		{"reversed history range", func(q *backend.DataQuery) {
			q.JSON = []byte(strings.ReplaceAll(string(q.JSON), "Entity Health Snapshot", "Entity Health History"))
			q.TimeRange.From = q.TimeRange.To.Add(time.Hour)
		}, "from before to"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			noCalls := mockClient(func(req *http.Request) (*http.Response, error) {
				t.Errorf("invalid query must not call upstream: %s", req.URL)
				return nil, fmt.Errorf("unexpected request")
			})
			query := queryFixture(t, "A", false)
			tc.change(&query)
			_, err := executeQuery(context.Background(), NewClient(noCalls, noCalls, testARMURL, testAudience), query, map[string]*HealthModel{})
			require.ErrorContains(t, err, tc.err)
		})
	}
}
