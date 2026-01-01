IMAGE_NAME := gcr.io/binocular-cv/mjpeg:latest
DOCKERFILE := Dockerfile
KUBE_YAML := ../kubernetes/mjpeg-deployment.yaml

.PHONY: all build push apply delete

# 'all' now performs a robust rolling update without deleting the service
all: build push apply

build:
	docker build --platform linux/amd64 -t $(IMAGE_NAME) -f $(DOCKERFILE) .

push:
	docker push $(IMAGE_NAME)

# 'apply' now updates the deployment and forces a restart to pull the latest image
apply:
	kubectl apply -f $(KUBE_YAML)
	kubectl rollout restart deployment mjpeg

# 'delete' is kept for manual cleanup if needed
delete:
	kubectl delete -f $(KUBE_YAML)




