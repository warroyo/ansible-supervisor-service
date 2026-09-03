# Using this from a VCF Automation 9.x blueprint (All Apps)

How to get "provision a VM, then configure it with Ansible" out of a **VCF Automation 9.x blueprint in an All Apps organization** - the thing you used to get from the Ansible Automation Platform integration and a `Cloud.Ansible.Tower` resource in a classic Aria Automation cloud template.

This page is about **All Apps** organizations only. If you are in a **VM Apps** organization you are running the classic Aria Automation model, where the built-in Ansible integration and its `Cloud.Ansible.Tower` resource type still exist and none of this is needed.

- [What changed from VM Apps](#what-changed-from-vm-apps)
- [One-time setup](#one-time-setup)
- [A complete blueprint](#a-complete-blueprint)
- [The patterns that matter](#the-patterns-that-matter)
- [Passing blueprint inputs to the playbook](#passing-blueprint-inputs-to-the-playbook)
- [Re-running the playbook as a day-2 update](#re-running-the-playbook-as-a-day-2-update)
- [Multiple VMs](#multiple-vms)
- [One-off actions with AnsibleRun](#one-off-actions-with-ansiblerun)
- [Decommissioning in the right order](#decommissioning-in-the-right-order)
- [Teardown](#teardown)
- [Troubleshooting](#troubleshooting)

## What changed from VM Apps

In an All Apps organization a blueprint is not an IaaS cloud template. Every resource in it is a Supervisor object: a `CCI.Supervisor.Namespace`, and then `CCI.Supervisor.Resource` entries each wrapping a Kubernetes manifest that gets applied into that namespace. The `Cloud.Ansible.Tower` resource type is not available here, and neither is the built-in Ansible integration behind it.

This service puts the capability back where All Apps can reach it: as a CRD on the Supervisor. An `AnsibleBinding` is an ordinary manifest, so a blueprint declares it exactly like it declares a `VirtualMachine`.

The product-level version of this mapping - what replaces the AAP integration regardless of blueprints - is in [the FAQ](FAQ.md#i-used-the-aap-integration-in-classic-aria-automation-what-maps-to-what). The blueprint-specific version:

| | VM Apps / classic Aria Automation | All Apps (VCFA 9.x) |
|---|---|---|
| Where Ansible config lives | `Cloud.Ansible.Tower` resource in the cloud template | `AnsibleBinding` manifest in a `CCI.Supervisor.Resource` |
| Where credentials live | AAP integration, configured org-wide in vRA | `AWXConnection` + `Secret`, per Supervisor namespace |
| How a host is targeted | vRA passes the provisioned machine to the integration | Controller matches VMs by label and scopes the run with `--limit` |
| Which VM gets configured | Implicit - the machine the resource hangs off | Explicit - whatever `vmSelector` matches, so scope it per deployment |
| Re-running | Day-2 action on the deployment | Change an input that lands in an annotation, then update the deployment |
| Deployment waits for the playbook | Built into the resource | `wait` block on the `AnsibleBinding` resource |

The practical difference is the fourth row. `Cloud.Ansible.Tower` was attached to one machine and could not target anything else. `AnsibleBinding` is a selector, which is more powerful and less safe - a careless selector reaches every matching VM in the namespace, including VMs belonging to other deployments. [Scope the selector per deployment](#scope-the-selector-to-the-deployment).

## One-time setup

Done once by the platform team, not by every blueprint.

**1. Install the service on the Supervisor.** vCenter → Workload Management → Services → Add Service. See the [Quickstart](QUICKSTART.md#1-install-the-service).

**2. Create the `AWXConnection` in each project's Supervisor namespace**, by hand or by whatever config management you use for namespaces:

```bash
kubectl create secret generic awx-token -n <project-supervisor-namespace> \
  --from-literal=token='<AWX API token>'

kubectl apply -n <project-supervisor-namespace> -f - <<'EOF'
apiVersion: field.vmware.com/v1
kind: AWXConnection
metadata:
  name: awx
spec:
  url: "https://awx.example.com"
  secretRef: "awx-token"
EOF
```

Keep this out of the blueprint. A blueprint that creates its own `Secret` puts the AWX API token into the deployment's input values, where it is visible to anyone who can read the deployment, and it gets re-created and deleted on every deploy. The connection is namespace infrastructure with a lifecycle of its own; the blueprint should only reference it by name. Consumers do not need to know the token exists.

If you genuinely need a blueprint-owned connection - a self-service org where each team brings its own AWX - use an `encrypted: true` input for the token and create the `Secret` and `AWXConnection` as two more `CCI.Supervisor.Resource` entries the binding `dependsOn`.

**3. Confirm project users can create the CRs.** The service ships RBAC that aggregates into the standard `edit` and `admin` cluster roles, so a user with edit on the Supervisor namespace can manage `AnsibleBinding`s without an extra grant. Deployments are applied as the requesting user, so this is what lets a blueprint create one at all.

**4. Prepare the AWX template**: Prompt on Launch enabled for Limit (and for Variables if the blueprint passes `extraVars`), a Machine credential attached, and an inventory. Details in the [Quickstart](QUICKSTART.md#2-prepare-the-awx-template).

## A complete blueprint

Provisions a VM and does not report the deployment as successful until Ansible has configured it.

```yaml
formatVersion: 2

inputs:
  vmName:
    type: string
    title: VM name
    description: Lowercase DNS-compatible name
    pattern: ^[a-z0-9]([a-z0-9-]{0,40}[a-z0-9])?$
    default: webserver
  vmClass:
    type: string
    title: VM class
    enum:
      - best-effort-small
      - best-effort-medium
    default: best-effort-small
  appEnvironment:
    type: string
    title: Environment
    enum:
      - development
      - staging
      - production
    default: development
  configRunId:
    type: string
    title: Configuration run ID
    description: Change this value and update the deployment to re-run the playbook
    default: '1'

outputs:
  vmIpAddress:
    type: string
    title: VM IP address
    value: ${resource.Webserver_VM.object.status.network.primaryIP4}
  ansibleState:
    type: string
    title: Ansible state
    value: ${resource.Webserver_Ansible.object.status.state}
  ansibleJobUrl:
    type: string
    title: AWX job
    value: ${resource.Webserver_Ansible.object.status.vms[0].lastJobURL}

resources:

  CCI_Supervisor_Namespace_1:
    type: CCI.Supervisor.Namespace
    properties:
      name: <your-project-supervisor-namespace>
      existing: true

  # Bootstrap the SSH user AWX's Machine credential logs in as. Nothing
  # about this service is installed on the VM - it only has to accept SSH.
  # The user name and public key below must match that Machine credential:
  # a VM that boots without them comes up fine and then fails the job with
  # `unreachable`, because sshd answers and then rejects the login.
  Webserver_Bootstrap:
    type: CCI.Supervisor.Resource
    properties:
      context: ${resource.CCI_Supervisor_Namespace_1.id}
      manifest:
        apiVersion: v1
        kind: Secret
        metadata:
          name: ${input.vmName}-bootstrap-${env.shortDeploymentId}
        stringData:
          user-data: |
            #cloud-config
            hostname: ${input.vmName}
            users:
              - name: ansible
                sudo: ALL=(ALL) NOPASSWD:ALL
                shell: /bin/bash
                ssh_authorized_keys:
                  - <your SSH public key>
            ssh_pwauth: false

  Webserver_VM:
    type: CCI.Supervisor.Resource
    dependsOn:
      - Webserver_Bootstrap
    properties:
      context: ${resource.CCI_Supervisor_Namespace_1.id}
      manifest:
        apiVersion: vmoperator.vmware.com/v1alpha5
        kind: VirtualMachine
        metadata:
          name: ${input.vmName}-${env.shortDeploymentId}
          labels:
            app: webserver
            deployment: ${env.shortDeploymentId}
        spec:
          className: ${input.vmClass}
          imageName: vmi-0123456789abcdef0
          storageClass: <your-storage-class>
          powerState: PoweredOn
          bootstrap:
            cloudInit:
              rawCloudConfig:
                name: ${resource.Webserver_Bootstrap.manifest.metadata.name}
                key: user-data
      wait:
        timeoutSeconds: 900
        conditions:
          - type: VirtualMachineCreated
            status: 'True'
        jsonPath:
          - path: '{.status.network.primaryIP4}'
            regex: \d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}

  Webserver_Ansible:
    type: CCI.Supervisor.Resource
    dependsOn:
      - Webserver_VM
    properties:
      context: ${resource.CCI_Supervisor_Namespace_1.id}
      manifest:
        apiVersion: field.vmware.com/v1
        kind: AnsibleBinding
        metadata:
          name: ${input.vmName}-config-${env.shortDeploymentId}
          annotations:
            ansible.field.vmware.com/reconcile-requested-at: ${input.configRunId}
        spec:
          vmSelector:
            app: webserver
            deployment: ${env.shortDeploymentId}
          awxConnectionRef: awx
          template:
            name: "Configure Webserver"
            type: JobTemplate
          extraVars:
            environment: ${input.appEnvironment}
            deployment_id: ${env.shortDeploymentId}
          cleanupPolicy: Delete
      wait:
        timeoutSeconds: 1800
        jsonPath:
          - path: '{.status.state}'
            regex: '^Ready$'
```

Substitute the namespace name, storage class, image and template name. The `imageName` and `storageClass` values come from the namespace itself (`kubectl get virtualmachineimage,storageclass -n <namespace>`), and the easiest way to get a VM manifest that is definitely valid for your environment is to build one once in the VCFA VM wizard and copy the YAML it produces.

## The patterns that matter

### Scope the selector to the deployment

`${env.shortDeploymentId}` is stamped onto the VM as a label and matched by `vmSelector`, so deployment A's binding only ever touches deployment A's VM. Without that second label, every deployment from this blueprint shares one selector: each binding matches every other deployment's VMs, runs the playbook against all of them, and they fight over the same AWX inventory hosts. This is the one thing that has no analogue in `Cloud.Ansible.Tower` and the one thing that will bite you.

### Name resources per deployment too

`${env.shortDeploymentId}` in the VM name keeps AWX inventory host names unique - the inventory host name defaults to the VM's resource name. Two deployments producing a VM called `webserver` would produce one contested AWX host, and the controller refuses the second rather than hijacking it.

### `dependsOn` orders creation, `wait` is what makes it mean something

Without `wait` on the VM, the binding is created the moment the VM object exists, before it has an IP. That is not an error - the binding sits in `Pending` and starts the run when the IP appears - but the deployment reports complete before anything is configured. Waiting for `{.status.network.primaryIP4}` and then for the binding's `{.status.state}` is what makes deployment success actually mean "configured".

### Wait on `jsonPath`, not `conditions`

These CRDs report `.status.state` and `.status.ready`, not a `.status.conditions` array, so a `conditions:` wait block will never match. Use the `jsonPath` form shown above.

### A failed playbook surfaces as a wait timeout

A `jsonPath` wait has no way to express "and fail immediately if the state becomes `Failed`", so a playbook that fails leaves the deployment waiting out the full `timeoutSeconds` before reporting failure. Keep the timeout tight enough that this is tolerable - a few minutes past the playbook's expected runtime - and read `.status.message` or the `ansibleJobUrl` output to find out what actually happened. If you would rather the deployment succeed and report the configuration state instead of blocking on it, drop the `wait` block from the binding and rely on the `ansibleState` output.

### Use the VM API version your Supervisor actually serves

The example above uses `vmoperator.vmware.com/v1alpha5`, current on VCF 9.x Supervisors, but this moves between releases and blog examples go stale fast. Check yours before copying anything:

```bash
kubectl get crd virtualmachines.vmoperator.vmware.com \
  -o jsonpath='{range .spec.versions[?(@.served)]}{.name}{"\t"}{.storage}{"\n"}{end}'
```

The version marked `true` in the second column is the storage version; any served version can be written. The safest source for a VM manifest is still the VCFA VM wizard in your own environment - whatever it emits is correct for that Supervisor by construction.

Whichever you pick, the controller reads the VM fine. It resolves the VirtualMachine API version by discovery at startup, preferring the newest the Supervisor serves, and the API server converts between served versions - so a VM your blueprint creates at `v1alpha5` is read correctly even though the controller asked for it at whatever version it settled on. There is nothing to keep in sync between the blueprint and the service, and no version for the blueprint to avoid. The controller logs its choice on startup:

```
virtualmachine api: vmoperator.vmware.com/v1alpha5
```

Field names can move with the version too. If the `vmIpAddress` output comes back empty, check what your version actually calls it with `kubectl get vm <name> -n <namespace> -o yaml` rather than trusting `primaryIP4` here.

## Passing blueprint inputs to the playbook

`extraVars` is what replaces the parameter passing you did on the `Cloud.Ansible.Tower` resource, and it is the reason to build a blueprint around this at all: the request form's inputs become variables the playbook branches on.

```yaml
          extraVars:
            environment: ${input.appEnvironment}
            app_version: ${input.appVersion}
            deployment_id: ${env.shortDeploymentId}
```

Two constraints:

- **The AWX template needs Prompt on Launch for Variables**, or AWX silently discards everything sent here.
- **Every value must be a string.** `extraVars` is a `map[string]string`. Declare inputs that feed it as `type: string` - use an `enum` for a picklist rather than an `integer` or `boolean` input - or concatenate into a string in the expression. An `integer` input dropped straight in fails schema validation on the manifest, which shows up as the resource failing to create rather than as an obvious type error.

Host-level variables work the same way through `hostVariables`, which is where an `ansible_host` override goes if AWX has to reach the VM at some address other than the IP it reports.

## Re-running the playbook as a day-2 update

The `configRunId` input is the whole mechanism. It lands in the binding's `ansible.field.vmware.com/reconcile-requested-at` annotation, and the controller re-launches the template against every matched VM whenever that annotation's value changes. To re-run: update the deployment, change `configRunId` to anything it has not been before, submit.

Any spec change does the same thing on its own - changing `appEnvironment` re-runs the playbook with the new value without touching `configRunId`, because it bumps the binding's `metadata.generation`. `configRunId` exists for the case where nothing about the desired state changed and you just want the playbook run again: config drifted, the playbook itself was updated in AWX, a run failed on something you have since fixed.

Give it a `title` that says so on the request form. Consumers read "Configuration run ID" as an opaque field otherwise.

## Multiple VMs

One binding fans out to every VM its selector matches, so a blueprint that provisions several VMs of the same role needs exactly one `AnsibleBinding` for all of them. Label each VM with the same `app` and `deployment` pair and leave the binding as-is: each VM gets its own AWX inventory host, its own run, and its own entry in `status.vms`, and the binding is `Ready` only once all of them have succeeded.

Different roles get different bindings pointed at different templates - `app: webserver` at one, `app: database` at another - each with the same per-deployment label, and each with its own `wait` if the deployment should block on it.

Ordering between roles is `dependsOn` between the binding resources: a `Webserver_Ansible` that `dependsOn: [Database_Ansible]` does not start until the database binding's wait has been satisfied. That is how you get "configure the database, then the app tier" out of a single deployment.

## One-off actions with AnsibleRun

An `AnsibleBinding` is standing state, which is right for "this tier should be configured like this" and wrong for "register this in DNS". A binding for a one-time action has to be created, waited on, and then remembered about forever, because nothing ever finishes it.

`AnsibleRun` is the one-time version: one AWX job, launched once, terminal, and optionally self-deleting. In a blueprint it is another `CCI.Supervisor.Resource`, waited on exactly the same way:

```yaml
  Webserver_DNS:
    type: CCI.Supervisor.Resource
    dependsOn:
      - Webserver_VM
    properties:
      context: ${resource.CCI_Supervisor_Namespace_1.id}
      manifest:
        apiVersion: field.vmware.com/v1
        kind: AnsibleRun
        metadata:
          name: dns-${input.vmName}-${env.shortDeploymentId}
        spec:
          awxConnectionRef: awx
          template:
            name: "Register DNS Record"
            type: JobTemplate
          extraVars:
            zone: ${input.dnsZone}
            record_state: present
          varsFrom:
            - resource:
                apiVersion: vmoperator.vmware.com/v1alpha5
                kind: VirtualMachine
                name: ${input.vmName}-${env.shortDeploymentId}
              vars:
                record_name: "{.metadata.name}"
                record_ip: "{.status.network.primaryIP4}"
          activeDeadlineSeconds: 600
      wait:
        timeoutSeconds: 900
        jsonPath:
          - path: '{.status.state}'
            regex: '^Ready$'
```

This one is safe in `resources:` because registering DNS *is* something that should happen at provisioning time. Runs that should happen at some other time are a different problem - see [Decommissioning in the right order](#decommissioning-in-the-right-order).

Four things about it are worth understanding rather than copying:

**The playbook does not target the VM.** It runs `hosts: localhost` on the AWX execution node and calls the DNS API, so this run has no `vmRef` and no `hosts` - nothing goes into the AWX inventory and no `--limit` is sent. The VM is *data*, arriving as variables through `varsFrom`. The Infoblox (or nsupdate, or Route53) credential is an AWX Credential on the template, exactly like the Machine credential that logs into VMs; nothing sensitive passes through the blueprint.

**`varsFrom` is what removes the ordering headache.** The alternative is passing `${resource.Webserver_VM.object.status.network.primaryIP4}` as an `extraVars` string, which forces the VM's `wait` block to have already resolved the IP. Reading it off the live object instead means the run simply sits `Pending` and retries until the IP appears - `activeDeadlineSeconds` is what stops that waiting forever. Keeping the `wait` on the VM is still worth it, since it makes the first attempt succeed.

**No `ttlSecondsAfterFinished` on a run the blueprint owns.** A TTL makes the CR delete itself, and VCFA still believes that resource is part of the deployment - so the next update or delete is reconciling against something that has silently vanished. Let the deployment's own lifecycle remove it. Save the TTL for runs created outside VCFA, where nothing else is tracking them.

**A blueprint-owned run must not derive its spec from inputs that change.** An `AnsibleRun`'s spec is immutable: the API server rejects any edit to it. So if `zone` here came from an input and someone updated the deployment with a different zone, VCFA would try to patch the existing run, be refused, and fail the update. Two ways out - keep the spec independent of mutable inputs, or put the varying value in `metadata.name` so a change creates a *new* resource instead of patching the old one:

```yaml
        metadata:
          name: dns-${input.vmName}-${input.dnsZone}-${env.shortDeploymentId}
```

That second form is also how you deliberately get "re-run on input change" out of a resource that otherwise never runs twice.

Use the same shape for anything else that happens once at provisioning: opening a change ticket, registering in a CMDB, notifying a channel, adding the VM to a monitoring group.

## Decommissioning in the right order

There is no pre-delete hook this service can use - the mechanism exists in VM Service but is restricted to privileged accounts, and [the FAQ explains why](FAQ.md#why-is-there-no-pre-delete-hook). So a decommission playbook that needs to log into the guest has to run **while the VM is still up**, and the sequencing belongs to the deployment rather than to this service.

**This is a genuine regression from VM Apps, and worth being straight about.** There, `Cloud.Ansible.Tower` takes both `templates.provision[]` and `templates.de-provision[]`, and the deprovision playbooks run as part of that resource's own teardown - declared in one place, ordered correctly by construction, with nothing for the blueprint author to sequence. All Apps has no typed Ansible resource, and the generic `CCI.Supervisor.Resource` has no lifecycle phase to attach one to, so the ordering becomes the caller's problem.

**The trap is putting the decommission run in the blueprint's `resources:` block.** Everything in there is created when the *deployment* is created, not when it is deleted. A decommission `AnsibleRun` declared alongside the VM runs the teardown playbook at provisioning time - against a VM that probably has no IP yet - and then, being terminal, never runs again. There is no delete-time counterpart to `resources:`, so this has to come from somewhere else.

Two ways to get it, plus one that looks obvious and does not work.

### A separate decommission blueprint

Cleanest, and it puts the action in the catalog where people can find it. A second, tiny blueprint that takes the namespace and VM name as inputs, targets the existing namespace, and contains only the run:

```yaml
formatVersion: 2

inputs:
  supervisorNamespace:
    type: string
    title: Supervisor namespace
  vmName:
    type: string
    title: VM to decommission
    description: The VirtualMachine resource name, e.g. webserver-a1b2

resources:

  CCI_Supervisor_Namespace_1:
    type: CCI.Supervisor.Namespace
    properties:
      name: ${input.supervisorNamespace}
      existing: true

  Decommission:
    type: CCI.Supervisor.Resource
    properties:
      context: ${resource.CCI_Supervisor_Namespace_1.id}
      manifest:
        apiVersion: field.vmware.com/v1
        kind: AnsibleRun
        metadata:
          name: decom-${input.vmName}-${env.shortDeploymentId}
        spec:
          awxConnectionRef: awx
          template:
            name: "Decommission Host"
            type: JobTemplate
          vmRef:
            name: ${input.vmName}      # the guest must still be up
          activeDeadlineSeconds: 900
      wait:
        timeoutSeconds: 1200
        jsonPath:
          - path: '{.status.state}'
            regex: '^Ready$'
```

Request it, let it reach `Ready`, then delete the original deployment. The decommission deployment itself can be deleted afterwards; its run created no inventory host worth keeping.

### Not: a conditional resource in the same blueprint

Worth ruling out explicitly, because it is the first thing most people try. The idea is an input that decides whether the decommission run exists, flipped on a day-2 update just before deleting. It does not work, and the resource type says so.

`CCI.Supervisor.Resource` has exactly six properties. Read them off your own instance:

```bash
curl -sk -H "Authorization: Bearer $TOKEN" \
  https://<vcfa>/deployment/api/resource-types \
  | jq '.content[] | select(.id=="CCI.Supervisor.Resource") | .schema.properties'
```

| Property | Notes |
|---|---|
| `context` | the namespace resource id; `recreateOnUpdate` |
| `count` | **`ignoreOnUpdate`** |
| `existing` | use an existing namespace |
| `manifest` | the Kubernetes object |
| `object` | computed - the live object, which is what `${resource.X.object...}` reads |
| `wait` | the wait block |

`count` is `ignoreOnUpdate`, so changing it on a deployment update is ignored - a run gated behind `count` cannot be made to appear later. And there is no `condition` property at all. Note also that the blueprint validation endpoint accepts unknown resource properties without complaint, so a made-up `condition:` or a `count` expression will happily validate and then do nothing; the resource-type schema is the only reliable answer.

The same list is the reason there is no in-blueprint deprovision hook to reach for. `Cloud.Ansible.Tower` in a VM Apps organization carries `templates.provision[]` **and `templates.de-provision[]`**, because it is a typed resource whose provider implements a deprovision phase. `CCI.Supervisor.Resource` is a generic manifest wrapper with no lifecycle phases - it applies an object on create and deletes it on destroy, and there is nowhere to hang "run this on the way out".

### Out of band

If whatever drives decommission is not VCFA at all - a pipeline, an Orchestrator workflow, a person - it applies the `AnsibleRun` to the namespace directly, waits on `.status.state`, and then removes the deployment. This is the case where `ttlSecondsAfterFinished` earns its keep, because nothing else owns the object.

### Cleanup that doesn't need the guest

Removing a DNS record, closing a CMDB entry, silencing an alert - none of these need the VM alive, so they are not bound by any of the above and can run after the deployment is gone. They cannot use `varsFrom` against the VM at that point, though, because there is no VM left to read: whatever creates the run has to pass the name and address as literal `extraVars`.

## Teardown

Deleting the deployment deletes the `AnsibleBinding`, whose finalizer blocks until the controller has removed the AWX inventory hosts it created. This is normally invisible - it takes a second or two - but it means **the service must still be running on the Supervisor when deployments are deleted.** If the service is removed first, the bindings hang in `Terminating` and the deployment's delete does not finish. Recovery is to reinstall the service and let it drain them, or strip the finalizers by hand and clean AWX up yourself ([how to find the leftovers](FAQ.md#how-do-i-find-awx-hosts-a-supervisor-left-behind)).

`AnsibleRun` carries the same finalizer and the same caveat, and a blueprint-owned one is deleted with the deployment like any other resource. Don't give it a `ttlSecondsAfterFinished` to try to tidy it up sooner - see [above](#one-off-actions-with-ansiblerun) for why a resource that deletes itself out from under VCFA is worse than one that sticks around.

Set `cleanupPolicy: Retain` on a binding or run if you want the AWX host entries kept after the deployment is gone - for run history or audit. They are then yours to clean up.

## Troubleshooting

Everything here is visible with `kubectl` against the project's Supervisor namespace, which is generally faster than reading it out of the deployment view.

| Symptom | Cause |
|---|---|
| Binding resource fails to create | Schema validation. Usually a non-string value in `extraVars` or `hostVariables`, or an empty `vmSelector` - which is rejected deliberately, since it would match every VM in the namespace |
| Deployment times out waiting on the binding, state `Pending` | The selector matches nothing. Check the labels on the VM actually match both `vmSelector` keys - a `${env.shortDeploymentId}` in one place and not the other is the common one |
| VM resource creation rejected, `no matches for kind "VirtualMachine"` | The blueprint's `apiVersion` is not a version this Supervisor serves. [Check the served list](#use-the-vm-api-version-your-supervisor-actually-serves) |
| Deployment times out, state `Failed`, no job URL | The controller refused to launch. Prompt on Launch for Limit is off on the AWX template, or the inventory host name collides with one another supervisor owns. `.status.message` says which |
| Deployment times out, state `Failed`, job URL present | AWX ran the playbook and it failed. Open the job |
| Job fails `unreachable` | The cloud-init public key does not match the AWX Machine credential's private key, or AWX has no route to the VM's IP |
| Second deployment fails where the first succeeded | Resource names are not per-deployment. Add `${env.shortDeploymentId}` to the VM name |
| Playbook ran against VMs from another deployment | The selector is not scoped per deployment. See [Scope the selector to the deployment](#scope-the-selector-to-the-deployment) |

```bash
kubectl get ansiblebinding -n <project-supervisor-namespace>
kubectl get ansiblebinding <name> -n <project-supervisor-namespace> -o jsonpath='{.status.message}'
kubectl get ansiblebinding <name> -n <project-supervisor-namespace> \
  -o jsonpath='{range .status.vms[*]}{.name}{"\t"}{.phase}{"\t"}{.lastJobURL}{"\n"}{end}'
```

Full CRD reference in the [README](README.md), edge cases in the [FAQ](FAQ.md).
