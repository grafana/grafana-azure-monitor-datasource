package healthmodels

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testModelID  = "/subscriptions/test-sub/resourceGroups/test-rg/providers/Microsoft.CloudHealth/healthmodels/test-model"
	testARMURL   = "https://management.azure.com"
	testEndpoint = "https://test-model.data.cloudhealth.azure.com"
	testAudience = "https://data.cloudhealth.azure.com"
)

var testTimestamp = time.Date(2026, 11, 17, 10, 0, 0, 123, time.FixedZone("test", 3600))

type mockTransport func(*http.Request) (*http.Response, error)

func (f mockTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func mockClient(handler mockTransport) *http.Client {
	return &http.Client{Transport: handler}
}

func jsonResponse(status int, value any) (*http.Response, error) {
	body, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(string(body))),
	}, nil
}

func modelFixture(endpoint string) Model {
	model := Model{ID: testModelID, Name: "test-model", Location: "uksouth"}
	model.Properties.DataPlaneURI = endpoint
	return model
}

func entityFixture(name string) Entity {
	return Entity{ID: testModelID + "/entities/" + name, Name: name, Properties: map[string]json.RawMessage{}}
}

func relationshipFixture() Relationship {
	relationship := Relationship{ID: testModelID + "/relationships/root-to-child", Name: "root-to-child"}
	relationship.Properties.ParentEntityName = "test-model"
	relationship.Properties.ChildEntityName = "child-entity"
	return relationship
}

func listFixture(value any, next string) map[string]any {
	return map[string]any{"value": value, "nextLink": next}
}

func TestHealthModelsContractVersions(t *testing.T) {
	assert.Equal(t, "2026-12-01-preview", ARMAPIVersion)
	assert.Equal(t, "2026-10-01-preview", DataPlaneAPIVersion)
}

func TestClientDiscoveryAndRelationshipsPaging(t *testing.T) {
	var paths []string
	arm := mockClient(func(req *http.Request) (*http.Response, error) {
		assert.Equal(t, "management.azure.com", req.URL.Host)
		assert.Equal(t, ARMAPIVersion, req.URL.Query().Get("api-version"))
		assert.Equal(t, testTimestamp.UTC().Format(time.RFC3339Nano), req.URL.Query().Get("timestamp"))
		assert.Equal(t, http.MethodGet, req.Method)
		paths = append(paths, req.URL.Path)
		switch req.URL.Path {
		case testModelID:
			return jsonResponse(http.StatusOK, modelFixture(testEndpoint))
		case testModelID + "/entities":
			if req.URL.Query().Get("page") == "2" {
				return jsonResponse(http.StatusOK, listFixture([]Entity{entityFixture("child-entity")}, ""))
			}
			next := *req.URL
			params := next.Query()
			params.Set("page", "2")
			next.RawQuery = params.Encode()
			return jsonResponse(http.StatusOK, listFixture([]Entity{entityFixture("test-model")}, next.String()))
		case testModelID + "/relationships":
			if req.URL.Query().Get("page") == "2" {
				return jsonResponse(http.StatusOK, listFixture([]Relationship{relationshipFixture()}, ""))
			}
			next := *req.URL
			params := next.Query()
			params.Set("page", "2")
			next.RawQuery = params.Encode()
			return jsonResponse(http.StatusOK, listFixture([]Relationship{}, next.RequestURI()))
		default:
			return nil, fmt.Errorf("unexpected ARM request %s", req.URL)
		}
	})
	dataPlane := mockClient(func(req *http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("discovery must not call data plane: %s", req.URL)
	})
	client := NewClient(arm, dataPlane, testARMURL, testAudience)
	model, err := client.GetHealthModel(context.Background(), testModelID, testTimestamp)
	require.NoError(t, err)
	require.Len(t, model.Entities, 2)
	assert.Equal(t, []string{"test-model", "child-entity"}, []string{model.Entities[0].Name, model.Entities[1].Name})
	relationships, err := client.GetRelationships(context.Background(), testModelID, testTimestamp)
	require.NoError(t, err)
	assert.Equal(t, []Relationship{relationshipFixture()}, relationships)
	assert.Len(t, paths, 5)
}

func TestClientSnapshotAndHistoryContract(t *testing.T) {
	start, end := testTimestamp.Add(-time.Hour), testTimestamp
	var snapshots, pages int
	dataPlane := mockClient(func(req *http.Request) (*http.Response, error) {
		assert.Equal(t, "test-model.data.cloudhealth.azure.com", req.URL.Host)
		assert.Equal(t, DataPlaneAPIVersion, req.URL.Query().Get("api-version"))
		assert.Equal(t, http.MethodGet, req.Method)
		assert.Equal(t, "application/json", req.Header.Get("Accept"))
		switch req.URL.Path {
		case "/api/entities/child-entity":
			snapshots++
			assert.Equal(t, end.UTC().Format(time.RFC3339Nano), req.URL.Query().Get("timestamp"))
			return jsonResponse(http.StatusOK, Snapshot{EntityName: "child-entity", HealthState: "Healthy"})
		case "/api/entities/child-entity/transitions":
			pages++
			record := Transition{PreviousState: "Healthy", NewState: "Degraded", OccurredAt: end}
			if req.URL.Query().Get("nextMarker") == "second" {
				assert.Empty(t, req.URL.Query().Get("startAt"))
				assert.Empty(t, req.URL.Query().Get("endAt"))
				record.PreviousState, record.NewState, record.OccurredAt = "Unknown", "Healthy", start
				return jsonResponse(http.StatusOK, listFixture([]Transition{record}, ""))
			}
			assert.Equal(t, start.UTC().Format(time.RFC3339Nano), req.URL.Query().Get("startAt"))
			assert.Equal(t, end.UTC().Format(time.RFC3339Nano), req.URL.Query().Get("endAt"))
			next := *req.URL
			next.RawQuery = url.Values{"api-version": {DataPlaneAPIVersion}, "nextMarker": {"second"}}.Encode()
			return jsonResponse(http.StatusOK, listFixture([]Transition{record}, next.String()))
		default:
			return nil, fmt.Errorf("unexpected data-plane request %s", req.URL)
		}
	})
	client := NewClient(http.DefaultClient, dataPlane, testARMURL, testAudience)
	snapshot, err := client.GetEntityHealth(context.Background(), testEndpoint, "child-entity", end)
	require.NoError(t, err)
	assert.Equal(t, "Healthy", snapshot.HealthState)
	history, err := client.GetEntityHistory(context.Background(), testEndpoint, "child-entity", start, end)
	require.NoError(t, err)
	require.Len(t, history, 2)
	assert.Equal(t, "Degraded", history[0].NewState)
	assert.Equal(t, "Healthy", history[1].NewState)
	assert.True(t, end.Equal(history[0].OccurredAt))
	assert.True(t, start.Equal(history[1].OccurredAt))
	assert.Equal(t, 1, snapshots)
	assert.Equal(t, 2, pages)
}

func TestReadPagesRejectsInvalidResponsesAndContinuationLinks(t *testing.T) {
	for _, tc := range []struct {
		name string
		page any
		err  string
	}{
		{"missing value", map[string]any{}, "value array"},
		{"null value", map[string]any{"value": nil}, "value array"},
		{"wrong value type", map[string]any{"value": "not an array"}, "invalid Health Models JSON"},
		{"cycle", listFixture([]Entity{}, "?page=1"), "repeated a page"},
		{"different host", listFixture([]Entity{}, "https://elsewhere.example/items?page=2"), "original endpoint"},
		{"different path", listFixture([]Entity{}, "/other?page=2"), "original endpoint"},
		{"http downgrade", listFixture([]Entity{}, "http://example.test/items?page=2"), "original endpoint"},
		{"credentials", listFixture([]Entity{}, "https://user@example.test/items?page=2"), "original endpoint"},
		{"fragment", listFixture([]Entity{}, "?page=2#fragment"), "original endpoint"},
		{"malformed URL", listFixture([]Entity{}, "https://[bad"), "invalid Health Models nextLink"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			client := mockClient(func(_ *http.Request) (*http.Response, error) {
				calls++
				return jsonResponse(http.StatusOK, tc.page)
			})
			_, err := readPages[Entity](context.Background(), client, "https://example.test/items?page=1")
			require.ErrorContains(t, err, tc.err)
			assert.Equal(t, 1, calls, "invalid continuations must not be requested")
		})
	}
}

func TestValidateEndpointAndModelID(t *testing.T) {
	for _, endpoint := range []string{
		testAudience, testEndpoint, testEndpoint + "/models/test-model", testEndpoint + ":443",
	} {
		_, err := ValidateEndpoint(endpoint, testAudience)
		require.NoError(t, err, endpoint)
	}
	for _, endpoint := range []string{
		"", "http://test-model.data.cloudhealth.azure.com", "https://data.cloudhealth.azure.com.attacker.test",
		"https://notdata.cloudhealth.azure.com.example", testEndpoint + ":8443", testEndpoint + "?token=value",
		testEndpoint + "#fragment", "https://user@test-model.data.cloudhealth.azure.com",
		testEndpoint + "/../other", testEndpoint + "/%2e%2e/other",
	} {
		_, err := ValidateEndpoint(endpoint, testAudience)
		require.Error(t, err, endpoint)
	}
	require.NoError(t, ValidateModelID(testModelID))
	for _, id := range []string{"", testModelID + "/entities/child-entity", testModelID + "?query=1",
		strings.Replace(testModelID, "test-rg", "..", 1), strings.Replace(testModelID, "test-model", "%2e%2e", 1)} {
		require.Error(t, ValidateModelID(id), id)
	}
}

func TestClientRejectsMalformedContractsAndMissingEndpoint(t *testing.T) {
	for _, tc := range []struct {
		name string
		body any
		path string
		err  string
	}{
		{"missing endpoint", modelFixture(""), testModelID, "invalid properties.dataPlaneUri"},
		{"wrong model", Model{ID: testModelID + "-other", Name: "other"}, testModelID, "expected id or name"},
		{"missing snapshot state", Snapshot{EntityName: "child-entity"}, "/api/entities/child-entity", "healthState"},
		{"wrong snapshot entity", Snapshot{EntityName: "other-entity", HealthState: "Healthy"}, "/api/entities/child-entity", "entityName"},
		{"malformed transition", listFixture([]Transition{{NewState: "Healthy"}}, ""), "/api/entities/child-entity/transitions", "malformed transition"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			transport := mockClient(func(req *http.Request) (*http.Response, error) {
				assert.Equal(t, tc.path, req.URL.Path)
				return jsonResponse(http.StatusOK, tc.body)
			})
			client := NewClient(transport, transport, testARMURL, testAudience)
			var err error
			switch tc.path {
			case testModelID:
				_, err = client.GetHealthModel(context.Background(), testModelID, testTimestamp)
			case "/api/entities/child-entity":
				_, err = client.GetEntityHealth(context.Background(), testEndpoint, "child-entity", testTimestamp)
			default:
				_, err = client.GetEntityHistory(context.Background(), testEndpoint, "child-entity", testTimestamp.Add(-time.Hour), testTimestamp)
			}
			require.ErrorContains(t, err, tc.err)
		})
	}
}

func TestClientRequiresExplicitTimeAndValidEntity(t *testing.T) {
	noCalls := mockClient(func(req *http.Request) (*http.Response, error) {
		t.Errorf("invalid input must not make a request: %s", req.URL)
		return nil, errors.New("unexpected request")
	})
	client := NewClient(noCalls, noCalls, testARMURL, testAudience)
	_, err := client.GetEntityHealth(context.Background(), testEndpoint, "child-entity", time.Time{})
	require.ErrorContains(t, err, "timestamp")
	for _, entity := range []string{"../other", "child/entity", "entity?token=bad", "ab"} {
		_, err = client.GetEntityHealth(context.Background(), testEndpoint, entity, testTimestamp)
		require.ErrorContains(t, err, "entity name")
	}
	for _, start := range []time.Time{{}, testTimestamp, testTimestamp.Add(time.Hour)} {
		_, err = client.GetEntityHistory(context.Background(), testEndpoint, "child-entity", start, testTimestamp)
		require.ErrorContains(t, err, "startAt before endAt")
	}
}

func TestReadJSONReportsFailures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		err    string
	}{
		{"authentication", 401, `{"error":{"code":"Unauthorized","message":"test auth failure"}}`, "401 (Unauthorized): test auth failure"},
		{"permissions", 403, `{}`, "HTTP 403"},
		{"unavailable", 503, "not json", "HTTP 503"},
		{"malformed json", 200, "{", "invalid Health Models JSON"},
		{"oversized", 200, strings.Repeat("x", maxResponseBytes+1), "exceeds"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := mockClient(func(_ *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body)), Header: http.Header{}}, nil
			})
			var result Snapshot
			require.ErrorContains(t, readJSON(context.Background(), client, testEndpoint, &result), tc.err)
		})
	}
}

func TestClientRejectsRedirectsWithoutChangingInjectedClient(t *testing.T) {
	redirects := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/redirected" {
			redirects++
		}
		http.Redirect(w, req, "/redirected", http.StatusFound)
	}))
	defer server.Close()
	original := server.Client()
	client := NewClient(original, original, server.URL, testAudience)
	var result Snapshot
	require.ErrorContains(t, readJSON(context.Background(), client.arm, server.URL, &result), "HTTP 302")
	assert.Zero(t, redirects)
	assert.Nil(t, original.CheckRedirect)
}

func TestReadEntitiesBoundedConcurrencyAndOrder(t *testing.T) {
	entities := make([]Entity, 27)
	for i := range entities {
		entities[i] = entityFixture(fmt.Sprintf("entity-%02d", i))
	}
	started := make(chan struct{}, len(entities))
	release := make(chan struct{})
	released := false
	done := make(chan []EntityResult, 1)
	var active, maximum atomic.Int32
	go func() {
		done <- readEntities(context.Background(), entities, func(name string) EntityResult {
			count := active.Add(1)
			for previous := maximum.Load(); count > previous && !maximum.CompareAndSwap(previous, count); previous = maximum.Load() {
			}
			defer active.Add(-1)
			started <- struct{}{}
			<-release
			return EntityResult{EntityName: name}
		})
	}()
	defer func() {
		if !released {
			close(release)
		}
	}()
	for i := 0; i < maxConcurrentReads; i++ {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("concurrent workers did not start")
		}
	}
	assert.EqualValues(t, maxConcurrentReads, maximum.Load())
	close(release)
	released = true
	select {
	case results := <-done:
		require.Len(t, results, len(entities))
		for i, result := range results {
			assert.Equal(t, entities[i].Name, result.EntityName)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("entity reads did not finish")
	}
	assert.LessOrEqual(t, maximum.Load(), int32(maxConcurrentReads))
}

func TestClientPartialFailuresAndCancellation(t *testing.T) {
	entities := []Entity{entityFixture("healthy-entity"), entityFixture("failed-entity")}
	client := NewClient(http.DefaultClient, mockClient(func(req *http.Request) (*http.Response, error) {
		if err := req.Context().Err(); err != nil {
			return nil, err
		}
		if strings.HasSuffix(req.URL.Path, "/failed-entity") {
			return jsonResponse(http.StatusForbidden, map[string]any{"error": map[string]string{"code": "Forbidden", "message": "test denial"}})
		}
		return jsonResponse(http.StatusOK, Snapshot{EntityName: "healthy-entity", HealthState: "Healthy"})
	}), testARMURL, testAudience)
	results := client.GetAllEntityHealth(context.Background(), entities, testEndpoint, testTimestamp)
	require.Len(t, results, 2)
	assert.Empty(t, results[0].Error)
	require.NotNil(t, results[0].Snapshot)
	assert.Equal(t, "failed-entity", results[1].EntityName)
	assert.Contains(t, results[1].Error, "403")
	assert.Nil(t, results[1].Snapshot)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	results = client.GetAllEntityHealth(ctx, entities, testEndpoint, testTimestamp)
	for i, result := range results {
		assert.Equal(t, entities[i].Name, result.EntityName)
		assert.Contains(t, result.Error, "context canceled")
		assert.Nil(t, result.Snapshot)
	}
	assert.Empty(t, client.GetAllEntityHealth(context.Background(), nil, testEndpoint, testTimestamp))
}

func TestClientPropagatesContextToBothPlanes(t *testing.T) {
	type contextKey struct{}
	ctx := context.WithValue(context.Background(), contextKey{}, "request-context")
	for _, plane := range []string{"ARM", "data plane"} {
		t.Run(plane, func(t *testing.T) {
			client := mockClient(func(req *http.Request) (*http.Response, error) {
				assert.Equal(t, "request-context", req.Context().Value(contextKey{}))
				assert.Empty(t, req.Header.Get("Authorization"))
				assert.Empty(t, req.Header.Get("Cookie"))
				return nil, context.DeadlineExceeded
			})
			c := NewClient(client, client, testARMURL, testAudience)
			var err error
			if plane == "ARM" {
				_, err = c.GetHealthModel(ctx, testModelID, testTimestamp)
			} else {
				_, err = c.GetEntityHealth(ctx, testEndpoint, "child-entity", testTimestamp)
			}
			require.ErrorIs(t, err, context.DeadlineExceeded)
		})
	}
}

func TestClientCancelsInFlightAndQueuedEntityReads(t *testing.T) {
	entities := make([]Entity, maxConcurrentReads+5)
	for i := range entities {
		entities[i] = entityFixture(fmt.Sprintf("entity-%02d", i))
	}
	started := make(chan struct{}, len(entities))
	dataPlane := mockClient(func(req *http.Request) (*http.Response, error) {
		started <- struct{}{}
		<-req.Context().Done()
		return nil, req.Context().Err()
	})
	client := NewClient(http.DefaultClient, dataPlane, testARMURL, testAudience)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan []EntityResult, 1)
	go func() {
		done <- client.GetAllEntityHealth(ctx, entities, testEndpoint, testTimestamp)
	}()
	for i := 0; i < maxConcurrentReads; i++ {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("data-plane reads did not start")
		}
	}
	cancel()
	select {
	case results := <-done:
		require.Len(t, results, len(entities))
		for i, result := range results {
			assert.Equal(t, entities[i].Name, result.EntityName)
			assert.Contains(t, result.Error, "context canceled")
			assert.Nil(t, result.Snapshot)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceled reads did not finish")
	}
}

func TestReadPagesHandlesEmptyLists(t *testing.T) {
	client := mockClient(func(_ *http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK, listFixture([]Entity{}, ""))
	})
	values, err := readPages[Entity](context.Background(), client, testARMURL+"/entities?"+url.Values{"api-version": {ARMAPIVersion}}.Encode())
	require.NoError(t, err)
	assert.NotNil(t, values)
	assert.Empty(t, values)
}
