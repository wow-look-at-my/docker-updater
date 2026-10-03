# Compose drift

A compose file can change after compose created a container from it. The container then runs a config the file no longer describes. docker-updater treats that drift like an available update. It recreates the container from the file.

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
| Hashes differ, no image update | recreate from the file | purple `out of sync` while held back, with a `force recreate` button |
| Hashes differ, image update found | the update goes through compose and applies the file too | amber `update available`, with a `force update` button |
| Hashes differ on docker-updater's own container | `ERROR` log, nothing applied | red drift line, counted as an error |
| No hash (compose failed, file not visible, no label) | `ERROR` log, nothing applied | red unknown line, counted as an error |

The recreate runs:

```sh
docker compose -f <config_files...> --project-directory <working_dir> -p <project> up -d --no-deps <service>
```

It waits on the same gates as an update. The pre-update check and dry-run mode hold it back. The force button skips both. The post-update health check runs on the new container.

An update of a drifted rolling or git-mode service also goes through that command. A rolling update then stops the container before it starts the new one, because compose cannot start the replacement first.

## Limits

- Compose keeps no copy of the config. A recreate that fails its health check cannot roll back. The error says so. The next cycle sees no drift. As a result, it does not loop. Fix the file and apply it again.
- The file replaces the container's config, mounts included. A file that swaps a bind mount for a tmpfs loses the data on the recreate. Use the pre-update check or dry-run mode to hold a container whose file you have not reviewed.
- docker-updater never recreates its own container through compose. Compose stops this process before it starts the replacement. The log and the dashboard give the command to run.
- Interpolation reads docker-updater's own environment and the project's `.env`. A variable that exists only in the shell that ran `docker compose up` reads as drift. The recreate drops it.
