package util

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"

	log "github.com/sirupsen/logrus"
)

// JSONResponse sends a JSON payload in response to an HTTP request
func JSONResponse(w http.ResponseWriter, v interface{}, statusCode int) {
	w.Header().Set("Content-Type", "application/json")

	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(v); err != nil {
		log.WithField("err", err).Error("Failed to serialize JSON payload")
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, "Failed to serialize JSON payload")
		return
	}

	w.WriteHeader(statusCode)
	if _, err := buf.WriteTo(w); err != nil {
		log.WithError(err).Error("Failed to write JSON response")
	}
}

type jsonError struct {
	Message string `json:"Err"`
}

// JSONErrResponse sends an error as a JSON object with an Err field
func JSONErrResponse(w http.ResponseWriter, err error, statusCode int) {
	log.WithError(err).Error("Error while processing request")

	w.Header().Set("Content-Type", "application/problem+json")
	if statusCode == 0 {
		statusCode = ErrToStatus(err)
	}

	var buf bytes.Buffer
	if encErr := json.NewEncoder(&buf).Encode(jsonError{err.Error()}); encErr != nil {
		log.WithError(encErr).Error("Failed to encode error response")
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, "Failed to encode error response")
		return
	}

	w.WriteHeader(statusCode)
	if _, writeErr := buf.WriteTo(w); writeErr != nil {
		log.WithError(writeErr).Error("Failed to write error response")
	}
}

// ParseJSONBody attempts to parse the request body as JSON
// Does not enforce unknown fields to allow forward compatibility with newer Docker API versions
func ParseJSONBody(v interface{}, w http.ResponseWriter, r *http.Request) error {
	d := json.NewDecoder(r.Body)
	if err := d.Decode(v); err != nil {
		JSONErrResponse(w, fmt.Errorf("failed to parse request body: %w", err), http.StatusBadRequest)
		return err
	}

	return nil
}
