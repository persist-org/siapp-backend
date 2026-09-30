// Package api embeds the OpenAPI contract so tests can validate against it.
package api

import _ "embed"

// OpenAPI is the raw api/openapi.yaml.
//
//go:embed openapi.yaml
var OpenAPI []byte
