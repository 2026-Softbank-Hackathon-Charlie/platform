// Package apiv1 holds the platform API types, generated from api/openapi.yaml.
//
// The CLI and other Go clients import this package so that every client and
// the server share one contract. Do not edit apiv1.gen.go by hand: change
// api/openapi.yaml and run `go generate ./pkg/apiv1`.
package apiv1

//go:generate go tool oapi-codegen -config oapi-codegen.yaml ../../api/openapi.yaml
