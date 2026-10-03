# The published image must START. Nothing else in this repo proves that.
#
# The image is scratch plus a handful of files, so every dependency it has is
# one it ships itself. A file that asks the kernel for an ELF interpreter finds
# no /lib, and the kernel answers ENOENT. Docker reports that against the
# ENTRYPOINT path, so the container reports "exec /docker-updater: no such file
# or directory" for a file that is present, and restarts forever.
#
# The workflow builds the image and passes it as $DU_IMAGE. Needs the host's
# docker and its socket, so --no-sandbox.

shared:
	files:
		export.sh: |
			# Flatten the image once. dats runs tests concurrently, so a test
			# that consumed another test's output would race it.
			set -eu
			WORK="$(dirname "$ENV_FILE")"
			test -n "${DU_IMAGE:-}" || { echo "DU_IMAGE is not set" >&2; exit 1; }
			# A registered APE binfmt handler makes the kernel run any APE
			# through a shell, so a bare exec of one succeeds and these tests
			# stop being able to fail. The handler is host-wide and containers
			# inherit it, so refuse to report a green nobody earned.
			for h in /proc/sys/fs/binfmt_misc/*; do
				case "${h##*/}" in status|register|'*') continue ;; esac
				if grep -qi '^magic 4d5a714670443d27$' "$h" 2>/dev/null &&
					grep -q '^enabled$' "$h" 2>/dev/null; then
					echo "an APE binfmt handler is registered at $h, so these tests would prove nothing; run this job without one" >&2
					exit 1
				fi
			done
			mkdir -p "$WORK/rootfs"
			cid="$(docker create "$DU_IMAGE")"
			docker export "$cid" | tar -x -C "$WORK/rootfs"
			docker rm "$cid" >/dev/null
			{
				echo "WORK='$WORK'"
				echo "IMAGE='$DU_IMAGE'"
			} > "$ENV_FILE"

setup: env ENV_FILE={shared.env} sh {shared.export.sh}

tests:
	- desc: the container starts, connects to docker and reaches the update loop
	  cmd: |
		set -eu
		. {shared.env}
		name="du-dats-start-$$"
		docker rm -f "$name" >/dev/null 2>&1 || true
		docker run -d --name "$name" \
			-v /var/run/docker.sock:/var/run/docker.sock "$IMAGE" >/dev/null
		i=0
		while [ "$i" -lt 30 ]; do
			if docker logs "$name" 2>&1 | grep -q 'starting docker-updater'; then break; fi
			i=$((i + 1))
			sleep 1
		done
		docker logs "$name" 2>&1
		echo "status=$(docker inspect -f '{{ .State.Status }}' "$name")"
		echo "restarts=$(docker inspect -f '{{ .RestartCount }}' "$name")"
		docker rm -f "$name" >/dev/null
	  outputs:
		stdout:
			- "connected to Docker"
			- "starting docker-updater"
			- "status=running"
			- "restarts=0"

	# The defect this suite exists for. Every executable in the image is checked,
	# not just the shell, because the next file added to a scratch image has the
	# same failure mode and the same misleading error message.
	- desc: nothing the image ships asks for an ELF interpreter
	  cmd: |
		set -eu
		. {shared.env}
		: > "$WORK/elf.txt"
		find "$WORK/rootfs" -type f -print | while read -r f; do
			if file -b "$f" | grep -q '^ELF'; then
				printf '%s: %s\n' "${f#"$WORK/rootfs"}" "$(file -b "$f")" >> "$WORK/elf.txt"
			fi
		done
		cat "$WORK/elf.txt"
		if grep -q 'interpreter ' "$WORK/elf.txt"; then
			echo 'the image ships a dynamically linked executable, and scratch has no /lib to load its interpreter from' >&2
			exit 1
		fi
		# A vacuous pass is the failure mode of a check like this one: an empty
		# rootfs would also match no interpreter. The image ships busybox, the
		# docker CLI and its plugins.
		found="$(wc -l < "$WORK/elf.txt")"
		test "$found" -ge 4 || { echo "expected at least 4 ELF files, found $found" >&2; exit 1; }
		echo "all-static elf=$found"
	  outputs:
		stdout:
			- "all-static elf="

	- desc: the shipped shell runs and carries the applets the APE trampoline needs
	  cmd: |
		set -eu
		. {shared.env}
		docker run --rm --entrypoint /bin/sh "$IMAGE" -c '
			for a in cksum tr mkdir cp; do
				command -v "$a" >/dev/null || { echo "missing applet: $a" >&2; exit 1; }
			done
			echo shell-ok'
	  outputs:
		stdout:
			- "shell-ok"

	# A rolling update creates the replacement from the OLD container's config,
	# so the entrypoint an old container recorded is the one the new image is
	# started with. It must stay a path the kernel can exec: a bare exec of the
	# APE is exit 126.
	- desc: the recorded entrypoint is the launcher, and it still starts the binary
	  cmd: |
		set -eu
		. {shared.env}
		docker image inspect "$IMAGE" --format 'entrypoint={{ json .Config.Entrypoint }}'
		docker run --rm --entrypoint /docker-updater "$IMAGE" 2>&1 | head -n 1
	  outputs:
		stdout:
			- 'entrypoint=["/docker-updater"]'
			- "docker-updater"
			- "starting"

	# The drift list must come from the request compose would send, through the
	# compose plugin the image ships, against a real daemon. The capture must
	# leave the container alone.
	- desc: a compose drift lists the changed fields and leaves the container running
	  cmd: |
		set -eu
		. {shared.env}
		p="$WORK/drift-$$"
		name="du-dats-drift-$$"
		mkdir -p "$p"
		write_compose() {
			printf 'services:\n  app:\n    image: %s\n    entrypoint: ["/bin/sh", "-c", "sleep 600"]\n    labels:\n      %s: "true"\n      docker-updater.well-known: "false"\n%b' "$IMAGE" "$name" "$1" > "$p/compose.yml"
		}
		write_compose ''
		docker compose -f "$p/compose.yml" -p "$name" up -d --quiet-pull 2>&1
		before="$(docker compose -f "$p/compose.yml" -p "$name" ps -q app)"
		write_compose '    mem_limit: 64m\n    pids_limit: 100\n    security_opt: ["no-new-privileges:true"]\n'
		docker run -d --name "$name-du" -v /var/run/docker.sock:/var/run/docker.sock -v "$p:$p" \
			-e DOCKER_UPDATER_LABEL="$name" -e DOCKER_UPDATER_DRY_RUN=true "$IMAGE" >/dev/null
		i=0
		while [ "$i" -lt 60 ]; do
			if docker exec "$name-du" wget -qO- http://127.0.0.1:8080/api/containers > "$p/api.json" 2>/dev/null &&
				grep -q '"differences' "$p/api.json"; then break; fi
			i=$((i + 1))
			sleep 1
		done
		docker logs "$name-du" 2>&1 | grep -i 'drift\|compose' || true
		grep -E '"(field|file|container|differences_error)":' "$p/api.json" | tr -d ' ' || true
		echo "fields=$(grep -c '"field":' "$p/api.json" || true)"
		after="$(docker compose -f "$p/compose.yml" -p "$name" ps -q app)"
		echo "unchanged=$([ "$before" = "$after" ] && echo yes || echo no)"
		echo "status=$(docker inspect -f '{{ .State.Status }}' "$after")"
		docker rm -f "$name-du" >/dev/null
		docker compose -f "$p/compose.yml" -p "$name" down -t 0 2>&1
	  outputs:
		stdout:
			- '"field":"HostConfig.Memory",'
			- '"file":"67108864",'
			- '"field":"HostConfig.PidsLimit",'
			- '"field":"HostConfig.SecurityOpt",'
			- '"file":"[\"no-new-privileges:true\"]",'
			- "fields=3"
			- "unchanged=yes"
			- "status=running"

	# The launcher must forward its arguments. A self-update spawns a detached
	# helper as `/docker-updater finish-self-update ...`, and a launcher that
	# dropped "$@" would start the update loop instead, so the updater could
	# never replace its own container.
	- desc: the launcher forwards arguments to the hidden self-update subcommand
	  cmd: |
		set -eu
		. {shared.env}
		docker run --rm "$IMAGE" finish-self-update 2>&1 | head -n 1
	  outputs:
		stdout:
			- "finish-self-update: --target and --image are required"
