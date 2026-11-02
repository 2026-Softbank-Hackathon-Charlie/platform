// Package api embeds the OpenAPI document so the server can serve it and
// tests can validate responses against it.
package api

import _ "embed"

// OpenAPI is api/openapi.yaml.
//
//go:embed openapi.yaml
var OpenAPI []byte
