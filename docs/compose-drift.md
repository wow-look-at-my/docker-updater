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

Each drift line has a toggle that lists the differences, as `field: container → file`. The list compares the image, entrypoint, command, environment, labels, mounts, ports, networks, restart policy, user, working directory, privileged mode and capabilities. It also compares security options, memory, swap, CPU and process limits, `shm_size`, ulimits, sysctls, devices, `init`, `read_only`, `tty`, `stdin_open`, the stop signal and grace period, `pid`, `ipc`, `hostname`, `group_add`, `dns`, `extra_hosts`, `runtime`, `oom_score_adj` and logging. It names environment variables but never shows their values. A seccomp profile shows as the hash of its content, because compose puts the whole profile into the container. Compose hashes a few rarer fields too, such as the healthcheck. A drift in one of those lists no difference. Compare `docker compose config <service>` with `docker inspect` for those.

## The recreate

The `recreate` button queues a recreate of the file the dashboard shows. The queue is in memory, and a restart of docker-updater forgets it. At the next check the recreate runs:

```sh
docker compose -f <config_files...> --project-directory <working_dir> -p <project> up -d --no-deps <service>
```

It waits on the same gates as an update. The pre-update check and dry-run mode hold it back. `force recreate` skips both and runs it now. `cancel` drops it. The post-update health check runs on the new container.

The queue names the file hash the operator approved. If the file changes again before the recreate runs, the entry is dropped and the dashboard offers `recreate` again.

An image update of a compose service in image mode always goes through compose. As a result, it applies the file whether a recreate is queued. An update of a rolling or git-mode service copies the running config, unless a recreate is queued. Then it goes through compose too. A rolling update then stops the container before it starts the new one, because compose cannot start the replacement first.

## Limits

- Compose keeps no copy of the config. A recreate that fails its health check cannot roll back. The error says so. The next cycle sees no drift. As a result, it does not loop. Fix the file and apply it again.
- The file replaces the container's config, mounts included. A file that swaps a bind mount for a tmpfs loses the data on the recreate. Read the differences before you queue it.
- docker-updater never recreates its own container through compose. Compose stops this process before it starts the replacement. The log and the dashboard give the command to run.
- Interpolation reads docker-updater's own environment and the project's `.env`. A variable that exists only in the shell that ran `docker compose up` reads as drift. The recreate drops it.
