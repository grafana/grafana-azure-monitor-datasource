package healthmodels

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	ARMAPIVersion       = "2026-12-01-preview"
	DataPlaneAPIVersion = "2026-10-01-preview"
	maxConcurrentReads  = 10
	maxResponseBytes    = 16 << 20
)

var modelIDPattern = regexp.MustCompile(`(?i)^/subscriptions/[^/%?#]+/resourceGroups/[^/%?#]+/providers/Microsoft\.CloudHealth/healthmodels/[^/%?#]+$`)
var entityNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9-]{1,258}[a-zA-Z0-9]$`)

type Model struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Location   string `json:"location,omitempty"`
	Properties struct {
		DataPlaneURI string `json:"dataPlaneUri"`
	} `json:"properties"`
}

type Entity struct {
	ID         string                     `json:"id"`
	Name       string                     `json:"name"`
	Properties map[string]json.RawMessage `json:"properties"`
}

type Relationship struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Properties struct {
		ParentEntityName string `json:"parentEntityName"`
		ChildEntityName  string `json:"childEntityName"`
	} `json:"properties"`
}

type Snapshot struct {
	EntityName                     string            `json:"entityName"`
	HealthState                    string            `json:"healthState"`
	LastHealthTransitionAt         *time.Time        `json:"lastHealthTransitionAt,omitempty"`
	LastHealthTransitionReason     json.RawMessage   `json:"lastHealthTransitionReason,omitempty"`
	Impact                         string            `json:"impact"`
	SignalInputs                   []json.RawMessage `json:"signalInputs"`
	DependencyInputs               []json.RawMessage `json:"dependencyInputs"`
	EffectiveDependencyAggregation json.RawMessage   `json:"effectiveDependencyAggregation"`
	SignalAggregationGroups        []json.RawMessage `json:"signalAggregationGroups"`
}

type Transition struct {
	PreviousState string    `json:"previousState"`
	NewState      string    `json:"newState"`
	OccurredAt    time.Time `json:"occurredAt"`
	Reason        string    `json:"reason,omitempty"`
}

type HealthModel struct {
	Model    Model    `json:"model"`
	Entities []Entity `json:"entities"`
}

// Failed entities remain in the result at their original position, with an explicit error.
type EntityResult struct {
	EntityName  string       `json:"entityName"`
	Snapshot    *Snapshot    `json:"snapshot,omitempty"`
	Transitions []Transition `json:"transitions,omitempty"`
	Error       string       `json:"error,omitempty"`
}

type Client struct {
	arm          *http.Client
	dataPlane    *http.Client
	armURL       string
	dataPlaneURL string
}

func NewClient(arm, dataPlane *http.Client, armURL, dataPlaneURL string) *Client {
	return &Client{
		arm: withoutRedirects(arm), dataPlane: withoutRedirects(dataPlane),
		armURL: strings.TrimRight(armURL, "/"), dataPlaneURL: dataPlaneURL,
	}
}

func withoutRedirects(client *http.Client) *http.Client {
	copy := *client
	copy.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	return &copy
}

func ValidateModelID(id string) error {
	if !modelIDPattern.MatchString(id) {
		return fmt.Errorf("healthModelResourceId must be a full Health Model ARM resource ID")
	}
	for _, part := range strings.Split(id, "/") {
		if part == "." || part == ".." {
			return fmt.Errorf("healthModelResourceId must not contain relative path segments")
		}
	}
	return nil
}

func ValidateEndpoint(endpoint, configuredURL string) (*url.URL, error) {
	u, err := url.Parse(endpoint)
	allowed, allowedErr := url.Parse(configuredURL)
	if err != nil || allowedErr != nil || allowed.Hostname() == "" || u.Scheme != "https" ||
		u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" ||
		(u.Port() != "" && u.Port() != "443") || strings.Contains(u.EscapedPath(), "%") {
		return nil, fmt.Errorf("Health Models data-plane endpoint must be an HTTPS URL in the configured Cloud Health environment")
	}
	host, expected := strings.ToLower(u.Hostname()), strings.ToLower(allowed.Hostname())
	if host != expected && !strings.HasSuffix(host, "."+expected) {
		return nil, fmt.Errorf("Health Models data-plane endpoint is outside the configured Cloud Health environment")
	}
	for _, part := range strings.Split(u.Path, "/") {
		if part == "." || part == ".." {
			return nil, fmt.Errorf("Health Models data-plane endpoint must not contain relative path segments")
		}
	}
	return u, nil
}

func armParams(timestamp time.Time) url.Values {
	params := url.Values{"api-version": {ARMAPIVersion}}
	if !timestamp.IsZero() {
		params.Set("timestamp", timestamp.UTC().Format(time.RFC3339Nano))
	}
	return params
}

func (c *Client) GetHealthModel(ctx context.Context, modelID string, timestamp time.Time) (*HealthModel, error) {
	if err := ValidateModelID(modelID); err != nil {
		return nil, err
	}
	target := c.armURL + modelID + "?" + armParams(timestamp).Encode()
	var model Model
	if err := readJSON(ctx, c.arm, target, &model); err != nil {
		return nil, fmt.Errorf("read Health Model metadata: %w", err)
	}
	if !strings.EqualFold(model.ID, modelID) || model.Name == "" {
		return nil, fmt.Errorf("Health Model metadata is missing its expected id or name")
	}
	if _, err := ValidateEndpoint(model.Properties.DataPlaneURI, c.dataPlaneURL); err != nil {
		return nil, fmt.Errorf("invalid properties.dataPlaneUri: %w", err)
	}
	return c.getModelEntities(ctx, model, timestamp)
}

func (c *Client) getModelEntities(ctx context.Context, model Model, timestamp time.Time) (*HealthModel, error) {
	modelID := model.ID
	entities, err := readPages[Entity](ctx, c.arm, c.armURL+modelID+"/entities?"+armParams(timestamp).Encode())
	if err != nil {
		return nil, fmt.Errorf("list Health Model entities: %w", err)
	}
	seen := map[string]bool{}
	for _, entity := range entities {
		if !entityNamePattern.MatchString(entity.Name) || entity.ID == "" || entity.Properties == nil || seen[entity.Name] {
			return nil, fmt.Errorf("entity list contains a malformed or duplicate entity")
		}
		seen[entity.Name] = true
	}
	return &HealthModel{Model: model, Entities: entities}, nil
}

func (c *Client) GetRelationships(ctx context.Context, modelID string, timestamp time.Time) ([]Relationship, error) {
	if err := ValidateModelID(modelID); err != nil {
		return nil, err
	}
	relationships, err := readPages[Relationship](ctx, c.arm, c.armURL+modelID+"/relationships?"+armParams(timestamp).Encode())
	if err != nil {
		return nil, fmt.Errorf("list Health Model relationships: %w", err)
	}
	for _, relationship := range relationships {
		if relationship.ID == "" || relationship.Name == "" || relationship.Properties.ParentEntityName == "" || relationship.Properties.ChildEntityName == "" {
			return nil, fmt.Errorf("relationship list contains a malformed relationship")
		}
	}
	return relationships, nil
}

func (c *Client) entityURL(endpoint, entity string) (*url.URL, error) {
	u, err := ValidateEndpoint(endpoint, c.dataPlaneURL)
	if err != nil {
		return nil, err
	}
	if !entityNamePattern.MatchString(entity) {
		return nil, fmt.Errorf("invalid Health Model entity name")
	}
	return u.JoinPath("api", "entities", entity), nil
}

func (c *Client) GetEntityHealth(ctx context.Context, endpoint, entity string, timestamp time.Time) (*Snapshot, error) {
	if timestamp.IsZero() {
		return nil, fmt.Errorf("an explicit snapshot timestamp is required")
	}
	u, err := c.entityURL(endpoint, entity)
	if err != nil {
		return nil, err
	}
	u.RawQuery = url.Values{
		"api-version": {DataPlaneAPIVersion},
		"timestamp":   {timestamp.UTC().Format(time.RFC3339Nano)},
	}.Encode()
	var snapshot Snapshot
	if err := readJSON(ctx, c.dataPlane, u.String(), &snapshot); err != nil {
		return nil, err
	}
	if snapshot.EntityName != entity || snapshot.HealthState == "" {
		return nil, fmt.Errorf("snapshot response is missing its expected entityName or healthState")
	}
	return &snapshot, nil
}

func (c *Client) GetEntityHistory(ctx context.Context, endpoint, entity string, startAt, endAt time.Time) ([]Transition, error) {
	if startAt.IsZero() || endAt.IsZero() || !startAt.Before(endAt) {
		return nil, fmt.Errorf("history requires an explicit startAt before endAt")
	}
	u, err := c.entityURL(endpoint, entity)
	if err != nil {
		return nil, err
	}
	u = u.JoinPath("transitions")
	u.RawQuery = url.Values{
		"api-version": {DataPlaneAPIVersion},
		"startAt":     {startAt.UTC().Format(time.RFC3339Nano)},
		"endAt":       {endAt.UTC().Format(time.RFC3339Nano)},
	}.Encode()
	transitions, err := readPages[Transition](ctx, c.dataPlane, u.String())
	if err != nil {
		return nil, err
	}
	for _, transition := range transitions {
		if transition.OccurredAt.IsZero() || transition.PreviousState == "" || transition.NewState == "" {
			return nil, fmt.Errorf("history contains a malformed transition")
		}
	}
	return transitions, nil
}

func (c *Client) GetAllEntityHealth(ctx context.Context, entities []Entity, endpoint string, timestamp time.Time) []EntityResult {
	return readEntities(ctx, entities, func(entity string) EntityResult {
		snapshot, err := c.GetEntityHealth(ctx, endpoint, entity, timestamp)
		result := EntityResult{EntityName: entity, Snapshot: snapshot}
		if err != nil {
			result.Error = err.Error()
		}
		return result
	})
}

func (c *Client) GetAllEntityHistory(ctx context.Context, entities []Entity, endpoint string, startAt, endAt time.Time) []EntityResult {
	return readEntities(ctx, entities, func(entity string) EntityResult {
		transitions, err := c.GetEntityHistory(ctx, endpoint, entity, startAt, endAt)
		result := EntityResult{EntityName: entity, Transitions: transitions}
		if err != nil {
			result.Error = err.Error()
		}
		return result
	})
}

func readEntities(ctx context.Context, entities []Entity, read func(string) EntityResult) []EntityResult {
	results := make([]EntityResult, len(entities))
	sem := make(chan struct{}, maxConcurrentReads)
	var wg sync.WaitGroup
	for i, entity := range entities {
		select {
		case <-ctx.Done():
			results[i] = EntityResult{EntityName: entity.Name, Error: ctx.Err().Error()}
			continue
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func(i int, name string) {
			defer wg.Done()
			defer func() { <-sem }()
			results[i] = read(name)
		}(i, entity.Name)
	}
	wg.Wait()
	return results
}

func readJSON(ctx context.Context, client *http.Client, target string, result any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	res, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("Health Models request failed: %w", err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, maxResponseBytes+1))
	if err != nil {
		return fmt.Errorf("read Health Models response: %w", err)
	}
	if len(body) > maxResponseBytes {
		return fmt.Errorf("Health Models response exceeds %d bytes", maxResponseBytes)
	}
	if res.StatusCode != http.StatusOK {
		var failure struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(body, &failure) == nil && failure.Error.Code != "" {
			return fmt.Errorf("Health Models HTTP %d (%s): %s", res.StatusCode, failure.Error.Code, failure.Error.Message)
		}
		return fmt.Errorf("Health Models HTTP %d; check credentials, read permissions and service availability", res.StatusCode)
	}
	if err := json.Unmarshal(body, result); err != nil {
		return fmt.Errorf("invalid Health Models JSON response: %w", err)
	}
	return nil
}

func readPages[T any](ctx context.Context, client *http.Client, target string) ([]T, error) {
	base, err := url.Parse(target)
	if err != nil {
		return nil, err
	}
	items := make([]T, 0)
	seen := map[string]bool{}
	for target != "" {
		if seen[target] || len(seen) >= 10000 {
			return nil, fmt.Errorf("Health Models pagination repeated a page or exceeded its safety limit")
		}
		seen[target] = true
		var page struct {
			Value    *[]T   `json:"value"`
			NextLink string `json:"nextLink"`
		}
		if err := readJSON(ctx, client, target, &page); err != nil {
			return nil, err
		}
		if page.Value == nil {
			return nil, fmt.Errorf("Health Models list response must contain a value array")
		}
		items = append(items, (*page.Value)...)
		if page.NextLink == "" {
			break
		}
		next, err := url.Parse(page.NextLink)
		if err != nil {
			return nil, fmt.Errorf("invalid Health Models nextLink")
		}
		next = base.ResolveReference(next)
		if next.Scheme != base.Scheme || next.Host != base.Host || next.EscapedPath() != base.EscapedPath() || next.User != nil || next.Fragment != "" {
			return nil, fmt.Errorf("Health Models nextLink must stay on the original endpoint and operation")
		}
		target = next.String()
	}
	return items, nil
}
