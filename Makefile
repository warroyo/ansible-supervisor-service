CONTROLLER_IMAGE = ghcr.io/warroyo/ansible-supervisor-service/controller

test-unit:
	cd controller && go vet ./... && go test -race ./...
test-e2e:
	test/e2e.sh

# Build/Release package configuration
#
# `release` expects the controller image for this VERSION to already be in
# the registry: run build-controller then release-controller first. The
# kbld config it generates only maps the `controller` placeholder onto an
# already-pushed digest, it does not build or push anything.
#
# The mapping is written as a digest rather than a tag on purpose. This
# file ships inside the imgpkg bundle, and at install time an `overrides`
# entry is applied ahead of .imgpkg/images.yml -- a tag there would send
# kapp-controller back to the registry to re-resolve it on every deploy,
# which both breaks air-gapped installs and unpins the image. A digest
# needs no lookup and matches what the lock file already records.
release:
	cp controller/manifests/crd.yml config/crd.yml
	printf 'apiVersion: kbld.k14s.io/v1alpha1\nkind: Config\noverrides:\n- image: controller\n  newImage: %s@%s\n' \
		"${CONTROLLER_IMAGE}" \
		"$$(docker buildx imagetools inspect ${CONTROLLER_IMAGE}:${VERSION} --format '{{.Manifest.Digest}}')" \
		> config/config-release.yml
	kctrl package release -y -v ${VERSION}
	cp carvel-artifacts/packages/ansible-supervisor.fling.vsphere.vmware.com/metadata.yml ./ansible-supervisor.yml
	echo "\n---" >> ./ansible-supervisor.yml
	cat carvel-artifacts/packages/ansible-supervisor.fling.vsphere.vmware.com/package.yml >> ./ansible-supervisor.yml
build-controller:
	docker build -f controller/Dockerfile -t ${CONTROLLER_IMAGE}:${VERSION} controller/.
release-controller:
	docker push ${CONTROLLER_IMAGE}:${VERSION}
