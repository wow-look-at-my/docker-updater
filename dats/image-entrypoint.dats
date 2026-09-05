# The image's ENTRYPOINT must never name the APE itself.
#
# go-toolchain builds one fat APE, so the image no longer copies a per-platform
# ELF. The kernel cannot exec an APE: the header is a shell script, and it
# answers with ENOEXEC, which docker reports as exit 126. A rolling update
# creates the replacement from the OLD container's config, so the entrypoint
# an old container recorded is the one the new image gets started with.
# /docker-updater must therefore stay a path the kernel can exec.
#
# These assertions read the Dockerfile, not a running container. The defect is
# a spelling in the Dockerfile. A bare exec also cannot be reproduced from a
# shell: when execve answers ENOEXEC, the shell runs the file as a script
# instead, so the broken form looks fine.

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

	- desc: the image carries the shell the APE trampoline needs
	  cmd: grep -E '^COPY --from=dockercli /shell ' Dockerfile
	  outputs:
		stdout:
			- "COPY --from=dockercli /shell /bin"
