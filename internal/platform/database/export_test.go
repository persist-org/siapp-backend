package database

// WithTxRaw exposes withTx to the external tests, which need raw SQL on the transaction
// because the platform has no sqlc write queries to exercise WithTx with.
var WithTxRaw = withTx
