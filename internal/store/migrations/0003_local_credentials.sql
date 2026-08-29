-- Local accounts: a username and password held by this server.
--
-- The OIDC path deliberately avoids ever seeing a password. Local accounts
-- exist because requiring an external identity provider is a real barrier for
-- a self-hosted game, and they make this server the custodian of password
-- hashes -- which is why the columns below carry lockout state as well.

CREATE TABLE local_credentials (
    account_id uuid PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,

    -- The display form. Lookups use the normalised (lower-cased) value, which
    -- is also the identity subject, so allowlisting a username before it is
    -- registered works through the same mechanism as every other provider.
    username text NOT NULL,

    -- A PHC-format Argon2id string, carrying its own parameters so they can be
    -- raised later without invalidating existing hashes.
    password_hash text NOT NULL,

    -- Consecutive failures, reset on any success. Throttling by account rather
    -- than only by address matters because a distributed attack changes
    -- address freely but must still target one account.
    failed_attempts int NOT NULL DEFAULT 0,
    locked_until    timestamptz,

    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- Case-insensitive uniqueness: nobody should be able to register "Admin"
-- alongside "admin".
CREATE UNIQUE INDEX local_credentials_username_unique
    ON local_credentials (lower(username));
