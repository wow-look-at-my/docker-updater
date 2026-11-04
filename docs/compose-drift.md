# Compose drift

A compose file can change after compose created a container from it. The container then runs a config the file no longer describes. docker-updater reports that drift and lists what differs. It recreates the container from the file only after an operator queues the recreate on the dashboard.

## The check

On every cycle, for each compose-managed container, the updater runs:

```sh
docker compose -f <config_files...> --project-directory <working_dir> -p <project> config --hash <service>
```

The project, files and directory come from the container's own compose labels. The result is compared with the container's `com.docker.compose.config-hash` label. Compose computes both values with the same function.

## What it does

| Result | Action | Dashboard |
| --- | --- | --- |
| Hashes match | nothing | nothing |
| Hashes differ, nothing queued | nothing applied | purple `out of sync`, with a `recreate` button |
| Hashes differ, recreate queued | recreate from the file at the next check | purple `recreate queued`, with `force recreate` and `cancel` buttons |
| Hashes differ on docker-updater's own container | `ERROR` log, nothing applied | red drift line, counted as an error |
| No hash (compose failed, file not visible, no label) | `ERROR` log, nothing applied | red unknown line, counted as an error |

Each drift line has a toggle that lists the differences, as `field: container → file`. Field names are the Docker API's, such as `HostConfig.Memory`.

## The differences

compose translates the file into a Docker create request, and docker-updater compares that request with `docker inspect`. It runs the recreate compose will do, against a socket of its own:

```sh
DOCKER_HOST=unix://<capture socket> docker compose -f <config_files...> --project-directory <working_dir> -p <project> up --no-deps --no-build --no-start --pull never --force-recreate <service>
```

The socket passes reads to the daemon. It records the create request and answers it with an error, so compose stops there. It refuses every other write. compose creates its replacement before it stops the container, so nothing changes.

Every key of `Config`, `HostConfig` and each network endpoint is compared. A few values get a rule:

- The daemon fills in some fields when the request leaves them empty. Examples are `Hostname`, `ShmSize`, `IpcMode`, `MaskedPaths` and the logging driver. Only a value the request sets is compared.
- The image supplies the command, entrypoint, user and stop signal that the request leaves empty. The daemon merges the image's environment, labels, exposed ports and volumes with the request's.
- compose's own labels change on every recreate. As a result, they are skipped.
- The daemon assigns the address and the IDs of an endpoint when the container starts. As a result, they are skipped.

Every value shows in full, environment values included. A seccomp profile shows as its whole JSON, because compose puts the profile into the request.

The list is unavailable when compose fails, or when it wants another write before the create. One example is a dependent service with `restart: true`, which compose stops first. The drift line then says why.

The capture never pulls. When the file names an image the daemon does not have, the drift line says `<image> is not pulled yet; the recreate pulls it`. The `recreate` button stays available.

## The recreate

The `recreate` button queues a recreate of the file the dashboard shows. The queue is in memory, and a start of docker-updater forgets it. At the next check the recreate runs:

```sh
docker compose -f <config_files...> --project-directory <working_dir> -p <project> up -d --no-deps <service>
```

With the default pull policy, compose pulls the image the file names when the daemon does not have it.

It waits on the same gates as an update. The pre-update check and dry-run mode hold it back. `force recreate` skips both and runs it now. `cancel` drops it. The post-update health check runs on the new container.

A failed check of the running image does not hold the recreate back. One example is a locally built image that is in no registry. The file can name another image, and then the recreate is what moves the container off the image nobody can check. A successful recreate clears the error and ends the `STUCK` run. While the check fails and the file names another image, the error says to queue the recreate. A failed update is different: an update that tried and failed holds the queued recreate back.

The queue names the file hash the operator approved. If the file changes again before the recreate runs, the entry is dropped and the dashboard offers `recreate` again.

An image update of a compose service in image mode always goes through compose. As a result, it applies the file whether a recreate is queued. An update of a rolling or git-mode service copies the running config, unless a recreate is queued. Then it goes through compose too. A rolling update then stops the container before it starts the new one, because compose cannot start the replacement first.

## Limits

- Compose keeps no copy of the config. A recreate that fails its health check cannot roll back. The error says so. The next cycle sees no drift. As a result, it does not loop. Fix the file and apply it again.
- The file replaces the container's config, mounts included. A file that swaps a bind mount for a tmpfs loses the data on the recreate. Read the differences before you queue it.
- docker-updater never recreates its own container through compose. Compose stops this process before it starts the replacement. The log and the dashboard give the command to run.
- Interpolation reads docker-updater's own environment and the project's `.env`. A variable that exists only in the shell that ran `docker compose up` reads as drift. The recreate drops it.
