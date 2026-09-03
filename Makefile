test-unit:
	cd controller && go vet ./... && go test ./...
test-e2e:
	test/e2e.sh

# Build/Release package configuration
release:
	cp controller/manifests/crd.yml config/crd.yml
	kctrl package release -y -v ${VERSION}
	cp carvel-artifacts/packages/ansible-supervisor.fling.vsphere.vmware.com/metadata.yml ./ansible-supervisor.yml
	echo "\n---" >> ./ansible-supervisor.yml
	cat carvel-artifacts/packages/ansible-supervisor.fling.vsphere.vmware.com/package.yml >> ./ansible-supervisor.yml
build-controller:
	docker build -f controller/Dockerfile -t ghcr.io/warroyo/ansible-supervisor-service/controller:${VERSION} controller/.
release-controller:
	docker push ghcr.io/warroyo/ansible-supervisor-service/controller:${VERSION}
