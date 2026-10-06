CREATE TABLE IF NOT EXISTS xianyu_order_claims (
    order_no VARCHAR(64) PRIMARY KEY,
    redeem_code_id BIGINT NOT NULL UNIQUE,
    account_id VARCHAR(80) NOT NULL,
    item_id VARCHAR(64) NOT NULL,
    buyer_id VARCHAR(80) NOT NULL,
    amount NUMERIC(20,2) NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

ALTER TABLE xianyu_order_claims
    DROP CONSTRAINT IF EXISTS fk_xianyu_order_claims_redeem_code;

ALTER TABLE xianyu_order_claims
    ADD CONSTRAINT fk_xianyu_order_claims_redeem_code
    FOREIGN KEY (redeem_code_id)
    REFERENCES redeem_codes(id)
    ON DELETE RESTRICT;

CREATE INDEX IF NOT EXISTS idx_xianyu_order_claims_code
    ON xianyu_order_claims(redeem_code_id);
