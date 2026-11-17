#!/bin/sh
set -eu
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" \
  --set=feeds_password="$FEEDS_PASSWORD" --set=articles_password="$ARTICLES_PASSWORD" <<'SQL'
CREATE ROLE feeds_app LOGIN PASSWORD :'feeds_password';
CREATE ROLE articles_app LOGIN PASSWORD :'articles_password';
REVOKE CREATE ON SCHEMA public FROM PUBLIC;
SQL
