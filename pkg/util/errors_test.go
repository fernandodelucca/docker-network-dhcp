package util

import (
	"errors"
	"fmt"
	"net/http"
	"testing"
)

func TestErrToStatus(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"IPAM is client error", ErrIPAM, http.StatusBadRequest},
		{"BridgeRequired is client error", ErrBridgeRequired, http.StatusBadRequest},
		{"NotBridge is client error", ErrNotBridge, http.StatusBadRequest},
		{"BridgeUsed is client error", ErrBridgeUsed, http.StatusBadRequest},
		{"MACAddress is client error", ErrMACAddress, http.StatusBadRequest},
		{"InvalidMode is client error", ErrInvalidMode, http.StatusBadRequest},
		{"NoLease is server-side", ErrNoLease, http.StatusInternalServerError},
		{"NoHint is server-side", ErrNoHint, http.StatusInternalServerError},
		{"NotVEth is server-side", ErrNotVEth, http.StatusInternalServerError},
		{"NoContainer is server-side", ErrNoContainer, http.StatusInternalServerError},
		{"NoSandbox is server-side", ErrNoSandbox, http.StatusInternalServerError},
		{"unknown error defaults to 500", errors.New("random"), http.StatusInternalServerError},
		{"wrapped client error keeps 400", fmt.Errorf("context: %w", ErrInvalidMode), http.StatusBadRequest},
		{"wrapped server error keeps 500", fmt.Errorf("context: %w", ErrNoLease), http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ErrToStatus(tc.err); got != tc.want {
				t.Errorf("ErrToStatus(%v) = %d, want %d", tc.err, got, tc.want)
			}
		})
	}
}
