# Contributing

## Getting Started

1. Fork the repository
2. Clone your fork
3. Install the pinned tool versions from `.tool-versions`: `asdf install` (or `mise install`)
4. Create a branch: `git checkout -b feat/your-feature-name`
4. Make your changes
5. Commit using [conventional commits](#commit-style)
6. Push and open a Pull Request

## Branch Naming

Branches should follow: `feat|bug|hotfix|release|chore/brief-3-5-word-description`

Examples:
- `feat/add-user-auth`
- `bug/fix-login-crash`
- `hotfix/security-patch`
- `chore/update-dependencies`

## Commit Style

Use [Conventional Commits](https://www.conventionalcommits.org/):

```
<type>(<scope>): <description>

[optional body]

[optional footer]
```

Types: `feat`, `fix`, `chore`, `docs`, `test`, `refactor`, `perf`, `ci`

Examples:
- `feat(auth): add OAuth2 login support`
- `fix(api): correct response status code for 404`
- `chore(deps): update go.mod dependencies`

## Versioning

Project artifacts (releases, tags, packages, container images) follow [Semantic Versioning](https://semver.org/):

- Format is bare `X.Y.Z` — **no prefix** (`1.4.2`, not `v1.4.2`)
- **MAJOR** — breaking changes
- **MINOR** — backwards-compatible features
- **PATCH** — backwards-compatible fixes

## Pull Requests

- Fill out the PR template completely
- Link any related issues
- Ensure CI passes before requesting review
- Maintainers will review within 48 hours

## Code Standards

- Write clear, commented code
- Add tests for new functionality
- Update documentation for user-facing changes
