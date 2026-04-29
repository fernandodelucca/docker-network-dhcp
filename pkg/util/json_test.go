package util

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestJSONResponse_HappyPath(t *testing.T) {
	rec := httptest.NewRecorder()
	JSONResponse(rec, map[string]string{"hello": "world"}, http.StatusCreated)

	if got := rec.Code; got != http.StatusCreated {
		t.Errorf("status = %d, want %d", got, http.StatusCreated)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	var decoded map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("body is not valid JSON: %v (raw=%s)", err, rec.Body.String())
	}
	if decoded["hello"] != "world" {
		t.Errorf("body = %v, want hello=world", decoded)
	}
}

func TestJSONResponse_EmptyStruct(t *testing.T) {
	rec := httptest.NewRecorder()
	JSONResponse(rec, struct{}{}, http.StatusOK)
	if got := strings.TrimSpace(rec.Body.String()); got != "{}" {
		t.Errorf("empty struct body = %q, want {}", got)
	}
}

func TestJSONErrResponse_PopulatesErrField(t *testing.T) {
	rec := httptest.NewRecorder()
	JSONErrResponse(rec, errors.New("kaboom"), 0)

	if got := rec.Code; got != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d (default for unknown errors)", got, http.StatusInternalServerError)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/problem+json" {
		t.Errorf("Content-Type = %q, want application/problem+json", got)
	}
	var decoded struct {
		Err string `json:"Err"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("body not valid JSON: %v", err)
	}
	if decoded.Err != "kaboom" {
		t.Errorf("Err field = %q, want %q", decoded.Err, "kaboom")
	}
}

func TestJSONErrResponse_UsesProvidedStatus(t *testing.T) {
	rec := httptest.NewRecorder()
	JSONErrResponse(rec, errors.New("bad input"), http.StatusBadRequest)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestJSONErrResponse_AutoStatusFromKnownError(t *testing.T) {
	rec := httptest.NewRecorder()
	JSONErrResponse(rec, ErrInvalidMode, 0)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("ErrInvalidMode → status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestParseJSONBody_HappyPath(t *testing.T) {
	body := `{"name":"test","count":42}`
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	rec := httptest.NewRecorder()

	var out struct {
		Name  string `json:"name"`
		Count int    `json:"count"`
	}
	if err := ParseJSONBody(&out, rec, req); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Name != "test" || out.Count != 42 {
		t.Errorf("decoded = %+v, want {Name:test Count:42}", out)
	}
	if rec.Code != http.StatusOK {
		// Default recorder code is 200 when WriteHeader not called.
		t.Errorf("response writer was unexpectedly written to (code=%d)", rec.Code)
	}
}

func TestParseJSONBody_AcceptsUnknownFields(t *testing.T) {
	// Intentionally permissive: a newer Docker daemon may add fields we don't
	// know about, and rejecting them would break compatibility.
	body := `{"name":"x","futureField":"some new option"}`
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	rec := httptest.NewRecorder()
	var out struct {
		Name string `json:"name"`
	}
	if err := ParseJSONBody(&out, rec, req); err != nil {
		t.Errorf("expected unknown fields to be tolerated, got error: %v", err)
	}
}

func TestParseJSONBody_InvalidJSONReturnsErrorAndWritesResponse(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("not json"))
	rec := httptest.NewRecorder()
	var out struct{}
	err := ParseJSONBody(&out, rec, req)
	if err == nil {
		t.Fatal("expected error on invalid JSON, got nil")
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d on parse failure", rec.Code, http.StatusBadRequest)
	}
}

// readerErr is an io.Reader that always errors. Used to simulate a body that
// cannot be read (e.g. closed connection mid-request).
type readerErr struct{}

func (readerErr) Read([]byte) (int, error) { return 0, errors.New("read failure") }

func TestParseJSONBody_ReadFailureReturnsError(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", io.NopCloser(readerErr{}))
	rec := httptest.NewRecorder()
	var out struct{}
	if err := ParseJSONBody(&out, rec, req); err == nil {
		t.Error("expected read error, got nil")
	}
}

// Verify JSON encoding is byte-stable so callers can rely on response shape.
func TestJSONResponse_DeterministicEncoding(t *testing.T) {
	rec1 := httptest.NewRecorder()
	rec2 := httptest.NewRecorder()
	v := map[string]int{"a": 1}
	JSONResponse(rec1, v, http.StatusOK)
	JSONResponse(rec2, v, http.StatusOK)
	if !bytes.Equal(rec1.Body.Bytes(), rec2.Body.Bytes()) {
		t.Errorf("JSONResponse output not deterministic:\n  %q\n  %q", rec1.Body.String(), rec2.Body.String())
	}
}
