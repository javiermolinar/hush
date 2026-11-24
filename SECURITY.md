# Security policy

## Reporting a vulnerability

Use **Security → Report a vulnerability** on GitHub. If private vulnerability reporting isn't available, contact a maintainer privately to arrange a reporting channel. Please don't open a public issue for an unresolved security problem.

Include the commit or build, affected OS, expected and observed behavior, and the impact. If possible, use a temporary workspace and synthetic data. Remove tokens, session contents, and other personal or proprietary data from examples and logs.

## Supported versions and scope

There are no published releases yet. Reports are triaged against `main`.

Hush runs local tools with the backend user's OS permissions. Workspace paths, the Unix socket, and prompt instructions are not a sandbox. Issues that bypass a documented trust, authorization, or isolation check are in scope.
