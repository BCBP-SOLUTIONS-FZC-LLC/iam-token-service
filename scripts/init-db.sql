-- Local-dev bootstrap for the `serviceaccount` database and its three DB
-- roles (§4.3, §4.4). Mirrors iam-realm-provisioner's scripts/init-db.sql
-- convention — production provisions these via Terraform; the domain
-- migration
-- (internal/adapter/outbound/postgres/migrations/000001_schema.up.sql)
-- only ALTERs/GRANTs, it never CREATE ROLEs (it just tolerates their
-- absence with a NOTICE, so migrations still apply cleanly against a
-- fresh Postgres with no roles pre-created at all).
--
-- This script runs (docker-entrypoint-initdb.d convention) connected AS
-- POSTGRES_USER — docker-compose.yml deliberately sets that to the neutral
-- "postgres" superuser, NOT serviceaccount_app: Postgres refuses to let the
-- bootstrap/connecting superuser strip its own SUPERUSER attribute
-- ("permission denied to alter role — the bootstrap user must have the
-- SUPERUSER attribute", confirmed empirically), so if POSTGRES_USER were
-- serviceaccount_app instead, this script could create the role but could
-- never actually demote it — it would stay a real superuser forever,
-- silently bypassing RLS-6/RLS-7 for every local-dev run regardless of the
-- NOBYPASSRLS attribute requested below. Keeping the bootstrap identity
-- separate from every app-facing role avoids that trap entirely.
--
-- serviceaccount_app          — runtime app role (cmd/server, cmd/consumer).
--                                NOBYPASSRLS, NOSUPERUSER — RLS-6/RLS-7
--                                actually enforced.
-- serviceaccount_migrator     — runs the migration. BYPASSRLS + DDL.
-- serviceaccount_reconciler   — cmd/rotator's cross-tenant enumeration
--                                pool. BYPASSRLS, SELECT-only (no write
--                                grant — every write the rotator performs
--                                goes through a separate serviceaccount_app
--                                connection scoped to that row's own tenant).

DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'serviceaccount_app') THEN
    CREATE ROLE serviceaccount_app LOGIN PASSWORD 'devpassword' NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'serviceaccount_migrator') THEN
    CREATE ROLE serviceaccount_migrator LOGIN PASSWORD 'devpassword' BYPASSRLS;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'serviceaccount_reconciler') THEN
    CREATE ROLE serviceaccount_reconciler LOGIN PASSWORD 'devpassword' BYPASSRLS;
  END IF;
END
$$;

SELECT 'CREATE DATABASE serviceaccount OWNER serviceaccount_migrator'
WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = 'serviceaccount')\gexec

GRANT ALL PRIVILEGES ON DATABASE serviceaccount TO serviceaccount_migrator;
GRANT CONNECT ON DATABASE serviceaccount TO serviceaccount_app;
GRANT CONNECT ON DATABASE serviceaccount TO serviceaccount_reconciler;

\c serviceaccount

GRANT USAGE, CREATE ON SCHEMA public TO serviceaccount_migrator;
GRANT USAGE ON SCHEMA public TO serviceaccount_app;
GRANT USAGE ON SCHEMA public TO serviceaccount_reconciler;
CREATE EXTENSION IF NOT EXISTS pgcrypto;
