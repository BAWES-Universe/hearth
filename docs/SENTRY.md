# Sentry integration — Hearth

Sentry gives us **error tracking + release/commit association** (which merged commit
broke prod). It does **not** do PR code review — that's CodeRabbit (`.coderabbit.yaml`).
They complement each other: CodeRabbit reviews the code in the PR; Sentry links
deployed releases back to the commits that shipped them.

## One-time setup (owner/admin)

1. **Create projects** in Sentry: e.g. `hearth-server` (Go) and `hearth-client` (JS).
2. **Install the "Sentry for GitHub" app** on `BAWES-Universe/hearth` — this attaches
   commits to releases and links issues mentioned in commits/PRs (the "PR review"
   attachment: Sentry comments on PRs when the touched code has open issues in prod).
3. **Auth token**: Sentry → Settings → Auth Tokens → create (scopes
   `project:releases`, `org:read`) → add as a GitHub Actions secret named
   `SENTRY_AUTH_TOKEN`. Never commit it (`.gitignore` already blocks `*.pat`, `.env`).
4. **DSNs**: server reads `SENTRY_DSN` from env; client reads `VITE_SENTRY_DSN`
   at build time. Add both as GitHub Actions secrets/vars when enabling.

## Env vars

| Var | Where | Purpose |
|-----|-------|---------|
| `SENTRY_AUTH_TOKEN` | GH Actions secret | sentry-cli auth for release/sourcemap uploads |
| `SENTRY_DSN` | server env | Go SDK ingest endpoint |
| `VITE_SENTRY_DSN` | client build env | browser SDK ingest endpoint |
| `SENTRY_ORG` / `SENTRY_PROJECT` | env | override `.sentryclirc` defaults |

`.sentryclirc` (repo root) holds only defaults — org/project/url placeholders, no secrets.

## CI wiring (add once CI is green — `.github/workflows/ci.yml`)

1. New `sentry-release` job on push to `main`:

   ```yaml
   sentry-release:
     runs-on: ubuntu-latest
     needs: [server, media, client]
     if: github.ref == 'refs/heads/main'
     env:
       SENTRY_AUTH_TOKEN: ${{ secrets.SENTRY_AUTH_TOKEN }}
       SENTRY_ORG: hearth-org
       SENTRY_PROJECT: hearth-server
     steps:
       - uses: actions/checkout@v4
       - run: curl -sL https://sentry.io/get-cli/ | bash
       - run: sentry-cli releases new "hearth@$GITHUB_SHA" --org "$SENTRY_ORG" --project "$SENTRY_PROJECT"
       - run: sentry-cli releases set-commits "hearth@$GITHUB_SHA" --auto
   ```

2. **Client source maps** (after `npm run build`): upload
   `client/dist/assets` with
   `sentry-cli sourcemaps upload --org "$SENTRY_ORG" --project hearth-client --release "hearth@$GITHUB_SHA" client/dist/assets`.
3. **Deploys**: `sentry-cli releases deploys "hearth@$GITHUB_SHA" new -e production`.

## Health check

`sentry-cli info` prints auth/org/project status. If `SENTRY_AUTH_TOKEN` is unset,
sentry-cli fails loudly — that is expected until setup step 3 is done.
