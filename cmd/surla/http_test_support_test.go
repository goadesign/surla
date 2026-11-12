// This file provides authenticated HTTP clients for the CLI journey tests.
package main

import (
	"net/http"
)

// bearerDoer attaches the test server credential to each outgoing request.
type bearerDoer struct {
	// client executes requests against the test server.
	client *http.Client
	// token authenticates the test caller.
	token string
}

// Do attaches bearer authentication before issuing a test request.
func (d *bearerDoer) Do(req *http.Request) (*http.Response, error) {
	req.Header.Set("Authorization", "Bearer "+d.token)
	return d.client.Do(req)
}
