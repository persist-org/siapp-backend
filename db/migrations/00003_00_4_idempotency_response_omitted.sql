-- +goose Up

-- A 2xx response that cannot be kept (not JSON, or larger than the middleware stores) still
-- completes its key: the handler's side effects happened, so a retry must not run it again.
-- Such a row has its status_code, a NULL response_body and response_omitted true; a replay
-- answers 409 CONFLICT, reason IDEMPOTENCY_RESPONSE_NOT_STORED. See pkg/platform/idem.
ALTER TABLE idempotency_keys ADD COLUMN response_omitted BOOLEAN NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE idempotency_keys DROP COLUMN response_omitted;
