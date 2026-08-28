# template

Template repository for `ctrl-research` projects.

## What's Included

- **Renovate** — automated dependency updates for Docker, Go modules, and GitHub Actions
- **Branch protection** — `main` requires PRs and review
- **CODEOWNERS** — `@ctrl-research/reviewers` auto-requested for review
- **MIT License**
- **.gitignore** — common exclusions for OS, IDE, build outputs, and secrets
- **.tool-versions** — single source of truth for language/tool versions (asdf/mise compatible)

## Using This Template

1. Click **Use this template** to create a new repository
2. Pin your project's language and tool versions in `.tool-versions`
3. Update `renovate.json` to configure managers and schedules for your project
4. Enable the new repo in the Renovate GitHub App if using hosted Renovate

## Renovate

Dependency updates are managed via Renovate. Configuration is in `renovate.json` and `.github/renovate-config.js`.

Enabled managers:
- `asdf` (keeps `.tool-versions` up to date)
- `docker-compose`
- `github-actions`
- `gomod`

Add or remove managers as needed for your project.

## Files

```
.
├── .github/
│   ├── CODEOWNERS           # Auto-request review from @ctrl-research/reviewers
│   ├── renovate-config.js   # Renovate platform config
│   └── workflows/
│       └── renovate.yaml    # Renovate GitHub Action workflow
├── .gitignore
├── .tool-versions          # Pinned language/tool versions (asdf/mise)
├── CONTRIBUTING.md
├── LICENSE
├── README.md
├── renovate.json           # Renovate settings
└── SECURITY.md
```
