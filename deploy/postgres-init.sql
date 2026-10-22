-- Run on every Compose startup, including volumes created by the former dev
-- stack. Do not reset existing passwords, database owners or application data.
-- These service passwords are development-only fixtures, like makez-local.
SELECT 'CREATE ROLE temporal LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE PASSWORD ''temporal-local'''
WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'temporal')
\gexec
SELECT 'CREATE ROLE opensandbox LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE PASSWORD ''opensandbox-local'''
WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'opensandbox')
\gexec

SELECT format('CREATE DATABASE %I OWNER %I', name, owner)
FROM (VALUES ('makez', 'makez'), ('temporal', 'temporal'),
             ('temporal_visibility', 'temporal'), ('opensandbox', 'opensandbox')) AS databases(name, owner)
WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = name)
\gexec

-- Fail on conflicting pre-existing resources rather than taking ownership or
-- silently changing privileges. The legacy Makez bootstrap role is preserved.
DO $$
BEGIN
    IF EXISTS (
        SELECT FROM pg_roles
        WHERE rolname IN ('temporal', 'opensandbox')
          AND (rolsuper OR rolcreatedb OR rolcreaterole OR NOT rolcanlogin)
    ) THEN
        RAISE EXCEPTION 'Existing infrastructure roles must be non-superuser logins without CREATEDB or CREATEROLE; review them manually';
    END IF;
    IF EXISTS (
        SELECT FROM (VALUES ('temporal', 'temporal'),
                            ('temporal_visibility', 'temporal'),
                            ('opensandbox', 'opensandbox')) AS expected(name, owner)
        JOIN pg_database ON datname = name
        WHERE pg_get_userbyid(datdba) <> owner
    ) THEN
        RAISE EXCEPTION 'Existing infrastructure databases have unexpected owners; review them manually';
    END IF;
END
$$;

-- Only the owning service (and the existing bootstrap superuser) can connect.
REVOKE CONNECT, TEMPORARY ON DATABASE makez FROM PUBLIC;
REVOKE CONNECT, TEMPORARY ON DATABASE temporal FROM PUBLIC;
REVOKE CONNECT, TEMPORARY ON DATABASE temporal_visibility FROM PUBLIC;
REVOKE CONNECT, TEMPORARY ON DATABASE opensandbox FROM PUBLIC;
