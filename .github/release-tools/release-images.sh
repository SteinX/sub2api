#!/usr/bin/env bash
set -euo pipefail
: "${RELEASE_VERSION:?}" "${RELEASE_SHA:?}" "${GITHUB_REPOSITORY:?}" "${RUNNER_TEMP:?}"
registries=("ghcr.io/${GITHUB_REPOSITORY,,}")
if [[ ${SIMPLE_RELEASE:-false} != true && ${DOCKERHUB_USERNAME:-skip} != skip ]]; then
  registries+=("${DOCKERHUB_USERNAME}/sub2api")
fi
arches=(amd64 arm64)
if [[ ${SIMPLE_RELEASE:-false} == true ]]; then arches=(amd64); fi
for arch in "${arches[@]}"; do
  args=(--platform "linux/$arch" --file ".release-context/$arch/Dockerfile"
    --label "org.opencontainers.image.version=$RELEASE_VERSION"
    --label "org.opencontainers.image.revision=$RELEASE_SHA"
    --label "org.opencontainers.image.source=https://github.com/$GITHUB_REPOSITORY")
  for registry in "${registries[@]}"; do
    args+=(--tag "$registry:$RELEASE_VERSION-$arch")
    if [[ ${SIMPLE_RELEASE:-false} == true ]]; then
      args+=(--tag "$registry:$RELEASE_VERSION" --tag "$registry:latest")
    fi
  done
  if [[ ${DRY_RUN:-false} == true ]]; then
    args+=(--output "type=oci,dest=$RUNNER_TEMP/sub2api-$arch.oci.tar")
  else
    # Finish every local build before checking acceptance at the actual write
    # boundary. A buildx --push would publish after a potentially long build
    # using a review/security snapshot taken before it started.
    args+=(--output "type=docker,dest=$RUNNER_TEMP/sub2api-$arch.docker.tar")
  fi
  docker buildx build "${args[@]}" ".release-context/$arch"
done
check_acceptance() {
  python3 "$RUNNER_TEMP/release-tools/devin-acceptance/release_guard.py" \
    --ref "${RELEASE_TAG:?}" --repository "$GITHUB_REPOSITORY"
}
if [[ ${DRY_RUN:-false} != true ]]; then
  for arch in "${arches[@]}"; do
    docker load --input "$RUNNER_TEMP/sub2api-$arch.docker.tar"
    for registry in "${registries[@]}"; do
      tags=("$RELEASE_VERSION-$arch")
      if [[ ${SIMPLE_RELEASE:-false} == true ]]; then tags+=("$RELEASE_VERSION" latest); fi
      for tag in "${tags[@]}"; do
        check_acceptance
        docker push "$registry:$tag"
      done
    done
  done
fi
if [[ ${DRY_RUN:-false} != true && ${SIMPLE_RELEASE:-false} != true ]]; then
  major=${RELEASE_VERSION%%.*}
  minor=${RELEASE_VERSION#*.}; minor=${minor%%.*}
  for registry in "${registries[@]}"; do
    check_acceptance
    docker buildx imagetools create \
      --tag "$registry:$RELEASE_VERSION" --tag "$registry:latest" \
      --tag "$registry:$major.$minor" --tag "$registry:$major" \
      "$registry:$RELEASE_VERSION-amd64" "$registry:$RELEASE_VERSION-arm64"
  done
fi
