CONTROLLER_IMAGE = ghcr.io/warroyo/ansible-supervisor-service/controller

test-unit:
	cd controller && go vet ./... && go test -race ./...
test-e2e:
	test/e2e.sh

# Pre-release gate. Neither target runs in CI and neither is wired into
# any workflow: dev-release pushes to the registry, and verify-supervisor
# needs a Supervisor and an AWX instance on the same network, which a
# hosted runner cannot reach. `make test-unit` and `make test-e2e` remain
# the whole of what GitHub Actions runs.
#
# The loop before cutting a tag:
#
#   make dev-release DEV_VERSION=1.0.1-rc1              # build, push, package
#   make install-supervisor-service DEV_VERSION=1.0.1-rc1   # register + upgrade
#   make verify-supervisor DEV_VERSION=1.0.1-rc1 \
#     SUPERVISOR_NS=my-ns AWX_URL=... AWX_TOKEN=... \
#     AWX_TEMPLATE="Configure Webserver" VM_LABEL=app=webserver
#
# A dev release is an ordinary release cut under a pre-release version,
# so it exercises the real build, the real digest pinning and the real
# package assembly rather than a parallel path that could drift from it.
dev-release:
	@test -n "$(DEV_VERSION)" || { echo "set DEV_VERSION, e.g. make dev-release DEV_VERSION=1.0.1-rc1"; exit 1; }
	$(MAKE) build-controller VERSION=$(DEV_VERSION)
	$(MAKE) release-controller VERSION=$(DEV_VERSION)
	$(MAKE) release VERSION=$(DEV_VERSION)
	@echo
	@echo "ansible-supervisor.yml is built for $(DEV_VERSION)."
	@echo "Install or upgrade the Supervisor service with it, then run: make verify-supervisor DEV_VERSION=$(DEV_VERSION) ..."

# The Supervisor's RBAC refuses even a full vSphere administrator the
# right to create CRDs, ClusterRoles, PackageInstalls, namespaces or
# ServiceAccounts, so there is no `kubectl apply` shortcut here: the
# vCenter service-install path is the only way this service lands. It is
# scripted rather than clicked so the release loop is repeatable.
install-supervisor-service:
	test/install-supervisor-service.sh

# EXPECT_IMAGE is resolved here rather than in the script so the check is
# against the digest actually in the registry for this version - which is
# what catches a run validating a stale install.
verify-supervisor:
	@test -n "$(DEV_VERSION)" || { echo "set DEV_VERSION to the version you installed, e.g. make verify-supervisor DEV_VERSION=1.0.1-rc1"; exit 1; }
	EXPECT_IMAGE="$(CONTROLLER_IMAGE)@$$(docker buildx imagetools inspect $(CONTROLLER_IMAGE):$(DEV_VERSION) --format '{{.Manifest.Digest}}')" \
		test/verify-supervisor.sh

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
