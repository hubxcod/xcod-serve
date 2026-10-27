# Contributing to xcod

Thanks for your interest in helping! Bug reports, ideas and pull requests are all welcome.

## Reporting a bug

Open an [issue](https://github.com/hubxcod/xcod-serve/issues/new/choose) and include:
- the xcod version (shown under **Server** in the panel, or `/opt/xcod/xcod -version`);
- your Linux distribution;
- what you did, what you expected and what happened;
- relevant log lines from `journalctl --namespace=xcod -n 100`.

Remove passwords, tokens and provider URLs before posting.

## Security issues

Do **not** open a public issue. Report it privately through
[GitHub Security Advisories](https://github.com/hubxcod/xcod-serve/security/advisories/new).

## Suggesting a feature

Open an issue describing the problem you want to solve. For larger changes, please discuss them in an issue before writing code.

## Submitting code

1. Fork the repository and create a branch from `main`.
2. Make your change and add tests when possible.
3. Check that everything passes:
   ```sh
   gofmt -l .        # must print nothing
   go vet ./...
   go test ./...
   ```
4. Open a pull request explaining what changed and why.

By submitting a pull request, you agree that your contribution is licensed under the [AGPL-3.0](LICENSE), the same license as the project.

### Development setup

You need Go 1.26 or newer. ffmpeg is optional and only needed for transcoding.

```sh
go run ./cmd/xcod     # panel at http://localhost:8090/admin
```

On the first run, the admin password is printed in the log. Data is stored in `./data`.

### Guidelines

- Keep it simple: standard library first, new dependencies only when really needed.
- Follow the style of the surrounding code. Code comments, messages and panel text are in English.
- No inline scripts or styles in the panel, because the CSP blocks them.
