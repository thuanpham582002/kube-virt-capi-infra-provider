docker buildx build \
  --platform linux/amd64,linux/arm64 \
  --push \
  -t ghcr.io/thuanpham582002/cluster-api-kubevirt-controller:latest \
  -f Dockerfile .
-t ghcr.io/thuanpham582002/cluster-api-kubevirt-controller:$(git rev-parse --short HEAD)

# Use this on the cluster to restart the controller manager after pushing a new image
# kubectl rollout restart deployment capk-controller-manager -n capk-system
