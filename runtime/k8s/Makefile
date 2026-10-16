.PHONY: build test fmt vet render-k8s render-openshift helm-lint containers

COMPONENTS := admission-controller orchestrator worker release-manager

build:
	go build ./...

test:
	go test ./...

fmt:
	gofmt -w $$(find . -name '*.go' -not -path './.git/*')

vet:
	go vet ./...

# Kustomize overlays render cleanly (no live cluster required).
render-k8s:
	kustomize build deploy/kustomize/overlays/development >/dev/null
	kustomize build deploy/kustomize/overlays/production >/dev/null

render-openshift:
	kustomize build deploy/kustomize/overlays/openshift >/dev/null

helm-lint:
	helm lint deploy/helm/ai-sdlc-k8s-runtime
	helm template deploy/helm/ai-sdlc-k8s-runtime >/dev/null

containers:
	@for component in $(COMPONENTS); do \
		podman build -f components/$$component/Containerfile \
			-t localhost/ai-sdlc-$$component:dev . || exit 1; \
	done
