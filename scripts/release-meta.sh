#!/usr/bin/env bash
set -euo pipefail
semver_re='^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$'

if [[ "$EVENT_NAME" == "workflow_dispatch" ]]; then
  raw="$INPUT_VERSION"
  version="${raw#v}"
  if [[ ! "$version" =~ $semver_re ]]; then
    echo "::error::Invalid version '$raw'. Expected X.Y.Z or vX.Y.Z"
    exit 1
  fi
  tag="v$version"
else
  tag="${GITHUB_REF_NAME}"
  version="${tag#v}"
  if [[ ! "$version" =~ $semver_re ]]; then
    echo "::error::Invalid tag '$tag'. Expected vX.Y.Z"
    exit 1
  fi
fi

if [[ "$GITHUB_REF" != "refs/tags/$tag" ]]; then
  echo "::error::Selected ref $GITHUB_REF must match requested release refs/tags/$tag (signing identity)."
  exit 1
fi

git fetch --force origin main --tags
if ! git rev-parse -q --verify "refs/tags/$tag" >/dev/null; then
  echo "::error::Tag $tag does not exist. Run mise run release:bump and push the tag first."
  exit 1
fi
tag_sha="$(git rev-list -n 1 "$tag")"
workflow_sha="$(git rev-parse "${GITHUB_SHA}^{commit}")"
if [[ "$tag_sha" != "$workflow_sha" ]]; then
  echo "::error::Tag $tag moved or does not match the workflow commit ($workflow_sha)."
  exit 1
fi
if ! git merge-base --is-ancestor "$tag_sha" origin/main; then
  echo "::error::Tag $tag ($tag_sha) is not reachable from origin/main"
  exit 1
fi

{
  echo "tag=$tag"
  echo "version=$version"
  echo "sha=$tag_sha"
} >> "$GITHUB_OUTPUT"
