# Compose drift

Some updates copy the running container's config instead of reading the compose file. These are a rolling update, a git-mode update, and an update of docker-updater itself. A compose file that changed after such a container was created therefore never applies. The next manual `docker compose up` applies it, possibly without warning. A file that swaps a bind mount for a tmpfs then wipes the data.

## The check

On every cycle, for each compose-managed container on one of those paths, the updater runs:

```sh
docker compose -f <config_files...> --project-directory <working_dir> -p <project> config --hash <service>
```

The project, files and directory come from the container's own compose labels. The result is compared with the container's `com.docker.compose.config-hash` label. Compose computes both values with the same function.

## What it reports

| Result | Log | Dashboard |
| --- | --- | --- |
| Hashes match | nothing | nothing |
| Hashes differ | `ERROR container <name>: drifted from its compose file ...` | red drift line, counted as an error |
| No hash (compose failed, file not visible, no label) | `ERROR container <name>: compose drift unknown ...` | red unknown line, counted as an error |

Each report names the container, the service, the files, and the command that applies the file:

```sh
docker compose -f <config_files...> --project-directory <working_dir> -p <project> up -d --no-deps <service>
```

## What it does not do

The updater never applies the file to a drifted container. Either side can be the correct one, so the operator decides. Compare `docker compose ... config <service>` with `docker inspect <container>`. Then run the command, or change the file to match the container.

Interpolation reads docker-updater's own environment and the project's `.env`. A variable that exists only in the shell that ran `docker compose up` reads as drift.
