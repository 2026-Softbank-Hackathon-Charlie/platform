-- Image scans the platform's scan workflow posts (SEC-04, ADR 0034).
--
-- One row per image digest: its latest scan. doc is the apiv1.SecurityImageScan
-- JSON and vulnerabilities the apiv1.SecurityVulnerability array the platform
-- normalised from the scanner's report; report is that original report,
-- gzip-compressed, whose sha256 doc carries. A scan replaces the row only when
-- it finished no earlier than the stored one, so a late retry cannot hide a
-- newer result. No foreign keys, like the other tables.

CREATE TABLE image_scans (
    digest          text PRIMARY KEY,
    id              text NOT NULL UNIQUE,
    status          text NOT NULL,
    finished_at     timestamptz NOT NULL,
    doc             jsonb NOT NULL,
    vulnerabilities jsonb NOT NULL,
    report          bytea,
    created_at      timestamptz NOT NULL,
    CHECK (digest <> '' AND id <> '' AND status <> '')
);

CREATE INDEX image_scans_finished_at ON image_scans (finished_at);
