package api

// Only models are generated (generate: models in oapi-codegen.yaml), never a server or
// router. Routes stay hand-mounted on chi so that the default-deny authentication
// middleware and each route's permission check stay explicit and visible where the route
// is declared, while request and response types can never drift from the contract.
//
// `make contract` copies the contract from persist-ecosystem and runs this generator;
// `make check` fails if either result differs from what is committed.
//
//go:generate oapi-codegen -config oapi-codegen.yaml openapi.yaml
