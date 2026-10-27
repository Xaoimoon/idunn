# Iðunn

Iðunn keeps Docker Swarm services on the latest image published under their tag. It checks registries with manifest `HEAD` requests (no pull, no rate-limit cost when nothing changed), updates services through the Swarm API so that each service's `update_config` applies, follows the update to its end, rolls back on failure and reports the outcome through [Shoutrrr](https://github.com/nicholas-fedor/shoutrrr) (Discord, Slack, Telegram, e-mail, ntfy, Gotify, generic webhooks…).

It brings together what [Watchtower](https://github.com/nicholas-fedor/watchtower) does well for standalone containers (built-in notifications, rich settings, cheap detection) and what makes [Shepherd](https://github.com/containrrr/shepherd) safe on Swarm (native service updates, rollback). For standalone containers and Compose projects, keep using Watchtower.

In Norse mythology, Iðunn keeps the apples that keep the gods young.

## Features

- Opt-in per service with a label (`idunn.enable=true` by default; key and value are configurable).
- Cron schedule (hourly by default) or fixed interval, blackout windows (`01:30-05:30,Sat 22:00-23:59`).
- One `HEAD` per image per pass, registry tokens reused until they expire, a `429` stops checks to that registry for the rest of the pass.
- Maturity delay: a new digest is only applied after it has been seen for a given time; the count restarts if the tag moves again.
- Cluster guard: no pass while a node is not `Ready` and `Active` (maintenance, reboot).
- Native updates to `tag@digest`, with the registry credentials forwarded to the nodes; `update_config` (order, parallelism, monitor, failure action) and healthchecks are honoured.
- Update tracking until `completed`, `paused` or a timeout, then rollback (`previous` spec) if enabled.
- Cleanup of the old images of updated repositories on every node, through a short-lived global job.
- One report per pass, only when something happened; built-in English and French templates, or your own Go template.
- Monitor-only (global or per service), dry-run, run-once.
- Highly available: deployed globally on the managers, only the Raft leader acts, and leadership is checked again before each service.
- Small and locked down: static binary on a distroless image (~28 MB), non-root, read-only file system, no listening port, stateless.

## How a pass works

1. The pass stops right away if this instance is not the Raft leader, if the time falls in a blackout window, or if a node is unavailable (notified once while it lasts).
2. Services carrying the filter label are listed; services carrying the ignore label, and Iðunn itself, are left out.
3. The reference image is the `com.docker.stack.image` label set by `docker stack deploy` (the image as written in the stack file), else the spec image without its digest. **A reference image pinned by digest is never updated.**
4. The tag's digest is fetched with a `HEAD` request (for multi-arch images, the index digest, which is what Swarm pins) and compared with the digest in the service spec.
5. A service whose update is already running is skipped. A new digest waits for the maturity delay, then, unless the service is in monitor-only or dry-run mode, the spec is updated to `tag@digest`.
6. Iðunn polls `UpdateStatus` every 5 seconds until `completed`, `rollback_completed`, `paused` or the timeout. On `paused` or timeout, it asks Swarm to roll back (if enabled).
7. If at least one service changed image, a global job removes the old images of those repositories on every node.
8. The report is sent.

Iðunn only changes the digest of running services. When you redeploy a stack, Swarm resolves the tag again and gets the same (or a newer) digest: there is no conflict with GitOps tools that deploy stack files.

## Quick start

1. Find the group owning the Docker socket on your managers: `stat -c %g /var/run/docker.sock`. It must be the same on every manager.
2. Optionally store notification URLs as a secret: `printf 'discord://token@webhookid' | docker secret create idunn_notification_url -`
3. Adapt [examples/stack.yml](examples/stack.yml) (image, socket group) and deploy it: `docker stack deploy -c stack.yml idunn`.
4. Add `idunn.enable: "true"` to the `deploy.labels` of the services to keep up to date.

Start with `IDUNN_MONITOR_ONLY=true` for a few days to see what would be updated.

## Configuration

All settings are environment variables. Variables marked *secret* also accept a `_FILE` variant that reads the value from a file, typically a Swarm secret.

| Variable | Default | Description |
| --- | --- | --- |
| `IDUNN_SCHEDULE` | `0 0 * * * *` | Cron expression with seconds (6 fields). Mutually exclusive with `IDUNN_INTERVAL`. |
| `IDUNN_INTERVAL` | | Fixed interval (e.g. `6h`), instead of the cron schedule. |
| `TZ` | `UTC` | Time zone of the schedule, the blackout windows and the timestamps. |
| `IDUNN_BLACKOUT` | | Comma-separated windows with no update: `HH:MM-HH:MM`, optionally prefixed by a day (`Sat 22:00-23:59`). A window may cross midnight. |
| `IDUNN_CLUSTER_GUARD` | `true` | Postpone the pass while a node is not `Ready` and `Active`. |
| `IDUNN_LABEL_FILTER` | `idunn.enable=true` | Label that opts a service in. |
| `IDUNN_LABEL_IGNORE` | `idunn.enable=false` | Label that opts a service out; wins over the filter. |
| `IDUNN_MIN_AGE` | `0` | Maturity delay of a new image (e.g. `6h`); `0` disables it. |
| `IDUNN_MONITOR_ONLY` | `false` | Detect and notify, never update. |
| `IDUNN_DRY_RUN` | `false` | Log what would be done, without calling any write API and without notifying new images. |
| `IDUNN_RUN_ONCE` | `false` | Run one pass immediately, then exit. |
| `IDUNN_ROLLBACK_ON_FAILURE` | `true` | Roll back when an update is paused or times out. |
| `IDUNN_UPDATE_TIMEOUT` | `10m` | Maximum duration of a service update. |
| `IDUNN_MAX_CONCURRENT` | `1` | Services updated in parallel. |
| `IDUNN_CLEANUP` | `true` | Remove old images after updates. |
| `IDUNN_CLEANUP_KEEP` | `1` | Unused images kept per repository and per node, besides the one in use. |
| `IDUNN_REGISTRY_AUTH_FILE` | | Docker `config.json` with registry credentials (use a secret). |
| `IDUNN_HEAD_FAILURE_WARN` | `auto` | Notify failed `HEAD` checks: `never`, `always`, or `auto` (only for registries known to support `HEAD`; others are logged). |
| `IDUNN_NOTIFICATION_URL` | | *Secret.* One or more Shoutrrr URLs, separated by spaces. |
| `IDUNN_NOTIFICATION_LANG` | `en` | Language of the built-in template: `en` or `fr`. |
| `IDUNN_NOTIFICATION_TEMPLATE` | built-in | Go `text/template` replacing the built-in one (`_FILE` accepted). |
| `IDUNN_NOTIFICATION_REPORT` | `true` | One report per pass (`true`) or one message per event (`false`). |
| `IDUNN_NOTIFICATION_LEVEL` | `info` | Minimum level notified: `info`, `warn`, `error`. |
| `IDUNN_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error`. |
| `IDUNN_LOG_FORMAT` | `text` | `text` or `json`. |

### Service labels

Set them in `deploy.labels`; they override the global settings for that service.

| Label | Effect |
| --- | --- |
| `idunn.enable=true` / `false` | Opts the service in or out (name set by `IDUNN_LABEL_FILTER` / `IDUNN_LABEL_IGNORE`). |
| `idunn.monitor-only=true` / `false` | Forces or lifts monitor-only mode for this service. |
| `idunn.min-age=<duration>` | Maturity delay for this service. |
| `idunn.timeout=<duration>` | Update timeout for this service (e.g. `20m` for a heavy image). |
| `idunn.rollback=false` | Disables the rollback for this service. |

## Notifications

A report is sent after a pass only if at least one event reaches `IDUNN_NOTIFICATION_LEVEL`: a pass without changes is silent.

| Event | Level | Notes |
| --- | --- | --- |
| Startup | info | Sent by the leader only. |
| Service updated | info | Old and new digest, duration. |
| New image waiting for maturity | info | Once per digest, with the time it will be applied after. |
| New image detected (monitor-only) | info | Once per digest. |
| Rolled back | warn | Swarm state and the task error. |
| Pass postponed, node unavailable | warn | Once while the situation lasts. |
| Image check failed | warn | Registry and HTTP code; once until the check succeeds again. |
| Cleanup failed on a node | warn | |
| Update paused | error | Rollback disabled; manual action required. |
| Rollback failed | error | Manual action required. |

Custom templates receive `.Events`, a list of events with the fields `Kind` (`startup`, `updated`, `pending`, `detected`, `rollback`, `rollback_failed`, `paused`, `guard`, `head_failed`, `cleanup_failed`), `Level`, `Time`, `Service`, `Image`, `OldDigest`, `NewDigest`, `Duration`, `ApplyAt`, `State`, `Message`, `Registry`, `StatusCode`, `Nodes`, and for startup `Version`, `Node`, `Schedule`, `Blackout`, `Tracked`, `Mode`. Helpers: `short` (12-character digest), `dur`, `clock`, `join`. See [the built-in templates](internal/notify/templates).

## Private registries

Mount a Docker `config.json` as a secret and point `IDUNN_REGISTRY_AUTH_FILE` at it. Iðunn uses it for its `HEAD` requests and forwards the matching credentials with each service update, so that every node can pull the image. Only static credentials (`auth`, `username`/`password`, `identitytoken`) are supported, not credential helpers.

Registry mirrors configured on the Docker daemons keep serving the pulls: Iðunn only talks to the registry for `HEAD` requests.

## Security

Iðunn needs the Docker socket of a manager, which grants full control of the cluster. Its code is kept small and its API use narrow:

- Read: `Info`, `NodeInspect`, `NodeList`, `ServiceList`, `ServiceInspect`, `TaskList`, and `ContainerInspect` on its own container (to identify its service and image).
- Write: `ServiceUpdate` on the selected services, and `ServiceCreate` / `ServiceRemove` for the cleanup job only.
- The cleanup job runs Iðunn's own image (`idunn cleanup`) with the same user, and only calls `ImageList`, `ContainerList` and `ImageRemove` on its node. Images are never force-removed: an image still referenced by a stopped container (Swarm task history) stays until those containers are pruned.
- No container creation, no exec, no listening port.
- Secrets are only read from files or environment variables, and are masked in logs, notifications and errors.

The image runs as user `65532`; give it the group owning the socket (`user: "65532:<gid>"`). Use `read_only: true`.

## Healthcheck

`idunn health` checks that the Docker API answers. It is the image's `HEALTHCHECK`, so a stack needs no healthcheck of its own.

## Building and testing

```sh
go test ./...                    # unit tests
sh scripts/integration.sh        # integration tests in a disposable docker:dind Swarm
docker build --build-arg VERSION=dev -t idunn:dev .
```

The integration script starts a privileged `docker:dind` container with its own engine, a one-node Swarm and a local registry, and removes it at the end. It covers an update under the same tag, a failing healthcheck leading to a rollback, pinned images, monitor-only, dry-run and cleanup.

## License

[Apache License 2.0](LICENSE).
