#!/usr/bin/env bash
set -euo pipefail

release_tag=${1:?Usage: check-release-tag.sh vMAJOR.MINOR.PATCH}
if [[ ! $release_tag =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]; then
  echo 'Release tags must use stable semantic versions, for example v0.1.0.' >&2
  exit 1
fi
release_commit=$(git rev-parse --verify "refs/tags/${release_tag}^{commit}")
if [[ $release_commit != "$(git rev-parse HEAD)" ]]; then
  echo 'The checkout must match the release tag.' >&2
  exit 1
fi
if ! git merge-base --is-ancestor "$release_commit" refs/remotes/origin/main; then
  echo 'Release tags must point to a commit on origin/main.' >&2
  exit 1
fi
