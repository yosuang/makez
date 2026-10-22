-- PostgreSQL runs this once, only when its data directory is empty.
-- Development-only accounts; each service owns only its own databases.
CREATE ROLE makez LOGIN PASSWORD 'makez-local';
CREATE ROLE temporal LOGIN PASSWORD 'temporal-local';
CREATE ROLE opensandbox LOGIN PASSWORD 'opensandbox-local';

CREATE DATABASE makez OWNER makez;
CREATE DATABASE temporal OWNER temporal;
CREATE DATABASE temporal_visibility OWNER temporal;
CREATE DATABASE opensandbox OWNER opensandbox;

REVOKE CONNECT, TEMPORARY ON DATABASE makez FROM PUBLIC;
REVOKE CONNECT, TEMPORARY ON DATABASE temporal FROM PUBLIC;
REVOKE CONNECT, TEMPORARY ON DATABASE temporal_visibility FROM PUBLIC;
REVOKE CONNECT, TEMPORARY ON DATABASE opensandbox FROM PUBLIC;
