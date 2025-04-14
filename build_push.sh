#!/bin/bash

set -e  # Exit on any error

# Validate input
if [ "$#" -ne 3 ]; then
  echo "Usage: $0 REGISTRY_PATH IMAGE_NAME TAG"
  echo "Example: $0 <account_id>.dkr.ecr.us-east-1.amazonaws.com my-app v1.0.0"
  exit 1
fi

# Input arguments
REGISTRY_PATH="$1"
IMAGE_NAME="$2"
TAG="$3"

# Full image base location
FULL_IMAGE_PATH="${REGISTRY_PATH}/${IMAGE_NAME}:${TAG}"

# Platforms to build
PLATFORMS=("amd64" "arm64")

# Step 0: Ensure Buildx is installed and properly set up
if ! docker buildx version >/dev/null 2>&1; then
  echo "Error: Docker Buildx is not installed or not supported by your Docker installation."
  exit 1
fi

# Check if Buildx builder exists and is bootstrapped
if ! docker buildx inspect mybuilder >/dev/null 2>&1; then
  echo "No Buildx builder found. Creating and bootstrapping a new builder..."
  docker buildx create --name mybuilder --use
  docker buildx inspect mybuilder --bootstrap
fi

# Verify Buildx platforms
if ! docker buildx inspect --bootstrap | grep -q "Platforms"; then
  echo "Error: Buildx does not support multi-platform builds. Please ensure QEMU is installed."
  echo "Run: docker run --rm --privileged multiarch/qemu-user-static --reset -p yes"
  exit 1
fi
echo "Buildx is correctly set up."

# Step 1: Build single-platform images ONLY and load them locally
for arch in "${PLATFORMS[@]}"; do
  echo "Building single-platform image for architecture: ${arch}"
  docker buildx build \
    --platform "linux/${arch}" \
    -t "${FULL_IMAGE_PATH}-${arch}" \
    --load \
    .
done

# Step 2: Push each single-platform image explicitly
for arch in "${PLATFORMS[@]}"; do
  echo "Pushing image for architecture: ${arch}"
  docker push "${FULL_IMAGE_PATH}-${arch}"
done

# Step 3: Create and push the multi-architecture manifest
echo "Creating and pushing a multi-architecture manifest..."
docker manifest create "${FULL_IMAGE_PATH}" \
  "${FULL_IMAGE_PATH}-amd64" \
  "${FULL_IMAGE_PATH}-arm64"

docker manifest annotate "${FULL_IMAGE_PATH}" "${FULL_IMAGE_PATH}-amd64" --arch amd64
docker manifest annotate "${FULL_IMAGE_PATH}" "${FULL_IMAGE_PATH}-arm64" --arch arm64

docker manifest push "${FULL_IMAGE_PATH}"

echo "Multi-architecture image successfully pushed to ECR: ${FULL_IMAGE_PATH}"
