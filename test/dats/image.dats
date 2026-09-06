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
		# docker CLI and its two plugins.
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
