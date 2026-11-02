#!/bin/sh
# The image installs this at /docker-updater, and the APE beside it under /usr/local/lib.
exec /bin/sh /usr/local/lib/docker-updater/docker-updater "$@"
