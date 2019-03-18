# Deploying Prow

This document will walk you through deploying your own Prow instance to a new Kubernetes cluster. If you encounter difficulties, please open an issue so that we can make this process easier.

Prow runs in any kubernetes cluster. Our `tackle` utility helps deploy it correctly, or you can perform each of the steps manually.

Both of these are focused on [Kubernetes Engine](https://cloud.google.com/kubernetes-engine/) but should work on any kubernetes distro with no/minimal changes.

## Tackle deployment

Prow's `tackle` utility walks you through deploying a new instance of prow in a couple minutes, try it out!

You need a few things:

1. [`bazel`](https://bazel.build/) build tool installed and working
1. The prow `tackle` utility. It is recommended to use it by running `bazel run //prow/cmd/tackle` from `test-infra` directory, alternatively you can install it by running `go get -u k8s.io/test-infra/prow/cmd/tackle` (in that case you would also need go installed and working).
1. A [personal access token][1] for the GitHub bot user you want to act as prow. If you're setting up prow for personal repo (not organization) then bot user == you (your GitHub account).

   - Token must have `public_repo` and `repo:status` scopes (and repo for private repos), more details [here][8].
   - Prow uploads this token into a secret in your cluster
   - Bot user must have write access to the repo

1. Optionally, credentials to a Kubernetes cluster

   - Otherwise the tackle command can help you create one

To install prow run the following from the `test-infra` directory and follow the on-screen instructions:

```bash
# Ideally use https://bazel.build, alternatively try:
#   go get -u k8s.io/test-infra/prow/cmd/tackle && tackle
bazel run //prow/cmd/tackle
```

The will help you through the following steps:

* Choosing a kubectl context (and creating a cluster / getting its credentials if necessary)
* Deploying prow into that cluster
* Configuring GitHub to send prow webhooks for your repos. Please note that the absolute path to a personal access token should be specified.

See the Next Steps section after running this utility.

## Manual deployment

If you do not want to use the `tackle` utility above, here are the manual set of commands tackle will run.

Prow runs in a kubernetes cluster, so first figure out which cluster you want to deploy prow into. If you already have a cluster, skip to the next step.

You can use the [GCP cloud console](https://console.cloud.google.com/) to set up a project and [create a new Kubernetes Engine cluster](https://console.cloud.google.com/kubernetes).

### Create the cluster

I'm assuming that `PROJECT` and `ZONE` environment variables are set.

```sh
export PROJECT=your-project
export ZONE=us-west1-a
```

Run the following to create the cluster. This will also set up `kubectl` to
point to the new cluster.

```sh
gcloud container --project "${PROJECT}" clusters create prow \
  --zone "${ZONE}" --machine-type n1-standard-4 --num-nodes 2
```

### Create cluster role bindings

As of 1.8 Kubernetes uses [Role-Based Access Control (“RBAC”)](https://kubernetes.io/docs/admin/authorization/rbac/) to drive authorization decisions, allowing `cluster-admin` to dynamically configure policies.
To create cluster resources you need to grant a user `cluster-admin` role in all namespaces for the cluster.

For Prow on GCP, you can use the following command.

```sh
kubectl create clusterrolebinding cluster-admin-binding \
  --clusterrole cluster-admin --user $(gcloud config get-value account)
```

For Prow on other platforms, the following command will likely work.

```sh
kubectl create clusterrolebinding cluster-admin-binding-"${USER}" \
  --clusterrole=cluster-admin --user="${USER}"
```

On some platforms the `USER` variable may not map correctly to the user
in-cluster. If you see an error of the following form, this is likely the case.

```console
Error from server (Forbidden): error when creating
"prow/cluster/starter.yaml": roles.rbac.authorization.k8s.io "<account>" is
forbidden: attempt to grant extra privileges:
[PolicyRule{Resources:["pods/log"], APIGroups:[""], Verbs:["get"]}
PolicyRule{Resources:["prowjobs"], APIGroups:["prow.k8s.io"], Verbs:["get"]}
APIGroups:["prow.k8s.io"], Verbs:["list"]}] user=&{<CLUSTER_USER>
[system:authenticated] map[]}...
```

Run the previous command substituting `USER` with `CLUSTER_USER` from the error
message above to solve this issue.

```sh
kubectl create clusterrolebinding cluster-admin-binding-"<CLUSTER_USER>" \
  --clusterrole=cluster-admin --user="<CLUSTER_USER>"
```

There are [relevant docs on Kubernetes Authentication](https://kubernetes.io/docs/reference/access-authn-authz/authentication/#authentication-strategies) that may help if neither of the above work.

### Create the GitHub secrets

You will need two secrets to talk to GitHub. The `hmac-token` is the token that
you give to GitHub for validating webhooks. Generate it using any reasonable
randomness-generator, eg `openssl rand -hex 20`.

```sh
# openssl rand -hex 20 > /path/to/hook/secret
kubectl create secret generic hmac-token --from-file=hmac=/path/to/hook/secret
```

The `oauth-token` is an OAuth2 token that has read and write access to the bot account:

- Bot user must have write access to the repo
- Token must have `public_repo` and `repo:status` scopes (and repo for private repos), more details [here][8].

Generate it from the
[account's settings -> Personal access tokens -> Generate new token][1].

```sh
# https://github.com/settings/tokens
kubectl create secret generic oauth-token --from-file=oauth=/path/to/oauth/secret
```

#### Bot account

The bot account used by prow must be granted owner level access to the GitHub
orgs that prow will operate on. Note that events triggered by this account are
ignored by some prow plugins. It is prudent to use a different bot account for
other GitHub automation that prow should interact with to prevent events from
being ignored unjustly.

### Add the prow components to the cluster

Run the following command to deploy a basic set of prow components.

```sh
kubectl apply -f prow/cluster/starter.yaml
```

After a moment, the cluster components will be running.

```console
$ kubectl get deployments
NAME         DESIRED   CURRENT   UP-TO-DATE   AVAILABLE   AGE
deck         2         2         2            2           1m
hook         2         2         2            2           1m
horologium   1         1         1            1           1m
plank        1         1         1            1           1m
sinker       1         1         1            1           1m
tide         1         1         1            1           1m
```

#### Get ingress IP address

Find out your external address. It might take a couple minutes for the IP to
show up.

```console
$ kubectl get ingress ing
NAME      HOSTS     ADDRESS          PORTS     AGE
ing       *         an.ip.addr.ess   80        3m
```

Go to that address in a web browser and verify that the "echo-test" job has a
green check-mark next to it. At this point you have a prow cluster that is ready
to start receiving GitHub events!

## Add the webhook to GitHub

Configure github to send your prow instance `application/json` webhooks
for specific repos and/or whole orgs.

You can do this with the `add-hook` utility:

```sh
# Note /path/to/hook/secret and /path/to/oauth/secret from earlier secrets step
# Note the an.ip.addr.ess from previous ingres step

# Ideally use https://bazel.build, alternatively try:
#   go get -u k8s.io/test-infra/experiment/add-hook && add-hook
bazel run //experiment/add-hook -- \
  --hmac-path=/path/to/hook/secret \
  --github-token-path=/path/to/oauth/secret \
  --hook-url http://an.ip.addr.ess/hook \
  --repo my-org/my-repo \
  --repo my-whole-org \
  --confirm=false  # Remove =false to actually add hook
```

Now go to your org or repo and click `Settings -> Webhooks`.

Look for the `http://an.ip.addr.ess/hook` you added above.
A green check mark (for a ping event, if you click edit and view the details of the event) suggests everything is working!

You can click `Add webhook` on the Webhooks page to add the hook manually
if you do not want to use the `add-hook` utility.

## Next steps

You now have a working Prow cluster (Woohoo!), but it isn't doing anything interesting yet.
This section will help you configure your first plugin and job, and complete any additional setup that your instance may need.

### Enable some plugins by modifying `plugins.yaml`

Create a file called `plugins.yaml` and add the following to it:

```yaml
plugins:
  YOUR_ORG/YOUR_REPO:
  - size
```

Replace `YOUR_ORG/YOUR_REPO:` with the appropriate values. If you want, you can
instead just say `YOUR_ORG:` and the plugin will run for every repo in the org.

Next, create an empty file called `config.yaml`:

```sh
touch config.yaml
```

Run the following to test the files, replacing the paths as necessary:

```sh
bazel run //prow/cmd/checkconfig -- --plugin-config=path/to/plugins.yaml --config-path=path/to/config.yaml
```

There should be no errors. You can run this as a part of your presubmit testing
so that any errors are caught before you try to update.

Now run the following to update the configmap, replacing the path as necessary:

```sh
kubectl create configmap plugins \
  --from-file=plugins.yaml=path/to/plugins.yaml --dry-run -o yaml \
  | kubectl replace configmap plugins -f -
```

We added a make rule to do this for us:

```Make
get-cluster-credentials:
    gcloud container clusters get-credentials "$(CLUSTER)" --project="$(PROJECT)" --zone="$(ZONE)"

update-plugins: get-cluster-credentials
    kubectl create configmap plugins --from-file=plugins.yaml=plugins.yaml --dry-run -o yaml | kubectl replace configmap plugins -f -
```

Now when you open a PR, it will automatically be labelled with a `size/*`
label. When you make a change to the plugin config and push it with `make
update-plugins`, you do not need to redeploy any of your cluster components.
They will pick up the change within a few minutes.

### Add more jobs by modifying `config.yaml`

Add the following to `config.yaml`:

```yaml
periodics:
- interval: 10m
  name: echo-test
  decorate: true
  spec:
    containers:
    - image: alpine
      command: ["/bin/date"]
postsubmits:
  YOUR_ORG/YOUR_REPO:
  - name: test-postsubmit
    decorate: true
    spec:
      containers:
      - image: alpine
        command: ["/bin/printenv"]
presubmits:
  YOUR_ORG/YOUR_REPO:
  - name: test-presubmit
    decorate: true
    always_run: true
    skip_report: true
    spec:
      containers:
      - image: alpine
        command: ["/bin/printenv"]
```

Again, run the following to test the files, replacing the paths as necessary:

```sh
bazel run //prow/cmd/checkconfig -- --plugin-config=path/to/plugins.yaml --config-path=path/to/config.yaml
```

Now run the following to update the configmap.

```sh
kubectl create configmap config \
  --from-file=config.yaml=path/to/config.yaml --dry-run -o yaml | kubectl replace configmap config -f -
```

We use a make rule:

```Make
update-config: get-cluster-credentials
    kubectl create configmap config --from-file=config.yaml=config.yaml --dry-run -o yaml | kubectl replace configmap config -f -
```

Presubmits and postsubmits are triggered by the `trigger` plugin. Be sure to
enable that plugin by adding it to the list you created in the last section.

Now when you open a PR it will automatically run the presubmit that you added
to this file. You can see it on your prow dashboard. Once you are happy that it
is stable, switch `skip_report` to `false`. Then, it will post a status on the
PR. When you make a change to the config and push it with `make update-config`,
you do not need to redeploy any of your cluster components. They will pick up
the change within a few minutes.

When you push or merge a new change to the git repo, the postsubmit job will run.

For more information on the job environment, see [`jobs.md`](/prow/jobs.md)

### Run test pods in a different namespace

You may choose to keep prowjobs or run tests in a different namespace. First
create the namespace by `kubectl create -f`ing this:

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: prow
```

Now, in `config.yaml`, set `prowjob_namespace` or `pod_namespace` to the
name from the YAML file. You can then use RBAC roles to limit what test pods
can do.

**Note**: If you set or update the `prowjob_namespace` or `pod_namespace`
fields after deploying the prow components, you will need to redeploy them
so that they pick up the change.

### Run test pods in different clusters

You may choose to run test pods in a separate cluster entirely. This is a good practice to keep testing isolated from Prow's service components and secrets. It can also be used to furcate job execution to different clusters.
Create a secret containing a `{"cluster-name": {cluster-details}}` map like this:

```yaml
default:
  endpoint: https://<master-ip>
  clientCertificate: <base64-encoded cert>
  clientKey: <base64-encoded key>
  clusterCaCertificate: <base64-encoded cert>
other:
  endpoint: https://<master-ip>
  clientCertificate: <base64-encoded cert>
  clientKey: <base64-encoded key>
  clusterCaCertificate: <base64-encoded cert>
```

Use [mkbuild-cluster][5] to determine these values:

```sh
bazel run //prow/cmd/mkbuild-cluster -- \
  --project=P --zone=Z --cluster=C \
  --alias=A \
  --print-entry | tee cluster.yaml
kubectl create secret generic build-cluster --from-file=cluster.yaml
```

Mount this secret into the prow components that need it (at minimum: `plank`,
`sinker` and `deck`) and set the `--build-cluster` flag to the location you mount it at. For
instance, you will need to merge the following into the plank deployment:

```yaml
spec:
  containers:
  - name: plank
    args:
    - --build-cluster=/etc/foo/cluster.yaml # basename matches --from-file key
    volumeMounts:
    - mountPath: /etc/foo
      name: cluster
      readOnly: true
  volumes:
  - name: cluster
    secret:
      defaultMode: 420
      secretName: build-cluster # example above contains a cluster.yaml key
```

Configure jobs to use the non-default cluster with the `cluster:` field.
The above example `cluster.yaml` defines two clusters: `default` and `other` to schedule jobs, which we can use as follows:

```yaml
periodics:
- name: cluster-unspecified
  # cluster:
  interval: 10m
  decorate: true
  spec:
    containers:
    - image: alpine
      command: ["/bin/date"]
- name: cluster-default
  cluster: default
  interval: 10m
  decorate: true
  spec:
    containers:
    - image: alpine
      command: ["/bin/date"]
- name: cluster-other
  cluster: other
  interval: 10m
  decorate: true
  spec:
    containers:
    - image: alpine
      command: ["/bin/date"]
```

This results in:

* The `cluster-unspecified` and `default-cluster` jobs run in the `default` cluster.
* The `cluster-other` job runs in the `other` cluster.

See [mkbuild-cluster][5] for more details about how to create/update `cluster.yaml`.

### Enable merge automation using Tide

PRs satisfying a set of predefined criteria can be configured to be
automatically merged by [Tide][6].

Tide can be enabled by modifying `config.yaml`.
See [how to configure tide][7] for more details.

#### Setup PR status dashboard

To setup a PR status dashboard like [prow.k8s.io/pr](https://prow.k8s.io/pr), follow the
instructions in [`pr_status_setup.md`](https://github.com/kubernetes/test-infra/blob/master/prow/docs/pr_status_setup.md).

### Configure SSL

Use [cert-manager][3] for automatic LetsEncrypt integration. If you
already have a cert then follow the [official docs][4] to set up HTTPS
termination. Promote your ingress IP to static IP. On GKE, run:

```sh
gcloud compute addresses create [ADDRESS_NAME] --addresses [IP_ADDRESS] --region [REGION]
```

Point the DNS record for your domain to point at that ingress IP. The convention
for naming is `prow.org.io`, but of course that's not a requirement.

Then, install cert-manager as described in its readme. You don't need to run it in
a separate namespace.

## Further reading

* [Developing for Prow](/prow/getting_started_develop.md)
* [Getting more out of Prow](/prow/more_prow.md)

[1]: https://github.com/settings/tokens
[2]: /prow/jobs.md#How-to-configure-new-jobs
[3]: https://github.com/jetstack/cert-manager
[4]: https://kubernetes.io/docs/concepts/services-networking/ingress/#tls
[5]: /prow/cmd/mkbuild-cluster/
[6]: /prow/cmd/tide/README.md
[7]: /prow/cmd/tide/config.md
[8]: https://github.com/kubernetes/test-infra/blob/master/prow/scaling.md#working-around-githubs-limited-acls
