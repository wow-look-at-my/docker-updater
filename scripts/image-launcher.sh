#!/bin/sh
# The image installs this at /docker-updater, and the APE beside it under
# /usr/local/lib. The kernel cannot exec an APE: the file's header is a shell
# script, and the image registers no binfmt handler. A shebang script IS
# execable, so every spelling of the entrypoint reaches the binary, including
# the one an older container recorded.
exec /bin/sh /usr/local/lib/docker-updater/docker-updater "$@"
