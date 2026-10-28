<p align="center"><img src="assets/idunn-logo.png" alt="Iðunn" width="220"></p>

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
- Update tracking until `completed`, `paused` or a timeout, then rollback (`previous` spec) if enabled. A digest that was rolled back is not retried until a newer one is published.
- Cleanup of the old images of updated repositories on every node, through a short-lived global job.
- One report per pass, only when something happened; built-in English and French templates, or your own Go template.
- Monitor-only (global or per service), dry-run, run-once.
- Optional self-update: Iðunn updates its own service last, after sending its report, and leaves the outcome to Swarm.
- Daily report of newer version tags for services pinned to a version (`mariadb:12.3.3`, `forgejo:16`), which Iðunn never updates by itself.
- Highly available: deployed globally on the managers, only the Raft leader acts, and leadership is checked again before each service.
- Small and locked down: static binary on a distroless image (~28 MB), unprivileged user with the socket group only, read-only file system, no listening port unless metrics are enabled, stateless.
- Optional Prometheus metrics: passes, updates and rollbacks per service, pending images, registry errors, newer versions available.

## How a pass works

1. The pass stops right away if this instance is not the Raft leader, if the time falls in a blackout window, or if a node is unavailable (notified once while it lasts).
2. Services carrying the filter label are listed; services carrying the ignore label, and Iðunn itself, are left out.
3. The reference image is the `com.docker.stack.image` label set by `docker stack deploy` (the image as written in the stack file), else the spec image without its digest. **A reference image pinned by digest is never updated.**
4. The tag's digest is fetched with a `HEAD` request (for multi-arch images, the index digest, which is what Swarm pins) and compared with the digest in the service spec.
5. A service whose update is already running is skipped, and so is a digest whose last update was rolled back (by Iðunn or by the service's own `update_config`), until the tag moves again. A new digest waits for the maturity delay, then, unless the service is in monitor-only or dry-run mode, the spec is updated to `tag@digest`.
6. Iðunn polls `UpdateStatus` every 5 seconds until `completed`, `rollback_completed`, `paused` or the timeout. On `paused` or timeout, it asks Swarm to roll back (if enabled).
7. If at least one service changed image, a global job removes the old images of those repositories on every node.
8. The report is sent.

Iðunn only changes the digest of running services. When you redeploy a stack, Swarm resolves the tag again and gets the same (or a newer) digest: there is no conflict with GitOps tools that deploy stack files.

## Quick start

1. Optionally store notification URLs as a secret: `printf 'discord://token@webhookid' | docker secret create idunn_notification_url -`
2. Adapt [examples/stack.yml](examples/stack.yml) (image path) and deploy it: `docker stack deploy -c stack.yml idunn`.
3. Add `idunn.enable: "true"` to the `deploy.labels` of the services to keep up to date.

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
| `IDUNN_SELF_UPDATE` | `false` | Let Iðunn [update its own service](#self-update). |
| `IDUNN_ROLLBACK_ON_FAILURE` | `true` | Roll back when an update is paused or times out. |
| `IDUNN_UPDATE_TIMEOUT` | `10m` | Maximum duration of a service update. |
| `IDUNN_MAX_CONCURRENT` | `1` | Services updated in parallel. |
| `IDUNN_CLEANUP` | `true` | Remove old images after updates. |
| `IDUNN_CLEANUP_KEEP` | `1` | Unused images kept per repository and per node, besides the one in use. |
| `IDUNN_VERSION_CHECK` | `false` | Enable the [version check](#new-versions-of-pinned-images). |
| `IDUNN_VERSION_SCHEDULE` | `0 0 9 * * *` | Cron expression with seconds for the version check (daily at 09:00 by default). |
| `IDUNN_VERSION_LEVEL` | `patch` | Smallest bump reported by the version check: `patch`, `minor` or `major`. |
| `IDUNN_REGISTRY_AUTH_FILE` | | Docker `config.json` with registry credentials (use a secret). |
| `IDUNN_HEAD_FAILURE_WARN` | `auto` | Notify failed `HEAD` checks: `never`, `always`, or `auto` (only for registries known to support `HEAD`; others are logged). |
| `IDUNN_NOTIFICATION_URL` | | *Secret.* One or more Shoutrrr URLs, separated by spaces. |
| `IDUNN_NOTIFICATION_LANG` | `en` | Language of the built-in template: `en` or `fr`. |
| `IDUNN_NOTIFICATION_TEMPLATE` | built-in | Go `text/template` replacing the built-in one (`_FILE` accepted). |
| `IDUNN_NOTIFICATION_REPORT` | `true` | One report per pass (`true`) or one message per event (`false`). |
| `IDUNN_NOTIFICATION_LEVEL` | `info` | Minimum level notified: `info`, `warn`, `error`. |
| `IDUNN_METRICS_ADDR` | | Address of the [Prometheus metrics](#metrics) endpoint (e.g. `:9640`); empty disables it. The unprivileged user cannot bind ports below 1024. |
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
| `idunn.version-level=<level>` | Smallest bump reported by the version check for this service: `patch`, `minor`, `major`, or `none` to leave it out. |

## Notifications

A report is sent after a pass only if at least one event reaches `IDUNN_NOTIFICATION_LEVEL`: a pass without changes is silent.

| Event | Level | Notes |
| --- | --- | --- |
| Startup | info | Sent by the leader only. |
| Service updated | info | Old and new digest, duration. |
| New image waiting for maturity | info | Once per digest, with the time it will be applied after. |
| New image detected (monitor-only) | info | Once per digest. |
| Rolled back | warn | Swarm state and the task error. |
| Rolled-back image not retried | warn | Once per digest, until a newer image is published. |
| Self-update started | info | In the report sent just before Iðunn's own service is updated. |
| Self-update refused | error | Swarm refused the update; Iðunn keeps running the current version. |
| Pass postponed, node unavailable | warn | Once while the situation lasts. |
| Image check failed | warn | Registry and HTTP code; once until the check succeeds again. |
| Cleanup failed on a node | warn | |
| Newer version available | info | Version check only: one report per check, listing every service with a newer tag. |
| Update paused | error | Rollback disabled; manual action required. |
| Rollback failed | error | Manual action required. |

Custom templates receive `.Events`, a list of events with the fields `Kind` (`startup`, `updated`, `pending`, `detected`, `rollback`, `rollback_failed`, `paused`, `guard`, `head_failed`, `cleanup_failed`, `new_version`, `self_update`, `self_update_failed`, `retry_skipped`), `Level`, `Time`, `Service`, `Image`, `OldDigest`, `NewDigest`, `Duration`, `ApplyAt`, `State`, `Message`, `Registry`, `StatusCode`, `Nodes`, `Tag` and `Bump` (new version), and for startup `Version`, `Node`, `Schedule`, `Blackout`, `Tracked`, `Mode`. Helpers: `short` (12-character digest), `dur`, `clock`, `join`. See [the built-in templates](internal/notify/templates).

## New versions of pinned images

A service pinned to a version (`mariadb:12.3.3`, or a branch such as `forgejo:16` or `haproxy:2.9-alpine`) only receives the rebuilds published under that tag: nothing tells you when `12.3.4`, `13.0.0` or `forgejo:17` comes out. With `IDUNN_VERSION_CHECK=true`, the leader lists, once a day, the tags of each such image and reports the newest one. It never updates anything: you move to the new version by editing the stack file.

- Every service is checked, whatever its opt-in or ignore label, Iðunn included: pinned versions are precisely the ones no automatic update covers. `idunn.version-level=none` leaves a service out.
- Only tags of the same shape are compared: same `v` prefix, same number of components and same suffix. `2.9-alpine` is compared with `3.2-alpine`, never with `3.2` or `3.2-bookworm`, and pre-releases such as `13.0.0-rc1` are left out of a plain `12.3.3`.
- A tag that is not a version (`latest`, `stable`, `alpine`) is skipped.
- The newest tag whose bump reaches the level (`IDUNN_VERSION_LEVEL` or `idunn.version-level`) is reported, with the size of the bump. At `minor`, `12.3.3` → `12.3.4` is ignored but `12.4.0` and `13.0.0` are reported.
- Each check sends one report listing everything available, and repeats it daily until you upgrade. Nothing is kept between restarts.
- One tag listing per repository and per check. Listing tags is not an image pull and does not count against Docker Hub pull limits.
- A listing is abandoned after 2 minutes and the failure is only logged: some registries, such as `docker.elastic.co`, page through an endless list of build tags. A registry that times out or answers `429` is not asked again for the rest of the check. Set `idunn.version-level=none` on those services to silence the warning.

## Self-update

By default Iðunn never touches its own service: you upgrade it by redeploying its stack. With `IDUNN_SELF_UPDATE=true`, it handles its own service like the others (new digest, maturity delay, monitor-only and dry-run modes), with these differences:

- It is checked whatever its opt-in or ignore label, and always last in the pass, after the cleanup.
- The report, including a "self-update started" line, is sent **before** the update, because Swarm stops the running instance to replace it. Iðunn does not follow the update: the new version announces itself with its startup notification.
- A failed start is handled by Swarm, so give Iðunn's service `update_config.failure_action: rollback` and keep its healthcheck. The digest that was rolled back is then not retried.
- The stack file must reference a tag (`idunn:latest`, or a channel tag), not `tag@digest`: an image pinned by digest is never updated.
- Images left behind by previous Iðunn versions are not removed by its own cleanup job.

## Metrics

With `IDUNN_METRICS_ADDR` set, every instance serves `/metrics` in the Prometheus text format. Only the Raft leader runs passes, so scrape all instances and use `idunn_leader` to tell them apart; counters restart from zero when an instance restarts.

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `idunn_build_info` | gauge | `version` | Always 1. |
| `idunn_leader` | gauge | | 1 on the instance that acts. |
| `idunn_passes_total` | counter | `outcome` | `completed`, `standby` (not the leader), `blackout`, `guard`, `error`. |
| `idunn_last_pass_timestamp_seconds` | gauge | | End of the last completed pass. |
| `idunn_last_pass_duration_seconds` | gauge | | Its duration. |
| `idunn_services_tracked` | gauge | | Services selected at the last pass. |
| `idunn_images_pending` | gauge | | Services whose new image waits for the maturity delay. |
| `idunn_updates_total` | counter | `service`, `outcome` | `updated`, `rollback`, `rollback_failed`, `paused`, `self_update`, `retry_skipped`. |
| `idunn_cleanup_failures_total` | counter | | Failed image cleanups. |
| `idunn_registry_errors_total` | counter | `registry`, `code` | Failed `HEAD` or tag listing; code `0` for unreachable or timeout. |
| `idunn_new_version_available` | gauge | `service`, `image`, `latest`, `bump` | 1 per service with a newer version tag, as of the last version check. |
| `idunn_last_version_check_timestamp_seconds` | gauge | | End of the last version check. |

A stale `idunn_last_pass_timestamp_seconds` on the leader (more than two schedule periods outside blackout windows) means passes stopped.

## Private registries

Mount a Docker `config.json` as a secret and point `IDUNN_REGISTRY_AUTH_FILE` at it. Iðunn uses it for its `HEAD` requests and tag listings, and forwards the matching credentials with each service update, so that every node can pull the image. Only static credentials (`auth`, `username`/`password`, `identitytoken`) are supported, not credential helpers.

Registry mirrors configured on the Docker daemons keep serving the pulls: Iðunn only talks to the registry for `HEAD` requests.

## Security

Iðunn needs the Docker socket of a manager, which grants full control of the cluster. Its code is kept small and its API use narrow:

- Read: `Info`, `NodeInspect`, `NodeList`, `ServiceList`, `ServiceInspect`, `TaskList`, and `ContainerInspect` on its own container (to identify its service and image).
- Write: `ServiceUpdate` on the selected services, and `ServiceCreate` / `ServiceRemove` for the cleanup job only.
- The cleanup job runs Iðunn's own image (`idunn cleanup`) with the same user, and only calls `ImageList`, `ContainerList` and `ImageRemove` on its node. Images are never force-removed: an image still referenced by a stopped container (Swarm task history) stays until those containers are pruned.
- No container creation, no exec. No listening port, unless `IDUNN_METRICS_ADDR` is set: the metrics endpoint is read-only and unauthenticated, so keep it on an internal network.
- Secrets are only read from files or environment variables, and are masked in logs, notifications and errors.

### User and socket group

The image starts as root only long enough to read the group owning the Docker socket, then switches for good to user `65532` with that group as its only supplementary group, and no capabilities. It exits if the switch fails. This works even when the socket group ID differs between managers, which a fixed `user:` cannot handle (and `group_add` is not supported by `docker stack deploy`). The cleanup job does the same.

If every manager shares the same socket group, you may instead set `user: "65532:<gid>"`; Iðunn then runs as given and skips the switch. Either way, use `read_only: true`.

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
