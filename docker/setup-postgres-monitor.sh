#!/bin/sh
set -eu

psql -X --set=ON_ERROR_STOP=1 --single-transaction --file=- <<'SQL'
\getenv monitor_password POSTGRES_EXPORTER_PASSWORD
SELECT 'CREATE ROLE reader_monitor'
WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'reader_monitor')
\gexec
ALTER ROLE reader_monitor LOGIN PASSWORD :'monitor_password';
GRANT CONNECT ON DATABASE reader TO reader_monitor;
GRANT pg_monitor TO reader_monitor;
SQL
