# V1 Deployment and Operations Findings

## Context

This document records user-visible deployment, release, and documentation findings identified after the V1 implementation.

It also records the current deployment requirements confirmed after implementation review.

## Findings

### F1. The default Compose file is not suitable as a local clone template

Current repository state uses a committed Compose file as the active local deployment file.

The desired clone workflow is:

    compose.yaml.example
        ↓
    user copies it to compose.yaml
        ↓
    user changes local deployment values
        ↓
    docker compose uses compose.yaml

`compose.yaml` is local deployment state and must not be tracked by Git.

The committed example file is `compose.yaml.example`.


### F2. `/data` is persistent internal application state, not a user working directory

The application stores at least these durable states under `/data`:

- Login Session;
- Terminal State.

These states must survive container recreation.

They are application-internal state and are not normal files that the user needs to inspect or edit.

The default Compose deployment should persist `/data` with a Docker named volume rather than expose it as a repository-local `./data` directory.

A local `/data/` directory can still exist during development or in a customized deployment and must remain outside Git.


### F3. Git ignore rules do not protect all local deployment state

Current `.gitignore` does not protect the runtime `data` directory.

The runtime directory can contain the complete persisted WeRead Cookie set.

The local deployment model also requires an untracked `compose.yaml`.

The repository ignore rules therefore need to cover these local paths:

    /compose.yaml
    /data/

The existing secret environment-file rules remain applicable.


### F4. The documented GHCR image path is not the normal clone-user image path

Locations:

- `README.md`
- Compose example
- `.github/workflows/release.yml`

The release workflow publishes the repository image under the repository owner and repository name.

The current deployment documentation uses:

    ghcr.io/OWNER/weread-cron

and tells a normal user to replace `OWNER` with their own GitHub account.

A user who only clones this repository does not have an image at their own GHCR namespace.

The normal deployment path must identify the image published by this repository.

Fork-and-publish behavior is a separate case.


### F5. `latest` release behavior can differ from the documented tag model

Location:

- `.github/workflows/release.yml`

The workflow contains explicit logic intended to control when `latest` is published.

The Docker metadata configuration can also generate `latest` automatically for version-tag events.

The effective `latest` behavior can therefore differ from the comments and documented release model.

A version-tag build can update `latest` even when the intended model treats `latest` as the main-branch image.


### F6. README does not cover the complete Compose CLI workflow

Location:

- `README.md`

The README documents daemon startup and manual `run` use.

The `books` command does not have the same complete Compose invocation examples.

A first-time user can also need `books` before the daemon is already running.

The deployment documentation does not currently give a complete image update workflow such as pull and recreate.

The README must remain consistent with the `compose.yaml.example` → local `compose.yaml` deployment model.


### F7. Some protocol comments describe stronger reference-project agreement than currently exists

Location:

- protocol implementation comments

One example describes report success behavior as a common behavior of both reference projects.

Current reference implementations differ:

- `weread.koplugin` accepts `succ` success or the presence of `synckey`;
- `wxread` uses a more restrictive progression condition.

The current weread-cron behavior can still be a valid V1 choice.

The issue is the accuracy of the source comment, not a confirmed protocol defect.


### F8. A damaged persisted Login Session has a narrower recovery path than the README can imply

Locations:

- Login Session restore code
- `README.md`

When the persisted Login Session file exists but cannot be decoded or restored, startup fails.

Fallback to `WEREAD_CRON_COOKIE` occurs when no persisted session exists, but not when the existing persisted file is damaged.

This is a conservative failure behavior.

Documentation that describes updating the initial Cookie as a general recovery mechanism must distinguish this case.