# The image's ENTRYPOINT must never name the APE, and the shell it ships must be
# static.
#
# go-toolchain builds one fat APE, so the image no longer copies a per-platform
# ELF. The kernel cannot exec an APE: the header is a shell script. A rolling
# update creates the replacement from the OLD container's config, so the
# entrypoint an old container recorded is the one the new image gets started
# with. /docker-updater must therefore stay a path the kernel can exec.
#
# The final image is scratch and ships no /lib, so a busybox that names an ELF
# interpreter cannot start either. Docker reports that ENOENT against the
# ENTRYPOINT path, so the message names a file that is present.
#
# These assertions read the Dockerfile, which is where both defects are spelled,
# and go-toolchain runs them sandboxed on every build with no host and no
# docker. test/dats/image.dats asks the same questions of a running container
# and catches what a spelling cannot: whether the paths resolve and whether the
# binary starts.

tests:
	- desc: the entrypoint names the launcher, and the launcher is what lands there
	  cmd: grep -E '^(ENTRYPOINT|COPY --chmod=755 scripts/)' Dockerfile
	  outputs:
		stdout:
			- "COPY --chmod=755 scripts/image-launcher.sh /docker-updater"
			- 'ENTRYPOINT ["/docker-updater"]'

	- desc: the entrypoint does not put a shell in front of a path
	  cmd: |
		set -eu
		if grep -nE '^ENTRYPOINT \["/bin/sh"' Dockerfile; then
			echo 'the entrypoint reads the APE through a shell; make the launcher the entrypoint' >&2
			exit 1
		fi
		echo no-shell-prefix
	  outputs:
		stdout:
			- "no-shell-prefix"

	- desc: the APE lands beside the launcher, not on top of it
	  cmd: grep -E '^COPY --chmod=755 build/' Dockerfile
	  outputs:
		stdout:
			- "COPY --chmod=755 build/docker-updater /usr/local/lib/docker-updater/docker-updater"

	- desc: the launcher is a shebang script, which is what the kernel can exec
	  cmd: head -n1 scripts/image-launcher.sh
	  outputs:
		stdout:
			- "#!/bin/sh"

	- desc: the launcher starts the APE at the path the Dockerfile puts it
	  cmd: grep -E '^exec ' scripts/image-launcher.sh
	  outputs:
		stdout:
			- 'exec /bin/sh /usr/local/lib/docker-updater/docker-updater "$@"'

	# The defect that shipped: the shell came from the docker CLI stage, and
	# alpine's busybox is a PIE against /lib/ld-musl-x86_64.so.1.
	- desc: the shell comes from the busybox image, which ships a static busybox
	  cmd: |
		set -eu
		grep -E '^FROM busybox:[a-z-]+ AS shell$' Dockerfile
		grep -E '^COPY --from=shell /shell /bin$' Dockerfile
	  outputs:
		stdout:
			- "AS shell"
			- "COPY --from=shell /shell /bin"

	- desc: the shell does not come from a stage built on an alpine image
	  cmd: |
		set -eu
		if grep -nE '^COPY --from=dockercli /shell ' Dockerfile; then
			echo 'the shell comes from the alpine CLI image, whose busybox needs an ELF interpreter that scratch has no /lib for' >&2
			exit 1
		fi
		echo shell-not-from-alpine
	  outputs:
		stdout:
			- "shell-not-from-alpine"
