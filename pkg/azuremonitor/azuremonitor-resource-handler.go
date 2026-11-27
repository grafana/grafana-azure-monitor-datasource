package azuremonitor

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/grafana/grafana-plugin-sdk-go/backend/log"

	"github.com/grafana/grafana-azure-monitor-datasource/pkg/azuremonitor/healthmodels"
)

func getTarget(original string) (target string, err error) {
	splittedPath := strings.Split(original, "/")
	if len(splittedPath) < 3 {
		err = fmt.Errorf("the request should contain the service on its path")
		return
	}
	target = fmt.Sprintf("/%s", strings.Join(splittedPath[2:], "/"))
	return
}

type httpServiceProxy struct {
	logger log.Logger
}

func (s *httpServiceProxy) writeErrorResponse(rw http.ResponseWriter, statusCode int, message string) error {
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(statusCode)

	// Set error response to initial error message
	errorBody := map[string]string{"error": message}

	// Attempt to locate JSON portion in error message
	re := regexp.MustCompile(`\{.*?\}`)
	jsonPart := re.FindString(message)
	if jsonPart != "" {
		var jsonData map[string]interface{}
		if unmarshalErr := json.Unmarshal([]byte(jsonPart), &jsonData); unmarshalErr != nil {
			errorBody["error"] = fmt.Sprintf("Invalid JSON format in error message. Raw error: %s", message)
			s.logger.Error("failed to unmarshal JSON error message", "error", unmarshalErr)
		} else {
			// Extract relevant fields for a formatted error message
			errorType, _ := jsonData["error"].(string)
			errorDescription, ok := jsonData["error_description"].(string)
			if !ok {
				s.logger.Error("unable to convert error_description to string", "rawError", jsonData["error_description"])
				// Attempt to just format the error as a string
				errorDescription = fmt.Sprintf("%v", jsonData["error_description"])
			}
			if errorType == "" {
				errorType = "UnknownError"
			}

			errorBody["error"] = fmt.Sprintf("%s: %s", errorType, errorDescription)
		}
	}

	jsonRes, _ := json.Marshal(errorBody)
	_, err := rw.Write(jsonRes)
	if err != nil {
		return fmt.Errorf("unable to write HTTP response: %v", err)
	}
	return nil
}

func (s *httpServiceProxy) Do(rw http.ResponseWriter, req *http.Request, cli *http.Client) (http.ResponseWriter, error) {
	res, err := cli.Do(req)
	if err != nil {
		return nil, s.writeErrorResponse(rw, http.StatusInternalServerError, fmt.Sprintf("unexpected error: %v", err))
	}

	defer func() {
		if err := res.Body.Close(); err != nil {
			s.logger.Warn("Failed to close response body", "err", err)
		}
	}()

	body, err := io.ReadAll(res.Body)
	if err != nil {
		rw.WriteHeader(http.StatusInternalServerError)
		_, err = fmt.Fprintf(rw, "unexpected error %v", err)
		if err != nil {
			return nil, fmt.Errorf("unable to write HTTP response: %v", err)
		}
		return nil, err
	}

	rw.WriteHeader(res.StatusCode)
	_, err = rw.Write(body)
	if err != nil {
		return nil, fmt.Errorf("unable to write HTTP response: %v", err)
	}

	for k, v := range res.Header {
		rw.Header().Set(k, v[0])
		for _, v := range v[1:] {
			rw.Header().Add(k, v)
		}
	}

	// Return the ResponseWriter for testing purposes
	return rw, nil
}

func writeErrorResponse(rw http.ResponseWriter, code int, msg string) {
	rw.WriteHeader(code)
	errorBody := map[string]string{
		"error": msg,
	}
	jsonRes, _ := json.Marshal(errorBody)
	_, err := rw.Write(jsonRes)
	if err != nil {
		backend.Logger.Error("Unable to write HTTP response", "error", err)
	}
}

func (s *DataSource) handleResourceReq(subDataSource string) func(rw http.ResponseWriter, req *http.Request) {
	return func(rw http.ResponseWriter, req *http.Request) {
		s.logger.Debug("Received resource call", "url", req.URL.String(), "method", req.Method)

		newPath, err := getTarget(req.URL.Path)
		if err != nil {
			writeErrorResponse(rw, http.StatusBadRequest, err.Error())
			return
		}

		if !isAllowedResourcePath(subDataSource, req.Method, newPath) {
			s.logger.Error("Rejected resource call to a path the datasource does not support", "method", req.Method, "path", newPath, "subDataSource", subDataSource)
			writeErrorResponse(rw, http.StatusForbidden, "the requested resource path is not supported by this datasource")
			return
		}

		dsInfo := s.info

		service := dsInfo.Services[subDataSource]
		serviceURL, err := url.Parse(service.URL)
		if err != nil {
			writeErrorResponse(rw, http.StatusInternalServerError, fmt.Sprintf("unexpected error %v", err))
			return
		}
		req.URL.Path = newPath
		req.URL.Host = serviceURL.Host
		req.URL.Scheme = serviceURL.Scheme

		_, err = s.executors[subDataSource].ResourceRequest(rw, req, service.HTTPClient)
		if err != nil {
			// The ResourceRequest function should handle writing the error response
			// We log the error here to ensure it's captured
			s.logger.Error("error in resource request", "error", err)
			return
		}
	}
}

func (s *DataSource) handleHealthModelsResourceReq(rw http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		rw.Header().Set("Allow", http.MethodGet)
		writeErrorResponse(rw, http.StatusMethodNotAllowed, "the Health Models resource route only supports GET requests")
		return
	}
	service, ok := s.info.Services[azureHealthModels]
	if !ok {
		writeErrorResponse(rw, http.StatusBadRequest, "Health Models is not configured for this Azure cloud; configure an explicit Health Models customized route")
		return
	}
	upstream, err := healthmodels.NewDataPlaneRequest(req, service.URL)
	if err != nil {
		writeErrorResponse(rw, http.StatusBadRequest, err.Error())
		return
	}
	_, err = s.executors[azureHealthModels].ResourceRequest(rw, upstream, service.HTTPClient)
	if err != nil {
		s.logger.Error("error in Health Models resource request", "error", err)
	}
}

// newResourceMux provides route definitions shared with the frontend.
// Check: /public/app/plugins/datasource/azuremonitor/utils/common.ts <routeNames>
func (s *DataSource) newResourceMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/azuremonitor/", s.handleResourceReq(azureMonitor))
	mux.HandleFunc("/loganalytics/", s.handleResourceReq(azureLogAnalytics))
	mux.HandleFunc("/resourcegraph/", s.handleResourceReq(azureResourceGraph))
	mux.HandleFunc("/healthmodels", s.handleHealthModelsResourceReq)
	mux.HandleFunc("/healthmodels/", s.handleHealthModelsResourceReq)
	return mux
}
